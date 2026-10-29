/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kuberuntime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"
	cadvisorapi "github.com/google/cadvisor/lib/model"
	"github.com/stretchr/testify/require"
	noopoteltrace "go.opentelemetry.io/otel/trace/noop"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/flowcontrol"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	critesting "k8s.io/cri-api/pkg/apis/testing"
	"k8s.io/klog/v2"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/credentialprovider"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	containertest "k8s.io/kubernetes/pkg/kubelet/container/testing"
	"k8s.io/kubernetes/pkg/kubelet/images"
	imagepullmanager "k8s.io/kubernetes/pkg/kubelet/images/pullmanager"
	"k8s.io/kubernetes/pkg/kubelet/nodestatus"
	testingclock "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
)

const (
	benchMiB = 1024 * 1024

	benchPodCount             = 60
	benchContainersPerPod     = 3
	benchSharedPublicImages   = 28
	benchTenantPrivateImages  = 30
	benchImageVolumeImages    = 4
	benchPinnedImages         = 2
	benchUnusedBatches        = 5
	benchUnusedBatchSize      = 6
	benchUnusedRingTotal      = benchUnusedBatches * benchUnusedBatchSize // 30
	benchEvictableImageBytes  = 100 * benchMiB
	benchCapacityBytes        = 10000 * benchMiB
	benchSetupAvailableBytes  = 2500 * benchMiB // 75% used (< HighThresholdPercent 85%)
	benchGCWaveAvailableBytes = 1400 * benchMiB // 86% used -> frees 600 MiB (6 * 100 MiB) down to 80%
)

var (
	errBenchRegistry404  = errors.New("rpc error: code = NotFound desc = failed to pull and unpack image: not found")
	errBenchUnauthorized = errors.New("rpc error: code = Unauthenticated desc = unauthorized: authentication required")
	emptyCRIImageStatus  = &runtimeapi.ImageStatusResponse{}
)

type benchNoOpRecorder struct{}

func (benchNoOpRecorder) Event(runtime.Object, string, string, string)                  {}
func (benchNoOpRecorder) Eventf(runtime.Object, string, string, string, ...interface{}) {}
func (benchNoOpRecorder) AnnotatedEventf(runtime.Object, map[string]string, string, string, string, ...interface{}) {
}
func (r benchNoOpRecorder) WithLogger(klog.Logger) record.EventRecorderLogger { return r }

type benchStatsProvider struct {
	fsStats *statsapi.FsStats
}

func (p *benchStatsProvider) ImageFsStats(context.Context) (*statsapi.FsStats, *statsapi.FsStats, error) {
	return p.fsStats, p.fsStats, nil
}

type benchCatalogEntry struct {
	img              *runtimeapi.Image
	statusResp       *runtimeapi.ImageStatusResponse
	lookupRefs       []string
	expectedUsername string
	expectedPassword string
}

// benchCRIService implements both internalapi.RuntimeService and internalapi.ImageManagerService
// with pre-materialized CRI objects so benchmark measurements capture real Kubelet production
// allocations without artificial test-fake cloning noise.
type benchCRIService struct {
	*critesting.FakeRuntimeService
	*critesting.FakeImageService

	versionResp        *runtimeapi.VersionResponse
	fsInfoResp         *runtimeapi.ImageFsInfoResponse
	sandboxes          []*runtimeapi.PodSandbox
	containers         []*runtimeapi.Container
	runningContainers  []*runtimeapi.Container
	containerStatusMap map[string]*runtimeapi.ContainerStatusResponse

	catalogByID    map[string]*benchCatalogEntry
	pullCatalog    map[string]*benchCatalogEntry
	byRef          map[string]*runtimeapi.ImageStatusResponse
	presentByID    map[string]bool
	activeImages   []*runtimeapi.Image
	evictedInWave  []string
	brokenImageRef string
	pullCalls      int
}

func (s *benchCRIService) Version(context.Context, string) (*runtimeapi.VersionResponse, error) {
	return s.versionResp, nil
}

func (s *benchCRIService) Close(context.Context) error {
	return nil
}

func (s *benchCRIService) ListPodSandbox(context.Context, *runtimeapi.PodSandboxFilter) ([]*runtimeapi.PodSandbox, error) {
	return s.sandboxes, nil
}

func (s *benchCRIService) ListContainers(_ context.Context, filter *runtimeapi.ContainerFilter) ([]*runtimeapi.Container, error) {
	if filter != nil && filter.State != nil && filter.State.State == runtimeapi.ContainerState_CONTAINER_RUNNING {
		return s.runningContainers, nil
	}
	return s.containers, nil
}

func (s *benchCRIService) ContainerStatus(_ context.Context, containerID string, _ bool) (*runtimeapi.ContainerStatusResponse, error) {
	if resp, ok := s.containerStatusMap[containerID]; ok {
		return resp, nil
	}
	return nil, errors.New("container not found")
}

func (s *benchCRIService) ListImages(context.Context, *runtimeapi.ImageFilter) ([]*runtimeapi.Image, error) {
	return s.activeImages, nil
}

func (s *benchCRIService) ImageStatus(_ context.Context, spec *runtimeapi.ImageSpec, _ bool) (*runtimeapi.ImageStatusResponse, error) {
	if resp, ok := s.byRef[spec.GetImage()]; ok {
		return resp, nil
	}
	return emptyCRIImageStatus, nil
}

func (s *benchCRIService) PullImage(_ context.Context, spec *runtimeapi.ImageSpec, auth *runtimeapi.AuthConfig, _ *runtimeapi.PodSandboxConfig) (string, error) {
	ref := spec.GetImage()
	s.pullCalls++
	if ref == s.brokenImageRef {
		return "", errBenchRegistry404
	}
	entry, ok := s.pullCatalog[ref]
	if !ok {
		return "", errBenchRegistry404
	}
	if entry.expectedUsername != "" {
		if auth == nil || auth.Username != entry.expectedUsername || auth.Password != entry.expectedPassword {
			return "", errBenchUnauthorized
		}
	}
	s.stageEntry(entry)
	return entry.img.Id, nil
}

func (s *benchCRIService) RemoveImage(_ context.Context, spec *runtimeapi.ImageSpec) error {
	id := spec.GetImage()
	entry, ok := s.catalogByID[id]
	if !ok || !s.presentByID[id] {
		return nil
	}
	s.presentByID[id] = false
	for _, r := range entry.lookupRefs {
		delete(s.byRef, r)
	}
	for i, img := range s.activeImages {
		if img.Id == id {
			copy(s.activeImages[i:], s.activeImages[i+1:])
			s.activeImages[len(s.activeImages)-1] = nil
			s.activeImages = s.activeImages[:len(s.activeImages)-1]
			break
		}
	}
	s.evictedInWave = append(s.evictedInWave, id)
	return nil
}

func (s *benchCRIService) ImageFsInfo(context.Context) (*runtimeapi.ImageFsInfoResponse, error) {
	return s.fsInfoResp, nil
}

func (s *benchCRIService) stageEntry(entry *benchCatalogEntry) {
	if s.presentByID[entry.img.Id] {
		return
	}
	s.presentByID[entry.img.Id] = true
	for _, r := range entry.lookupRefs {
		s.byRef[r] = entry.statusResp
	}
	s.activeImages = append(s.activeImages, entry.img)
}

type benchEnsureTask struct {
	objRef         *v1.ObjectReference
	pod            *v1.Pod
	requestedImage string
	pullSecrets    []v1.Secret
	sandboxCfg     *runtimeapi.PodSandboxConfig
	pullPolicy     v1.PullPolicy
	wantImageRef   string
	wantErr        error
}

type imagePoolingEnv struct {
	ctx                context.Context
	cri                *benchCRIService
	statsProvider      *benchStatsProvider
	runtimeManager     *kubeGenericRuntimeManager
	imageManager       images.ImageManager
	imageGCManager     images.ImageGCManager
	pullManager        *imagepullmanager.PullManager
	nodeImagesSetter   nodestatus.Setter
	nodeTemplate       v1.Node
	beganGC            time.Time
	steadyEnsureTasks  []benchEnsureTask
	alwaysPullTasks    []benchEnsureTask
	neverAndErrorTasks []benchEnsureTask
	unusedEntries      [benchUnusedRingTotal]*benchCatalogEntry
	unusedPullTasks    [benchUnusedBatches][2]benchEnsureTask
}

func makeDockerConfigJSONSecret(namespace, name, uid, registry, username, password string) v1.Secret {
	authToken := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	payload, err := json.Marshal(map[string]any{
		"auths": map[string]any{
			registry: map[string]string{
				"username": username,
				"password": password,
				"auth":     authToken,
			},
		},
	})
	if err != nil {
		panic(err)
	}
	return v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(uid),
		},
		Type: v1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			v1.DockerConfigJsonKey: payload,
		},
	}
}

func buildCatalogEntry(idHex, repo, tag string, size int64, pinned bool, annotations map[string]string, user, pass string, includeTagless bool) (*benchCatalogEntry, string) {
	imageID := "sha256:" + idHex
	digestRef := fmt.Sprintf("%s@sha256:%s", repo, idHex)
	tagRef := fmt.Sprintf("%s:%s", repo, tag)
	altTagRef := fmt.Sprintf("%s:%s-linux-amd64", repo, tag)

	criImg := &runtimeapi.Image{
		Id:          imageID,
		RepoTags:    []string{tagRef, altTagRef},
		RepoDigests: []string{digestRef, fmt.Sprintf("%s@sha256:%s", repo, idHex[:56]+"ffffffff")},
		Size:        uint64(size),
		Pinned:      pinned,
		Spec: &runtimeapi.ImageSpec{
			Image:       tagRef,
			Annotations: annotations,
		},
	}
	refs := []string{imageID, tagRef, altTagRef, digestRef}
	if includeTagless && tag == "latest" {
		refs = append(refs, repo)
	}
	entry := &benchCatalogEntry{
		img:              criImg,
		statusResp:       &runtimeapi.ImageStatusResponse{Image: criImg},
		lookupRefs:       refs,
		expectedUsername: user,
		expectedPassword: pass,
	}
	return entry, digestRef
}

func newImagePoolingEnv(tb testing.TB) *imagePoolingEnv {
	tb.Helper()
	ctx := klog.NewContext(context.Background(), logr.Discard())
	logger := klog.FromContext(ctx)

	cri := &benchCRIService{
		FakeRuntimeService: critesting.NewFakeRuntimeService(),
		FakeImageService:   critesting.NewFakeImageService(),
		versionResp: &runtimeapi.VersionResponse{
			Version:           kubeRuntimeAPIVersion,
			RuntimeName:       "containerd",
			RuntimeVersion:    "2.0.0",
			RuntimeApiVersion: "v1",
		},
		fsInfoResp: &runtimeapi.ImageFsInfoResponse{
			ImageFilesystems: []*runtimeapi.FilesystemUsage{
				{UsedBytes: &runtimeapi.UInt64Value{Value: 8600 * benchMiB}},
			},
		},
		containerStatusMap: make(map[string]*runtimeapi.ContainerStatusResponse, benchPodCount*benchContainersPerPod+16),
		catalogByID:        make(map[string]*benchCatalogEntry, 128),
		pullCatalog:        make(map[string]*benchCatalogEntry, 512),
		byRef:              make(map[string]*runtimeapi.ImageStatusResponse, 512),
		presentByID:        make(map[string]bool, 128),
		activeImages:       make([]*runtimeapi.Image, 0, 128),
		evictedInWave:      make([]string, 0, 16),
		brokenImageRef:     "registry.k8s.io/broken/nonexistent:v0.0.1",
	}

	registerCatalog := func(e *benchCatalogEntry) {
		cri.catalogByID[e.img.Id] = e
		for _, r := range e.lookupRefs {
			cri.pullCatalog[r] = e
		}
	}

	defaultImgAnnotations := map[string]string{
		"io.cri-containerd.image": "managed",
		"org.opencontainers.image.vendor": "kubernetes-bench",
	}

	// 1. Pinned system/sandbox images (2)
	for i := range benchPinnedImages {
		idHex := fmt.Sprintf("10000000000000000000000000000000000000000000000000000000000000%02x", i)
		repo := fmt.Sprintf("registry.k8s.io/system/pinned-%d", i)
		entry, _ := buildCatalogEntry(idHex, repo, "v1.0", 40*benchMiB, true, defaultImgAnnotations, "", "", false)
		registerCatalog(entry)
		cri.stageEntry(entry)
	}

	// 2. Shared public/node-accessible images (28)
	publicEntries := make([]*benchCatalogEntry, benchSharedPublicImages)
	publicDigestRefs := make([]string, benchSharedPublicImages)
	for i := range benchSharedPublicImages {
		idHex := fmt.Sprintf("20000000000000000000000000000000000000000000000000000000000000%02x", i)
		repo := fmt.Sprintf("registry.k8s.io/workloads/shared-service-%02d", i)
		tag := fmt.Sprintf("v1.%d.0", i)
		includeTagless := i%7 == 0
		if includeTagless {
			tag = "latest"
		}
		entry, digestRef := buildCatalogEntry(idHex, repo, tag, int64(45+i)*benchMiB, false, defaultImgAnnotations, "", "", includeTagless)
		registerCatalog(entry)
		cri.stageEntry(entry)
		publicEntries[i] = entry
		publicDigestRefs[i] = digestRef
	}

	// 3. Private tenant images authenticated with per-tenant pull secrets (30)
	tenantEntries := make([]*benchCatalogEntry, benchTenantPrivateImages)
	tenantSecrets := make([][]v1.Secret, benchTenantPrivateImages)
	for i := range benchTenantPrivateImages {
		idHex := fmt.Sprintf("30000000000000000000000000000000000000000000000000000000000000%02x", i)
		ns := fmt.Sprintf("tenant-ns-%02d", i%15)
		registry := fmt.Sprintf("us-central1-docker.pkg.dev/tenant-proj-%02d", i%15)
		repo := fmt.Sprintf("%s/apps/backend-%02d", registry, i)
		user := fmt.Sprintf("oauth2accesstoken-%02d", i%15)
		pass := fmt.Sprintf("secret-token-value-%02d", i%15)
		entry, _ := buildCatalogEntry(idHex, repo, "v2.4.1", int64(60+i)*benchMiB, false, defaultImgAnnotations, user, pass, false)
		registerCatalog(entry)
		cri.stageEntry(entry)
		tenantEntries[i] = entry

		primarySecret := makeDockerConfigJSONSecret(
			ns,
			fmt.Sprintf("regcred-primary-%02d", i%15),
			fmt.Sprintf("sec-uid-primary-%02d", i%15),
			registry,
			user,
			pass,
		)
		secondarySecret := makeDockerConfigJSONSecret(
			ns,
			fmt.Sprintf("regcred-mirror-%02d", i%15),
			fmt.Sprintf("sec-uid-mirror-%02d", i%15),
			"mirror.gcr.io/unused",
			"mirror-user",
			"mirror-pass",
		)
		tenantSecrets[i] = []v1.Secret{primarySecret, secondarySecret}
	}

	// 4. OCI ImageVolume images mounted into running containers (4)
	volumeEntries := make([]*benchCatalogEntry, benchImageVolumeImages)
	for i := range benchImageVolumeImages {
		idHex := fmt.Sprintf("40000000000000000000000000000000000000000000000000000000000000%02x", i)
		repo := fmt.Sprintf("us-docker.pkg.dev/ml-models/weights/artifact-%02d", i)
		entry, _ := buildCatalogEntry(idHex, repo, "v1", int64(80+i)*benchMiB, false, defaultImgAnnotations, "", "", false)
		registerCatalog(entry)
		cri.stageEntry(entry)
		volumeEntries[i] = entry
	}

	// 5. Rotating 5-batch * 6-image unused eviction ring (30 images, 100 MiB each)
	var unusedEntries [benchUnusedRingTotal]*benchCatalogEntry
	unusedSecrets := make([]v1.Secret, 1)
	unusedPrivRegistry := "europe-west1-docker.pkg.dev/batch-jobs"
	unusedPrivUser := "batch-puller"
	unusedPrivPass := "batch-secret-key"
	unusedSecrets[0] = makeDockerConfigJSONSecret("batch-ns", "batch-regcred", "sec-uid-batch", unusedPrivRegistry, unusedPrivUser, unusedPrivPass)

	for i := range benchUnusedRingTotal {
		idHex := fmt.Sprintf("50000000000000000000000000000000000000000000000000000000000000%02x", i)
		slotInBatch := i % benchUnusedBatchSize
		var entry *benchCatalogEntry
		if slotInBatch == 1 {
			repo := fmt.Sprintf("%s/etl/worker-%02d", unusedPrivRegistry, i)
			entry, _ = buildCatalogEntry(idHex, repo, "v1.0.0", benchEvictableImageBytes, false, defaultImgAnnotations, unusedPrivUser, unusedPrivPass, false)
		} else {
			repo := fmt.Sprintf("registry.k8s.io/batch/ephemeral-job-%02d", i)
			entry, _ = buildCatalogEntry(idHex, repo, "v1.0.0", benchEvictableImageBytes, false, defaultImgAnnotations, "", "", false)
		}
		registerCatalog(entry)
		unusedEntries[i] = entry
	}

	// Initialize kubeGenericRuntimeManager, PullManager, ImageManager, and ImageGCManager.
	noopRecorder := benchNoOpRecorder{}
	filteredRecorder := kubecontainer.FilterEventRecorder(noopRecorder)
	runtimeManager, err := newFakeKubeRuntimeManager(
		ctx,
		cri,
		cri,
		&cadvisorapi.MachineInfo{NumCores: 16, MemoryCapacity: 64 * 1024 * benchMiB},
		&containertest.FakeOS{},
		&containertest.FakeRuntimeHelper{},
		noopoteltrace.NewTracerProvider().Tracer("bench"),
		nil,
	)
	require.NoError(tb, err)
	runtimeManager.recorder = noopRecorder

	fsAccessor, err := imagepullmanager.NewFSPullRecordsAccessor(logger, tb.TempDir())
	require.NoError(tb, err)
	memCacheAccessor := imagepullmanager.NewCachedPullRecordsAccessor(logger, fsAccessor, 300, 600, 16)
	pullManager, err := imagepullmanager.NewImagePullManager(
		ctx,
		memCacheAccessor,
		imagepullmanager.AlwaysVerifyImagePullPolicy(),
		runtimeManager,
		16,
	)
	require.NoError(tb, err)

	baseTime := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	fakeClock := testingclock.NewFakeClock(baseTime)
	backOff := flowcontrol.NewBackOff(10*time.Second, 300*time.Second)
	backOff.Clock = fakeClock

	imgManager := images.NewImageManager(
		filteredRecorder,
		&credentialprovider.BasicDockerKeyring{},
		runtimeManager,
		pullManager,
		backOff,
		false,
		ptr.To[int32](0),
		0,
		0,
		&fakePodPullingTimeRecorder{},
	)
	runtimeManager.imagePuller = imgManager

	statsProvider := &benchStatsProvider{
		fsStats: &statsapi.FsStats{
			CapacityBytes:  ptr.To(uint64(benchCapacityBytes)),
			AvailableBytes: ptr.To(uint64(benchSetupAvailableBytes)),
		},
	}
	nodeRef := &v1.ObjectReference{Kind: "Node", Name: "bench-node-01", UID: "node-uid-01"}
	gcPolicy := images.ImageGCPolicy{
		HighThresholdPercent: 85,
		LowThresholdPercent:  80,
		MinAge:               time.Nanosecond,
	}
	imgGCManager, err := images.NewImageGCManager(
		runtimeManager,
		statsProvider,
		[]images.PostImageGCHook{pullManager.PruneUnknownRecords},
		noopRecorder,
		nodeRef,
		gcPolicy,
		noopoteltrace.NewTracerProvider(),
	)
	require.NoError(tb, err)

	// Record initial pulled records for all pre-populated public & private images by pulling once via PullManager.
	for _, entry := range publicEntries {
		pullManager.RecordImagePulled(ctx, entry.img.RepoTags[0], entry.img.Id, &kubeletconfiginternal.ImagePullCredentials{
			NodePodsAccessible: true,
		})
	}
	for i, entry := range tenantEntries {
		sec := tenantSecrets[i][0]
		var dc credentialprovider.DockerConfigJSON
		require.NoError(tb, json.Unmarshal(sec.Data[v1.DockerConfigJsonKey], &dc))
		var hash string
		for _, authEntry := range dc.Auths {
			tracked := credentialprovider.NewTrackedAuthConfig(&credentialprovider.AuthConfig{
				Username: authEntry.Username,
				Password: authEntry.Password,
			}, &credentialprovider.CredentialSource{
				Secret: &credentialprovider.SecretCoordinates{
					UID:       string(sec.UID),
					Namespace: sec.Namespace,
					Name:      sec.Name,
				},
			})
			hash = tracked.AuthConfigHash
		}
		pullManager.RecordImagePulled(ctx, entry.img.RepoTags[0], entry.img.Id, &kubeletconfiginternal.ImagePullCredentials{
			KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{
				{
					UID:            string(sec.UID),
					Namespace:      sec.Namespace,
					Name:           sec.Name,
					CredentialHash: hash,
				},
			},
		})
	}

	// Build 60 pods * 3 containers (180 running containers + 12 exited historical containers)
	// and corresponding EnsureImageExists tasks.
	steadyTasks := make([]benchEnsureTask, 0, benchPodCount*benchContainersPerPod)
	alwaysTasks := make([]benchEnsureTask, 0, 4)

	for p := range benchPodCount {
		ns := fmt.Sprintf("tenant-ns-%02d", p%15)
		podName := fmt.Sprintf("workload-pod-%02d", p)
		podUID := types.UID(fmt.Sprintf("pod-uid-%04d", p))
		sandboxID := fmt.Sprintf("sandbox-%04d", p)
		createdAtNs := baseTime.Add(time.Duration(p) * time.Second).UnixNano()

		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podName,
				Namespace: ns,
				UID:       podUID,
				Annotations: map[string]string{
					"prometheus.io/scrape":               "true",
					"prometheus.io/port":                 "9090",
					"kubectl.kubernetes.io/default-logs": "app-main",
				},
			},
			Spec: v1.PodSpec{
				ServiceAccountName: "default",
				Containers:         make([]v1.Container, benchContainersPerPod),
			},
		}
		sandboxCfg := &runtimeapi.PodSandboxConfig{
			Metadata: &runtimeapi.PodSandboxMetadata{
				Name:      podName,
				Namespace: ns,
				Uid:       string(podUID),
			},
		}
		cri.sandboxes = append(cri.sandboxes, &runtimeapi.PodSandbox{
			Id:        sandboxID,
			Metadata:  sandboxCfg.Metadata,
			State:     runtimeapi.PodSandboxState_SANDBOX_READY,
			CreatedAt: createdAtNs,
			Labels:    newPodLabels(pod),
		})

		for c := range benchContainersPerPod {
			cName := fmt.Sprintf("container-%d", c)
			cID := fmt.Sprintf("cid-%02d-%d", p, c)
			var (
				reqImage   string
				entry      *benchCatalogEntry
				secrets    []v1.Secret
				pullPolicy = v1.PullIfNotPresent
			)

			switch c {
			case 0:
				// Sidecar container sharing public images (tagless, :tag, or @sha256:digest)
				pubIdx := p % benchSharedPublicImages
				entry = publicEntries[pubIdx]
				switch {
				case pubIdx%7 == 0:
					// Tagless image string -> exercises applyDefaultImageTag(":latest")
					reqImage = entry.lookupRefs[len(entry.lookupRefs)-1]
				case p%4 == 0:
					reqImage = publicDigestRefs[pubIdx]
				default:
					reqImage = entry.img.RepoTags[0]
				}
			case 1:
				// Primary app container: 30 pods use private secret-pulled images, 30 use public images
				if p < benchTenantPrivateImages {
					entry = tenantEntries[p]
					reqImage = entry.img.RepoTags[0]
					secrets = tenantSecrets[p]
				} else {
					pubIdx := (p + 5) % benchSharedPublicImages
					entry = publicEntries[pubIdx]
					reqImage = entry.img.RepoTags[0]
				}
			default:
				// Secondary container: 30 pods also verify private secret-pulled images, 30 use public images
				if p >= benchTenantPrivateImages {
					privIdx := p - benchTenantPrivateImages
					entry = tenantEntries[privIdx]
					reqImage = entry.img.RepoTags[0]
					secrets = tenantSecrets[privIdx]
				} else {
					pubIdx := (p + 11) % benchSharedPublicImages
					entry = publicEntries[pubIdx]
					reqImage = entry.img.RepoTags[0]
				}
			}

			v1Container := v1.Container{
				Name:                     cName,
				Image:                    reqImage,
				ImagePullPolicy:          pullPolicy,
				TerminationMessagePath:   v1.TerminationMessagePathDefault,
				TerminationMessagePolicy: v1.TerminationMessageReadFile,
				Ports: []v1.ContainerPort{
					{Name: "http", ContainerPort: 8080, Protocol: v1.ProtocolTCP},
				},
			}
			pod.Spec.Containers[c] = v1Container
			objRef := &v1.ObjectReference{
				Kind:      "Pod",
				Namespace: ns,
				Name:      podName,
				UID:       podUID,
				FieldPath: fmt.Sprintf("spec.containers{%s}", cName),
			}

			// Select 4 containers (2 public, 2 private) to exercise PullAlways refreshes
			if (p == 0 && c == 0) || (p == 7 && c == 0) || (p == 1 && c == 1) || (p == 2 && c == 1) {
				alwaysTasks = append(alwaysTasks, benchEnsureTask{
					objRef:         objRef,
					pod:            pod,
					requestedImage: reqImage,
					pullSecrets:    secrets,
					sandboxCfg:     sandboxCfg,
					pullPolicy:     v1.PullAlways,
					wantImageRef:   entry.img.Id,
				})
			} else {
				steadyTasks = append(steadyTasks, benchEnsureTask{
					objRef:         objRef,
					pod:            pod,
					requestedImage: reqImage,
					pullSecrets:    secrets,
					sandboxCfg:     sandboxCfg,
					pullPolicy:     v1.PullIfNotPresent,
					wantImageRef:   entry.img.Id,
				})
			}

			cLabels := newContainerLabels(&v1Container, pod)
			cAnnotations := newContainerAnnotations(ctx, &v1Container, pod, 0, &kubecontainer.RunContainerOptions{})
			criContainer := &runtimeapi.Container{
				Id:           cID,
				PodSandboxId: sandboxID,
				Metadata:     &runtimeapi.ContainerMetadata{Name: cName},
				Image:        &runtimeapi.ImageSpec{Image: reqImage},
				ImageRef:     entry.img.RepoDigests[0],
				ImageId:      entry.img.Id,
				State:        runtimeapi.ContainerState_CONTAINER_RUNNING,
				CreatedAt:    createdAtNs,
				Labels:       cLabels,
				Annotations:  cAnnotations,
			}
			cri.containers = append(cri.containers, criContainer)
			cri.runningContainers = append(cri.runningContainers, criContainer)

			mounts := []*runtimeapi.Mount{
				{ContainerPath: "/var/run/secrets/kubernetes.io/serviceaccount", HostPath: "/var/lib/kubelet/pods/sa", Readonly: true},
				{ContainerPath: "/etc/config", HostPath: "/var/lib/kubelet/pods/cfg", Readonly: true},
				{ContainerPath: "/tmp", HostPath: "/var/lib/kubelet/pods/tmp", Readonly: false},
			}
			// Mount OCI ImageVolumes on 8 containers across the node
			if c == 1 && p < 8 {
				volEntry := volumeEntries[p%benchImageVolumeImages]
				mounts = append(mounts, &runtimeapi.Mount{
					ContainerPath: "/mnt/model-weights",
					HostPath:      "/var/lib/containerd/image-volumes/vol",
					Readonly:      true,
					Image:         &runtimeapi.ImageSpec{Image: volEntry.img.Id},
				})
			}

			cri.containerStatusMap[cID] = &runtimeapi.ContainerStatusResponse{
				Status: &runtimeapi.ContainerStatus{
					Id:          cID,
					Metadata:    &runtimeapi.ContainerMetadata{Name: cName},
					State:       runtimeapi.ContainerState_CONTAINER_RUNNING,
					CreatedAt:   createdAtNs,
					StartedAt:   createdAtNs + int64(time.Second),
					Image:       &runtimeapi.ImageSpec{Image: reqImage},
					ImageRef:    entry.img.RepoDigests[0],
					ImageId:     entry.img.Id,
					Labels:      cLabels,
					Annotations: cAnnotations,
					Mounts:      mounts,
				},
			}
		}

		// Add 12 exited historical containers across the first 12 pods for realistic GetPods(ctx, true) load
		if p < 12 {
			exitedID := fmt.Sprintf("cid-exited-%02d", p)
			pubEntry := publicEntries[p%benchSharedPublicImages]
			exitedSpec := pod.Spec.Containers[0]
			cLabels := newContainerLabels(&exitedSpec, pod)
			cAnnotations := newContainerAnnotations(ctx, &exitedSpec, pod, 1, &kubecontainer.RunContainerOptions{})
			exitedContainer := &runtimeapi.Container{
				Id:           exitedID,
				PodSandboxId: sandboxID,
				Metadata:     &runtimeapi.ContainerMetadata{Name: exitedSpec.Name, Attempt: 0},
				Image:        &runtimeapi.ImageSpec{Image: pubEntry.img.RepoTags[0]},
				ImageRef:     pubEntry.img.RepoDigests[0],
				ImageId:      pubEntry.img.Id,
				State:        runtimeapi.ContainerState_CONTAINER_EXITED,
				CreatedAt:    createdAtNs - int64(10*time.Minute),
				Labels:       cLabels,
				Annotations:  cAnnotations,
			}
			cri.containers = append(cri.containers, exitedContainer)
			cri.containerStatusMap[exitedID] = &runtimeapi.ContainerStatusResponse{
				Status: &runtimeapi.ContainerStatus{
					Id:          exitedID,
					Metadata:    &runtimeapi.ContainerMetadata{Name: exitedSpec.Name, Attempt: 0},
					State:       runtimeapi.ContainerState_CONTAINER_EXITED,
					CreatedAt:   createdAtNs - int64(10*time.Minute),
					StartedAt:   createdAtNs - int64(9*time.Minute),
					FinishedAt:  createdAtNs - int64(time.Minute),
					ExitCode:    0,
					Reason:      "Completed",
					Image:       &runtimeapi.ImageSpec{Image: pubEntry.img.RepoTags[0]},
					ImageRef:    pubEntry.img.RepoDigests[0],
					ImageId:     pubEntry.img.Id,
					Labels:      cLabels,
					Annotations: cAnnotations,
				},
			}
		}
	}

	// Build PullNever & BackOff tasks (4 checks per wave)
	unauthorizedPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "unauth-pod", Namespace: "untrusted-ns", UID: "pod-uid-unauth"},
		Spec:       v1.PodSpec{ServiceAccountName: "default"},
	}
	unauthorizedSandbox := &runtimeapi.PodSandboxConfig{
		Metadata: &runtimeapi.PodSandboxMetadata{Name: "unauth-pod", Namespace: "untrusted-ns", Uid: "pod-uid-unauth"},
	}
	neverAndErrorTasks := []benchEnsureTask{
		{
			// Missing image with PullNever -> ErrImageNeverPull
			objRef:         &v1.ObjectReference{Kind: "Pod", Namespace: "untrusted-ns", Name: "unauth-pod", UID: "pod-uid-unauth"},
			pod:            unauthorizedPod,
			requestedImage: "registry.k8s.io/nonexistent/never-image:v1",
			sandboxCfg:     unauthorizedSandbox,
			pullPolicy:     v1.PullNever,
			wantErr:        images.ErrImageNeverPull,
		},
		{
			// Secret-pulled image present locally, but pod lacks valid pullSecrets with PullNever -> ErrImageNeverPull
			objRef:         &v1.ObjectReference{Kind: "Pod", Namespace: "untrusted-ns", Name: "unauth-pod", UID: "pod-uid-unauth"},
			pod:            unauthorizedPod,
			requestedImage: tenantEntries[0].img.RepoTags[0],
			sandboxCfg:     unauthorizedSandbox,
			pullPolicy:     v1.PullNever,
			wantErr:        images.ErrImageNeverPull,
		},
	}
	for bIdx := range 2 {
		bPod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("backoff-pod-%d", bIdx),
				Namespace: "default",
				UID:       types.UID(fmt.Sprintf("pod-uid-backoff-%d", bIdx)),
			},
			Spec: v1.PodSpec{ServiceAccountName: "default"},
		}
		bRef := &v1.ObjectReference{Kind: "Pod", Namespace: "default", Name: bPod.Name, UID: bPod.UID}
		bSandbox := &runtimeapi.PodSandboxConfig{
			Metadata: &runtimeapi.PodSandboxMetadata{Name: bPod.Name, Namespace: "default", Uid: string(bPod.UID)},
		}
		// Seed initial failed pull so subsequent waves hit deterministic ImagePullBackOff
		_, _, err := imgManager.EnsureImageExists(ctx, bRef, bPod, cri.brokenImageRef, nil, bSandbox, "", v1.PullIfNotPresent)
		require.ErrorIs(tb, err, images.ErrImagePull)
		neverAndErrorTasks = append(neverAndErrorTasks, benchEnsureTask{
			objRef:         bRef,
			pod:            bPod,
			requestedImage: cri.brokenImageRef,
			sandboxCfg:     bSandbox,
			pullPolicy:     v1.PullIfNotPresent,
			wantErr:        images.ErrImagePullBackOff,
		})
	}
	fakeClock.SetTime(baseTime.Add(100 * time.Millisecond))

	// Build rotating cache-miss pull tasks for slots 0 & 1 of each of the 5 unused batches
	var unusedPullTasks [benchUnusedBatches][2]benchEnsureTask
	batchPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "batch-launcher",
			Namespace: "batch-ns",
			UID:       "pod-uid-batch-launcher",
			Annotations: map[string]string{
				"batch.kubernetes.io/job-name": "etl-runner",
			},
		},
		Spec: v1.PodSpec{ServiceAccountName: "default"},
	}
	batchObjRef := &v1.ObjectReference{Kind: "Pod", Namespace: "batch-ns", Name: "batch-launcher", UID: "pod-uid-batch-launcher"}
	batchSandbox := &runtimeapi.PodSandboxConfig{
		Metadata: &runtimeapi.PodSandboxMetadata{Name: "batch-launcher", Namespace: "batch-ns", Uid: "pod-uid-batch-launcher"},
	}
	for b := range benchUnusedBatches {
		pubSlot := b*benchUnusedBatchSize + 0
		privSlot := b*benchUnusedBatchSize + 1
		unusedPullTasks[b][0] = benchEnsureTask{
			objRef:         batchObjRef,
			pod:            batchPod,
			requestedImage: unusedEntries[pubSlot].img.RepoTags[0],
			sandboxCfg:     batchSandbox,
			pullPolicy:     v1.PullIfNotPresent,
			wantImageRef:   unusedEntries[pubSlot].img.Id,
		}
		unusedPullTasks[b][1] = benchEnsureTask{
			objRef:         batchObjRef,
			pod:            batchPod,
			requestedImage: unusedEntries[privSlot].img.RepoTags[0],
			pullSecrets:    unusedSecrets,
			sandboxCfg:     batchSandbox,
			pullPolicy:     v1.PullIfNotPresent,
			wantImageRef:   unusedEntries[privSlot].img.Id,
		}
	}

	// Seed initial 4 unused batches (batches 0..3 = 24 unused images) with strictly ordered firstDetected timestamps.
	beganGC := baseTime.Add(-10 * time.Minute)
	for b := range benchUnusedBatches - 1 {
		for s := range 2 {
			task := unusedPullTasks[b][s]
			gotRef, _, pullErr := imgManager.EnsureImageExists(ctx, task.objRef, task.pod, task.requestedImage, task.pullSecrets, task.sandboxCfg, "", task.pullPolicy)
			require.NoError(tb, pullErr)
			require.Equal(tb, task.wantImageRef, gotRef)
		}
		for s := 2; s < benchUnusedBatchSize; s++ {
			cri.stageEntry(unusedEntries[b*benchUnusedBatchSize+s])
		}
		require.NoError(tb, imgGCManager.GarbageCollect(ctx, beganGC))
		time.Sleep(50 * time.Microsecond)
	}

	// Warm imageGCManager.imageCache once via Start(startCtx) and cancel immediately once populated.
	startCtx, startCancel := context.WithCancel(ctx)
	imgGCManager.Start(startCtx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cachedImgs, listErr := imgGCManager.GetImageList(ctx)
		if listErr == nil && len(cachedImgs) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	startCancel()

	// Switch filesystem availability to 86% used so each wave's GarbageCollect frees 600 MiB (6 unused images).
	statsProvider.fsStats.AvailableBytes = ptr.To(uint64(benchGCWaveAvailableBytes))

	return &imagePoolingEnv{
		ctx:                ctx,
		cri:                cri,
		statsProvider:      statsProvider,
		runtimeManager:     runtimeManager,
		imageManager:       imgManager,
		imageGCManager:     imgGCManager,
		pullManager:        pullManager,
		nodeImagesSetter:   nodestatus.Images(50, imgGCManager.GetImageList),
		nodeTemplate:       v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "bench-node-01"}},
		beganGC:            beganGC,
		steadyEnsureTasks:  steadyTasks,
		alwaysPullTasks:    alwaysTasks,
		neverAndErrorTasks: neverAndErrorTasks,
		unusedEntries:      unusedEntries,
		unusedPullTasks:    unusedPullTasks,
	}
}

func (env *imagePoolingEnv) runWave(wave int) error {
	incomingBatch := (wave + benchUnusedBatches - 1) % benchUnusedBatches
	baseSlot := incomingBatch * benchUnusedBatchSize
	env.cri.evictedInWave = env.cri.evictedInWave[:0]

	// 1. Reconcile 176 steady-state IfNotPresent container specs across 60 pods (public + secret-verified).
	for i := range env.steadyEnsureTasks {
		t := &env.steadyEnsureTasks[i]
		gotRef, _, err := env.imageManager.EnsureImageExists(env.ctx, t.objRef, t.pod, t.requestedImage, t.pullSecrets, t.sandboxCfg, "", t.pullPolicy)
		if err != nil || gotRef != t.wantImageRef {
			return fmt.Errorf("steady task %d (%s): got (%q, %v), want (%q, nil)", i, t.requestedImage, gotRef, err, t.wantImageRef)
		}
	}

	// 2. Execute 4 PullAlways refreshes + 2 PullIfNotPresent cache misses (incoming unused batch slots 0 & 1)
	// and stage remaining 4 incoming unused images into CRI.
	pullsBeforeStep2 := env.cri.pullCalls
	for i := range env.alwaysPullTasks {
		t := &env.alwaysPullTasks[i]
		gotRef, _, err := env.imageManager.EnsureImageExists(env.ctx, t.objRef, t.pod, t.requestedImage, t.pullSecrets, t.sandboxCfg, "", t.pullPolicy)
		if err != nil || gotRef != t.wantImageRef {
			return fmt.Errorf("always task %d: got (%q, %v), want (%q, nil)", i, gotRef, err, t.wantImageRef)
		}
	}
	for s := range 2 {
		t := &env.unusedPullTasks[incomingBatch][s]
		gotRef, _, err := env.imageManager.EnsureImageExists(env.ctx, t.objRef, t.pod, t.requestedImage, t.pullSecrets, t.sandboxCfg, "", t.pullPolicy)
		if err != nil || gotRef != t.wantImageRef {
			return fmt.Errorf("cache-miss pull %d: got (%q, %v), want (%q, nil)", s, gotRef, err, t.wantImageRef)
		}
	}
	if gotStep2Pulls := env.cri.pullCalls - pullsBeforeStep2; gotStep2Pulls != 6 {
		return fmt.Errorf("expected 6 CRI PullImage calls for PullAlways + cache misses, got %d", gotStep2Pulls)
	}
	for s := 2; s < benchUnusedBatchSize; s++ {
		env.cri.stageEntry(env.unusedEntries[baseSlot+s])
	}

	// 3. Verify PullNever (missing + unauthorized secret image) and ImagePullBackOff fast-rejections.
	pullsBeforeStep3 := env.cri.pullCalls
	for i := range env.neverAndErrorTasks {
		t := &env.neverAndErrorTasks[i]
		_, _, err := env.imageManager.EnsureImageExists(env.ctx, t.objRef, t.pod, t.requestedImage, t.pullSecrets, t.sandboxCfg, "", t.pullPolicy)
		if !errors.Is(err, t.wantErr) {
			return fmt.Errorf("policy/error task %d: got err %v, want %v", i, err, t.wantErr)
		}
	}
	if gotStep3Pulls := env.cri.pullCalls - pullsBeforeStep3; gotStep3Pulls != 0 {
		return fmt.Errorf("expected 0 CRI PullImage calls for PullNever + BackOff rejections, got %d", gotStep3Pulls)
	}

	// 4. Periodic runtime image listing, ImageStats, and NodeStatus image reporting.
	listed, err := env.runtimeManager.ListImages(env.ctx)
	if err != nil || len(listed) != 94 {
		return fmt.Errorf("ListImages: got len=%d err=%v, want len=94", len(listed), err)
	}
	stats, err := env.runtimeManager.ImageStats(env.ctx)
	if err != nil || stats == nil || stats.TotalStorageBytes == 0 {
		return fmt.Errorf("ImageStats failed: stats=%v err=%v", stats, err)
	}
	node := env.nodeTemplate
	if err := env.nodeImagesSetter(env.ctx, &node); err != nil {
		return fmt.Errorf("nodestatus.Images setter failed: %w", err)
	}
	if len(node.Status.Images) != 50 {
		return fmt.Errorf("nodestatus.Images: got %d images, want 50", len(node.Status.Images))
	}

	// 5. Periodic ImageGCManager.GarbageCollect + PullManager.PruneUnknownRecords hook.
	if err := env.imageGCManager.GarbageCollect(env.ctx, env.beganGC); err != nil {
		return fmt.Errorf("GarbageCollect failed: %w", err)
	}
	if len(env.cri.evictedInWave) != benchUnusedBatchSize {
		return fmt.Errorf("expected %d evicted unused images, got %d (%v)", benchUnusedBatchSize, len(env.cri.evictedInWave), env.cri.evictedInWave)
	}

	return nil
}

func BenchmarkImagePoolingAndManagement(b *testing.B) {
	env := newImagePoolingEnv(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if err := env.runWave(i); err != nil {
			b.Fatal(err)
		}
	}
}

func TestImagePoolingAndManagementEquivalence(t *testing.T) {
	env := newImagePoolingEnv(t)

	// Execute 12 full waves (> 2 complete 5-batch ring rotations) and verify exact FIFO batch eviction,
	// pinned/in-use/ImageVolume protection, secret verification, and NodeStatus sorting invariants.
	for wave := range 12 {
		require.NoError(t, env.runWave(wave), "wave %d failed", wave)

		expectedBatch := wave % benchUnusedBatches
		wantEvicted := make([]string, benchUnusedBatchSize)
		for s := range benchUnusedBatchSize {
			wantEvicted[s] = env.unusedEntries[expectedBatch*benchUnusedBatchSize+s].img.Id
		}
		gotEvicted := slices.Clone(env.cri.evictedInWave)
		slices.Sort(gotEvicted)
		slices.Sort(wantEvicted)
		require.Equal(t, wantEvicted, gotEvicted, "wave %d evicted wrong image set", wave)

		// Verify evicted batch's pulled records were pruned from PullManager
		for s := range 2 {
			evictedEntry := env.unusedEntries[expectedBatch*benchUnusedBatchSize+s]
			mustPull, err := env.pullManager.MustAttemptImagePull(env.ctx, evictedEntry.img.RepoTags[0], evictedEntry.img.Id, func() ([]kubeletconfiginternal.ImagePullSecret, *kubeletconfiginternal.ImagePullServiceAccount, error) {
				return nil, nil, nil
			})
			require.NoError(t, err)
			require.True(t, mustPull, "pruned image %s must require re-authentication", evictedEntry.img.Id)
		}
	}

	// Verify all pinned (2), shared public (28), private tenant (30), and OCI ImageVolume (4) images remain intact.
	remaining, err := env.runtimeManager.ListImages(env.ctx)
	require.NoError(t, err)
	require.Len(t, remaining, 88) // 2 pinned + 28 public + 30 private + 4 volume + 24 warm unused

	// Verify wrong credentials on a secret-protected local image trigger re-pull & fail authentication.
	badSecret := makeDockerConfigJSONSecret("untrusted-ns", "bad-cred", "bad-uid", "us-central1-docker.pkg.dev/tenant-proj-00", "wrong-user", "wrong-pass")
	unauthPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "attacker-pod", Namespace: "untrusted-ns", UID: "attacker-uid"},
		Spec:       v1.PodSpec{ServiceAccountName: "default"},
	}
	unauthSandbox := &runtimeapi.PodSandboxConfig{
		Metadata: &runtimeapi.PodSandboxMetadata{Name: "attacker-pod", Namespace: "untrusted-ns", Uid: "attacker-uid"},
	}
	_, _, err = env.imageManager.EnsureImageExists(
		env.ctx,
		&v1.ObjectReference{Kind: "Pod", Namespace: "untrusted-ns", Name: "attacker-pod", UID: "attacker-uid"},
		unauthPod,
		"us-central1-docker.pkg.dev/tenant-proj-00/apps/backend-00:v2.4.1",
		[]v1.Secret{badSecret},
		unauthSandbox,
		"",
		v1.PullIfNotPresent,
	)
	require.ErrorIs(t, err, images.ErrImagePull)

	// Verify MinAge protection prevents premature eviction of newly detected unused images.
	minAgeStats := &benchStatsProvider{
		fsStats: &statsapi.FsStats{
			CapacityBytes:  ptr.To(uint64(benchCapacityBytes)),
			AvailableBytes: ptr.To(uint64(benchGCWaveAvailableBytes)),
		},
	}
	strictMinAgeGC, err := images.NewImageGCManager(
		env.runtimeManager,
		minAgeStats,
		nil,
		benchNoOpRecorder{},
		&v1.ObjectReference{Kind: "Node", Name: "bench-node-01"},
		images.ImageGCPolicy{
			HighThresholdPercent: 85,
			LowThresholdPercent:  80,
			MinAge:               10 * time.Minute,
		},
		noopoteltrace.NewTracerProvider(),
	)
	require.NoError(t, err)
	env.cri.evictedInWave = env.cri.evictedInWave[:0]
	err = strictMinAgeGC.GarbageCollect(env.ctx, env.beganGC)
	require.Error(t, err, "expected insufficient space freed when all unused images are younger than MinAge")
	require.Empty(t, env.cri.evictedInWave, "no images younger than MinAge may be evicted")
}
