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

package collectors_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cadvisorapi "github.com/google/cadvisor/lib/model"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel/trace"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	clientmetrics "k8s.io/client-go/tools/metrics"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/metrics/prometheus/controllers"
	_ "k8s.io/component-base/metrics/prometheus/restclient"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	csitrans "k8s.io/csi-translation-lib"
	"k8s.io/klog/v2"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	adcache "k8s.io/kubernetes/pkg/controller/volume/attachdetach/cache"
	admetrics "k8s.io/kubernetes/pkg/controller/volume/attachdetach/metrics"
	pvmetrics "k8s.io/kubernetes/pkg/controller/volume/persistentvolume/metrics"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/metrics/collectors"
	pmcache "k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	pmmetrics "k8s.io/kubernetes/pkg/kubelet/pluginmanager/metrics"
	serverstats "k8s.io/kubernetes/pkg/kubelet/server/stats"
	vmcache "k8s.io/kubernetes/pkg/kubelet/volumemanager/cache"
	vmmetrics "k8s.io/kubernetes/pkg/kubelet/volumemanager/metrics"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/csi"
	"k8s.io/kubernetes/pkg/volume/csimigration"
	volumetesting "k8s.io/kubernetes/pkg/volume/testing"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/operationexecutor"
	volumetypes "k8s.io/kubernetes/pkg/volume/util/types"
	"k8s.io/utils/ptr"
)

const (
	benchPodCount         = 60
	benchContainersPerPod = 3
	benchNamespaceCount   = 12
	benchNodeCount        = 60
	benchPVCount          = 240
	benchPluginCount      = 16
)

// benchResponseWriter captures HTTP handler output without httptest.NewRecorder
// per-scrape allocations so benchmarks measure real metrics exposition allocations.
type benchResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func newBenchResponseWriter(initialCap int) *benchResponseWriter {
	w := &benchResponseWriter{
		header: make(http.Header, 4),
		code:   http.StatusOK,
	}
	w.body.Grow(initialCap)
	return w
}

func (w *benchResponseWriter) Reset() {
	clear(w.header)
	w.body.Reset()
	w.code = http.StatusOK
}

func (w *benchResponseWriter) Header() http.Header {
	return w.header
}

func (w *benchResponseWriter) Write(b []byte) (int, error) {
	return w.body.Write(b)
}

func (w *benchResponseWriter) WriteHeader(statusCode int) {
	w.code = statusCode
}

// staticStatsProvider implements serverstats.Provider and serverstats.SummaryProvider
// with zero mock-framework reflection overhead.
type staticStatsProvider struct {
	summary  *statsapi.Summary
	podStats []statsapi.PodStats
}

var _ serverstats.Provider = (*staticStatsProvider)(nil)
var _ serverstats.SummaryProvider = (*staticStatsProvider)(nil)

func (p *staticStatsProvider) Get(_ context.Context, _ bool) (*statsapi.Summary, error) {
	return p.summary, nil
}
func (p *staticStatsProvider) GetCPUAndMemoryStats(_ context.Context) (*statsapi.Summary, error) {
	return p.summary, nil
}
func (p *staticStatsProvider) ListPodStats(_ context.Context) ([]statsapi.PodStats, error) {
	return p.podStats, nil
}
func (p *staticStatsProvider) ListPodCPUAndMemoryStats(_ context.Context) ([]statsapi.PodStats, error) {
	return p.podStats, nil
}
func (p *staticStatsProvider) ListPodStatsAndUpdateCPUNanoCoreUsage(_ context.Context) ([]statsapi.PodStats, error) {
	return p.podStats, nil
}
func (p *staticStatsProvider) ImageFsStats(_ context.Context) (*statsapi.FsStats, *statsapi.FsStats, error) {
	return nil, nil, nil
}
func (p *staticStatsProvider) GetCgroupStats(_ context.Context, _ string, _ bool) (*statsapi.ContainerStats, *statsapi.NetworkStats, error) {
	return nil, nil, nil
}
func (p *staticStatsProvider) GetCgroupCPUAndMemoryStats(_ string, _ bool) (*statsapi.ContainerStats, error) {
	return nil, nil
}
func (p *staticStatsProvider) RootFsStats() (*statsapi.FsStats, error) {
	return nil, nil
}
func (p *staticStatsProvider) GetRequestedContainersInfo(_ string, _ cadvisorapi.RequestOptions) (map[string]*cadvisorapi.ContainerInfo, error) {
	return nil, nil
}
func (p *staticStatsProvider) GetPodByName(_, _ string) (*v1.Pod, bool)    { return nil, false }
func (p *staticStatsProvider) GetNode(_ context.Context) (*v1.Node, error) { return nil, nil }
func (p *staticStatsProvider) GetNodeConfig() cm.NodeConfig                { return cm.NodeConfig{} }
func (p *staticStatsProvider) ListVolumesForPod(_ types.UID) (map[string]volume.Volume, bool) {
	return nil, false
}
func (p *staticStatsProvider) ListBlockVolumesForPod(_ types.UID) (map[string]volume.BlockVolume, bool) {
	return nil, false
}
func (p *staticStatsProvider) GetPods() []*v1.Pod                          { return nil }
func (p *staticStatsProvider) RlimitStats() (*statsapi.RlimitStats, error) { return nil, nil }
func (p *staticStatsProvider) GetPodCgroupRoot() string                    { return "/" }
func (p *staticStatsProvider) GetPodByCgroupfs(_ string) (*v1.Pod, bool)   { return nil, false }

type slicePVLister struct {
	pvs []*v1.PersistentVolume
}

func (l *slicePVLister) List() []*v1.PersistentVolume { return l.pvs }

type slicePVCLister struct {
	pvcs []*v1.PersistentVolumeClaim
}

func (l *slicePVCLister) List() []*v1.PersistentVolumeClaim { return l.pvcs }

// legacyCollectorsSingleton holds the process-wide registration onto legacyregistry
// (whose Register functions guard registration with sync.Once).
var (
	legacyOnce    sync.Once
	legacyHandler http.Handler
	ctrlMetrics   *controllers.ControllerManagerMetrics
	allowListOnce sync.Once
)

func initLegacyCollectors() {
	legacyOnce.Do(func() {
		logger := klog.Background()

		// 1. Initialize CSI + attachable fake volume plugin manager for PV, A/D, and Kubelet VolumeManager.
		var csiPluginMgr volume.VolumePluginMgr
		fakeClient := fake.NewSimpleClientset()
		fakeHost := volumetesting.NewFakeVolumeHost(nil, "/tmp/apo-kubelet-metrics-bench", fakeClient, nil)
		fakeAttachPlugin := &volumetesting.FakeAttachableVolumePlugin{
			FakeDeviceMountableVolumePlugin: volumetesting.FakeDeviceMountableVolumePlugin{
				FakeBasicVolumePlugin: volumetesting.FakeBasicVolumePlugin{
					Plugin: volumetesting.FakeVolumePlugin{
						PluginName: "fake-attach-plugin",
						Host:       fakeHost,
					},
				},
			},
		}
		_ = csiPluginMgr.InitPlugins(append(csi.ProbeVolumePlugins(), fakeAttachPlugin), nil, fakeHost)

		pvs := make([]*v1.PersistentVolume, 0, benchPVCount)
		pvcs := make([]*v1.PersistentVolumeClaim, 0, benchPVCount)

		pvIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
		pvcIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
		podIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})

		csiDrivers := []string{
			"pd.csi.storage.gke.io",
			"ebs.csi.aws.com",
			"disk.csi.azure.com",
			"filestore.csi.storage.gke.io",
		}
		storageClasses := []string{"standard-rwo", "premium-rwo", "fast-ssd", "archive-cold"}
		vacNames := []string{"", "silver-tier", "gold-tier"}
		fsMode := v1.PersistentVolumeFilesystem
		blockMode := v1.PersistentVolumeBlock

		for i := range benchPVCount {
			ns := fmt.Sprintf("tenant-ns-%02d", i%benchNamespaceCount)
			sc := storageClasses[i%len(storageClasses)]
			driver := csiDrivers[i%len(csiDrivers)]
			mode := &fsMode
			if i%5 == 0 {
				mode = &blockMode
			}
			pvName := fmt.Sprintf("pv-vol-%03d", i)
			pvcName := fmt.Sprintf("pvc-claim-%03d", i)
			pvcUID := types.UID(fmt.Sprintf("pvc-uid-%03d", i))

			pv := &v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: pvName,
					UID:  types.UID(fmt.Sprintf("pv-uid-%03d", i)),
				},
				Spec: v1.PersistentVolumeSpec{
					StorageClassName: sc,
					VolumeMode:       mode,
					Capacity: v1.ResourceList{
						v1.ResourceStorage: resource.MustParse("10Gi"),
					},
					AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
					PersistentVolumeSource: v1.PersistentVolumeSource{
						CSI: &v1.CSIPersistentVolumeSource{
							Driver:       driver,
							VolumeHandle: fmt.Sprintf("vol-handle-%03d", i),
						},
					},
				},
			}

			var vacPtr *string
			if vac := vacNames[i%len(vacNames)]; vac != "" {
				v := vac
				vacPtr = &v
			}
			pvc := &v1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pvcName,
					Namespace: ns,
					UID:       pvcUID,
				},
				Spec: v1.PersistentVolumeClaimSpec{
					StorageClassName:          &sc,
					VolumeAttributesClassName: vacPtr,
					AccessModes:               []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
					Resources: v1.VolumeResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceStorage: resource.MustParse("10Gi"),
						},
					},
				},
			}

			// First 200 are bound; remaining 40 exercise unbound PV and PVC metrics.
			if i < 200 {
				pv.Status.Phase = v1.VolumeBound
				pv.Spec.ClaimRef = &v1.ObjectReference{
					Namespace: ns,
					Name:      pvcName,
					UID:       pvcUID,
				}
				pvc.Spec.VolumeName = pvName
				pvc.Status.Phase = v1.ClaimBound
			} else {
				pv.Status.Phase = v1.VolumeAvailable
				pvc.Status.Phase = v1.ClaimPending
			}

			pvs = append(pvs, pv)
			pvcs = append(pvcs, pvc)
			_ = pvIndexer.Add(pv)
			_ = pvcIndexer.Add(pvc)
		}

		pvmetrics.Register(&slicePVLister{pvs: pvs}, &slicePVCLister{pvcs: pvcs}, &csiPluginMgr)

		// 2. Attach/Detach Controller state across 60 nodes and 60 running pods mounting bound CSI PVCs.
		adDSW := adcache.NewDesiredStateOfWorld(&csiPluginMgr)
		adASW := adcache.NewActualStateOfWorld(&csiPluginMgr)
		for i := range benchNodeCount {
			nodeName := types.NodeName(fmt.Sprintf("gke-pool-node-%02d", i))
			adDSW.AddNode(nodeName)
			ns := fmt.Sprintf("tenant-ns-%02d", i%benchNamespaceCount)
			pvcA := fmt.Sprintf("pvc-claim-%03d", i*2)
			pvcB := fmt.Sprintf("pvc-claim-%03d", i*2+1)
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("workload-pod-%02d", i),
					Namespace: ns,
					UID:       types.UID(fmt.Sprintf("workload-pod-uid-%02d", i)),
				},
				Spec: v1.PodSpec{
					NodeName: string(nodeName),
					Volumes: []v1.Volume{
						{
							Name: "data-a",
							VolumeSource: v1.VolumeSource{
								PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{ClaimName: pvcA},
							},
						},
						{
							Name: "data-b",
							VolumeSource: v1.VolumeSource{
								PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{ClaimName: pvcB},
							},
						},
					},
				},
				Status: v1.PodStatus{Phase: v1.PodRunning},
			}
			_ = podIndexer.Add(pod)

			attachSpec := volume.NewSpecFromVolume(&v1.Volume{
				Name: fmt.Sprintf("fake-attach-plugin-vol-%02d", i),
				VolumeSource: v1.VolumeSource{
					GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{PDName: fmt.Sprintf("pd-ad-%02d", i)},
				},
			})
			volNameA, err := adDSW.AddPod(volumetypes.UniquePodName(pod.UID), pod, attachSpec, nodeName)
			if err == nil {
				_, _ = adASW.AddVolumeNode(logger, volNameA, attachSpec, nodeName, "/dev/disk/by-id/vol-a", true)
			}
		}

		csiTranslator := csitrans.New()
		admetrics.Register(
			corelisters.NewPersistentVolumeClaimLister(pvcIndexer),
			corelisters.NewPersistentVolumeLister(pvIndexer),
			corelisters.NewPodLister(podIndexer),
			adASW,
			adDSW,
			&csiPluginMgr,
			csimigration.NewPluginManager(csiTranslator),
			csiTranslator,
		)

		// 3. Kubelet VolumeManager & PluginManager state.
		var kubeletVolMgr volume.VolumePluginMgr
		fakePlugin := &volumetesting.FakeVolumePlugin{PluginName: "kubernetes.io/fake-bench-plugin", Host: fakeHost}
		_ = kubeletVolMgr.InitPlugins(append(csi.ProbeVolumePlugins(), fakePlugin), nil, fakeHost)
		seLinuxTranslator := volumeutil.NewFakeSELinuxLabelTranslator()
		vmDSW := vmcache.NewDesiredStateOfWorld(&kubeletVolMgr, seLinuxTranslator)
		vmASW := vmcache.NewActualStateOfWorld(types.NodeName("gke-pool-node-00"), &kubeletVolMgr)

		for i := range benchPodCount {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("node-pod-%02d", i),
					Namespace: fmt.Sprintf("tenant-ns-%02d", i%benchNamespaceCount),
					UID:       types.UID(fmt.Sprintf("node-pod-uid-%02d", i)),
				},
			}
			var spec *volume.Spec
			if i%2 == 0 {
				spec = volume.NewSpecFromPersistentVolume(pvs[i], false)
			} else {
				vol := &v1.Volume{
					Name: fmt.Sprintf("ephemeral-%02d", i),
					VolumeSource: v1.VolumeSource{
						GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{PDName: fmt.Sprintf("pd-%02d", i)},
					},
				}
				spec = volume.NewSpecFromVolume(vol)
			}
			uniquePod := volumeutil.GetUniquePodName(pod)
			volName, err := vmDSW.AddPodToVolume(logger, uniquePod, pod, spec, spec.Name(), "", nil)
			if err == nil && i%2 == 1 {
				mounter, _ := fakePlugin.NewMounter(spec, pod)
				_ = vmASW.MarkVolumeAsAttached(logger, "", spec, "", "/dev/fake")
				_ = vmASW.AddPodToVolume(operationexecutor.MarkVolumeOpts{
					PodName:          uniquePod,
					PodUID:           pod.UID,
					VolumeName:       volName,
					Mounter:          mounter,
					VolumeSpec:       spec,
					VolumeMountState: operationexecutor.VolumeMounted,
				})
			}
		}
		vmmetrics.Register(vmASW, vmDSW, &kubeletVolMgr)

		pmDSW := pmcache.NewDesiredStateOfWorld()
		pmASW := pmcache.NewActualStateOfWorld()
		ctx := context.Background()
		for i := range benchPluginCount {
			sock := fmt.Sprintf("/var/lib/kubelet/plugins/driver-%02d/csi.sock", i)
			_ = pmDSW.AddOrUpdatePlugin(ctx, sock)
			if i < benchPluginCount-2 {
				_ = pmASW.AddPlugin(ctx, pmcache.PluginInfo{SocketPath: sock})
			}
		}
		pmmetrics.Register(pmASW, pmDSW)

		// 4. ControllerManager metrics.
		controllers.Register()
		ctrlMetrics = controllers.NewControllerManagerMetrics("kube-controller-manager")

		legacyHandler = legacyregistry.Handler()
	})
}

type podOpEvent struct {
	namespace string
	operation string
	reason    string
}

type highCardinalityMetricsEnv struct {
	clockNanos atomic.Int64

	resourceRegistry metrics.KubeRegistry
	resourceHandler  http.Handler

	nodeRegistry metrics.KubeRegistry
	nodeHandler  http.Handler

	collectorsToClear []metrics.StableCollector

	podOpsCounterVec     *metrics.CounterVec
	podActiveGaugeVec    *metrics.GaugeVec
	runtimeOpDurationVec *metrics.HistogramVec
	podQueueTimingVec    *metrics.TimingHistogramVec
	deprecatedCounter    *metrics.Counter
	scalarCounter        *metrics.Counter
	scalarHistogram      *metrics.Histogram

	unsampledCtx context.Context
	sampledCtx   context.Context

	podOpEvents     []podOpEvent
	controllerNames []string
	restHosts       []string
	restVerbs       []string
	restCodes       []string
	apiServerURL    *url.URL

	reqText        *http.Request
	reqOpenMetrics *http.Request
	reqGzipProto   *http.Request
	respWriter     *benchResponseWriter
}

func newHighCardinalityMetricsEnv(tb testing.TB) *highCardinalityMetricsEnv {
	tb.Helper()

	initLegacyCollectors()

	allowListOnce.Do(func() {
		metrics.SetLabelAllowList(map[string]string{
			"kubelet_bench_pod_ops_total,reason": "Scheduled,Pulled,Created,Started,ProbeSucceeded",
		})
	})

	env := &highCardinalityMetricsEnv{
		unsampledCtx: context.Background(),
		respWriter:   newBenchResponseWriter(512 * 1024),
	}
	env.clockNanos.Store(1700000000000000000)

	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	env.sampledCtx = trace.ContextWithSpanContext(context.Background(), spanCtx)

	baseTime := metav1.NewTime(time.UnixMilli(1700000000123))
	swapTime := metav1.NewTime(time.UnixMilli(1700000010123))
	startBase := metav1.NewTime(time.UnixMilli(1699999000456))

	containerNames := [benchContainersPerPod]string{"app", "sidecar", "envoy"}
	pods := make([]statsapi.PodStats, benchPodCount)
	for i := range benchPodCount {
		ns := fmt.Sprintf("tenant-ns-%02d", i%benchNamespaceCount)
		podName := fmt.Sprintf("pod-%02d-deployment-7f8c9", i)
		podUID := fmt.Sprintf("uid-pod-%04d", i)

		containers := make([]statsapi.ContainerStats, benchContainersPerPod)
		for cIdx := range benchContainersPerPod {
			cpuPtr := ptr.To(uint64((i*benchContainersPerPod + cIdx + 1) * 250_000_000))
			if i == benchPodCount-1 && cIdx == benchContainersPerPod-1 {
				// Exercise nil UsageCoreNanoSeconds container skip branch in addContainerCPUMetrics.
				cpuPtr = nil
			}
			workSet := uint64((cIdx+1)*64*1024*1024 + i*1024*1024)
			swapBytes := uint64(cIdx * 4 * 1024 * 1024)
			swapLim := uint64(64 * 1024 * 1024)
			logUsed := uint64((cIdx+1)*512*1024 + i*4096)

			containers[cIdx] = statsapi.ContainerStats{
				Name:      containerNames[cIdx],
				StartTime: metav1.NewTime(startBase.Time.Add(time.Duration(i*10+cIdx) * time.Second)),
				CPU: &statsapi.CPUStats{
					Time:                 baseTime,
					UsageCoreNanoSeconds: cpuPtr,
				},
				Memory: &statsapi.MemoryStats{
					Time:            baseTime,
					WorkingSetBytes: ptr.To(workSet),
				},
				Swap: &statsapi.SwapStats{
					Time:               swapTime,
					SwapUsageBytes:     ptr.To(swapBytes),
					SwapAvailableBytes: ptr.To(swapLim - swapBytes),
				},
				Logs: &statsapi.FsStats{
					Time:      baseTime,
					UsedBytes: ptr.To(logUsed),
				},
			}
		}

		// 2 unique PVCs per pod (= 120 unique PVCs), plus 1 shared PVC on first 10 pods
		// and 1 ephemeral volume without PVCRef to exercise VolumeStatsCollector filtering.
		volStats := make([]statsapi.VolumeStats, 0, 4)
		for vIdx := range 2 {
			pvcIdx := i*2 + vIdx
			capBytes := uint64(20 * 1024 * 1024 * 1024)
			usedBytes := uint64((pvcIdx + 1) * 50 * 1024 * 1024)
			availBytes := capBytes - usedBytes
			inodes := uint64(1_310_720)
			inodesUsed := uint64(1000 + pvcIdx*17)
			inodesFree := inodes - inodesUsed
			volStats = append(volStats, statsapi.VolumeStats{
				Name: fmt.Sprintf("vol-%d", vIdx),
				PVCRef: &statsapi.PVCReference{
					Namespace: ns,
					Name:      fmt.Sprintf("data-pvc-%03d", pvcIdx),
				},
				FsStats: statsapi.FsStats{
					Time:           baseTime,
					CapacityBytes:  ptr.To(capBytes),
					AvailableBytes: ptr.To(availBytes),
					UsedBytes:      ptr.To(usedBytes),
					Inodes:         ptr.To(inodes),
					InodesFree:     ptr.To(inodesFree),
					InodesUsed:     ptr.To(inodesUsed),
				},
			})
		}
		if i >= benchNamespaceCount && i < benchNamespaceCount+10 {
			// Shared RWX PVC already seen on an earlier pod in the same namespace.
			sharedOrigin := i % benchNamespaceCount
			volStats = append(volStats, statsapi.VolumeStats{
				Name: "shared-rwx",
				PVCRef: &statsapi.PVCReference{
					Namespace: ns,
					Name:      fmt.Sprintf("data-pvc-%03d", sharedOrigin*2),
				},
				FsStats: statsapi.FsStats{
					Time:           baseTime,
					CapacityBytes:  ptr.To(uint64(1024)),
					AvailableBytes: ptr.To(uint64(512)),
					UsedBytes:      ptr.To(uint64(512)),
					Inodes:         ptr.To(uint64(100)),
					InodesFree:     ptr.To(uint64(50)),
					InodesUsed:     ptr.To(uint64(50)),
				},
			})
		}
		volStats = append(volStats, statsapi.VolumeStats{
			Name:   "tmp-emptydir",
			PVCRef: nil,
			FsStats: statsapi.FsStats{
				Time:          baseTime,
				CapacityBytes: ptr.To(uint64(1024 * 1024)),
			},
		})

		pods[i] = statsapi.PodStats{
			PodRef: statsapi.PodReference{
				Name:      podName,
				Namespace: ns,
				UID:       podUID,
			},
			StartTime:  startBase,
			Containers: containers,
			CPU: &statsapi.CPUStats{
				Time:                 baseTime,
				UsageCoreNanoSeconds: ptr.To(uint64((i + 1) * 1_500_000_000)),
			},
			Memory: &statsapi.MemoryStats{
				Time:            baseTime,
				WorkingSetBytes: ptr.To(uint64((i + 1) * 192 * 1024 * 1024)),
			},
			Swap: &statsapi.SwapStats{
				Time:           swapTime,
				SwapUsageBytes: ptr.To(uint64(i * 8 * 1024 * 1024)),
			},
			VolumeStats: volStats,
		}
	}

	summary := &statsapi.Summary{
		Node: statsapi.NodeStats{
			NodeName: "gke-pool-node-00",
			CPU: &statsapi.CPUStats{
				Time:                 baseTime,
				UsageCoreNanoSeconds: ptr.To(uint64(987_654_000_000)),
			},
			Memory: &statsapi.MemoryStats{
				Time:            baseTime,
				WorkingSetBytes: ptr.To(uint64(24 * 1024 * 1024 * 1024)),
			},
			Swap: &statsapi.SwapStats{
				Time:           swapTime,
				SwapUsageBytes: ptr.To(uint64(512 * 1024 * 1024)),
			},
		},
		Pods: pods,
	}

	provider := &staticStatsProvider{
		summary:  summary,
		podStats: pods,
	}

	// Build CRI metric descriptors and 60 PodSandboxMetrics (840 CRI samples per scrape).
	criDescriptors := []*runtimeapi.MetricDescriptor{
		{
			Name:      "container_network_receive_bytes_total",
			Help:      "Cumulative count of bytes received",
			LabelKeys: []string{"pod", "namespace", "interface"},
		},
		{
			Name:      "container_network_transmit_bytes_total",
			Help:      "Cumulative count of bytes transmitted",
			LabelKeys: []string{"pod", "namespace", "interface"},
		},
		{
			Name:      "container_cpu_user_seconds_total",
			Help:      "Cumulative user cpu time consumed in seconds",
			LabelKeys: []string{"container", "pod", "namespace", "image"},
		},
		{
			Name:      "container_memory_rss",
			Help:      "Size of RSS in bytes",
			LabelKeys: []string{"container", "pod", "namespace", "image"},
		},
		{
			Name:      "container_oom_events_total",
			Help:      "Count of out of memory events observed for the container",
			LabelKeys: []string{"container", "pod", "namespace"},
		},
		{
			Name:      "container_fs_writes_bytes_total",
			Help:      "Cumulative count of bytes written",
			LabelKeys: []string{"container", "pod", "namespace", "device"},
		},
	}

	criTimestampNs := baseTime.Time.UnixNano()
	criPodMetrics := make([]*runtimeapi.PodSandboxMetrics, benchPodCount)
	for i := range benchPodCount {
		ns := fmt.Sprintf("tenant-ns-%02d", i%benchNamespaceCount)
		podName := fmt.Sprintf("pod-%02d-deployment-7f8c9", i)
		podUID := fmt.Sprintf("uid-pod-%04d", i)

		ctrMetrics := make([]*runtimeapi.ContainerMetrics, benchContainersPerPod)
		for cIdx := range benchContainersPerPod {
			cName := containerNames[cIdx]
			img := fmt.Sprintf("registry.k8s.io/workload/%s:v1.%d", cName, cIdx)
			ctrMetrics[cIdx] = &runtimeapi.ContainerMetrics{
				ContainerId: fmt.Sprintf("containerd://%s-%d", podUID, cIdx),
				Metrics: []*runtimeapi.Metric{
					{
						Name:        "container_cpu_user_seconds_total",
						Timestamp:   criTimestampNs,
						MetricType:  runtimeapi.MetricType_COUNTER,
						LabelValues: []string{cName, podName, ns, img},
						Value:       &runtimeapi.UInt64Value{Value: uint64(120 + i*3 + cIdx)},
					},
					{
						Name:        "container_memory_rss",
						Timestamp:   0,
						MetricType:  runtimeapi.MetricType_GAUGE,
						LabelValues: []string{cName, podName, ns, img},
						Value:       &runtimeapi.UInt64Value{Value: uint64((cIdx + 1) * 32 * 1024 * 1024)},
					},
					{
						Name:        "container_oom_events_total",
						Timestamp:   criTimestampNs,
						MetricType:  runtimeapi.MetricType_COUNTER,
						LabelValues: []string{cName, podName, ns},
						Value:       &runtimeapi.UInt64Value{Value: uint64(i % 2)},
					},
					{
						Name:        "container_fs_writes_bytes_total",
						Timestamp:   criTimestampNs,
						MetricType:  runtimeapi.MetricType_COUNTER,
						LabelValues: []string{cName, podName, ns, "/dev/sda1"},
						Value:       &runtimeapi.UInt64Value{Value: uint64((i + 1) * 65536)},
					},
				},
			}
		}

		criPodMetrics[i] = &runtimeapi.PodSandboxMetrics{
			PodSandboxId: podUID,
			Metrics: []*runtimeapi.Metric{
				{
					Name:        "container_network_receive_bytes_total",
					Timestamp:   criTimestampNs,
					MetricType:  runtimeapi.MetricType_COUNTER,
					LabelValues: []string{podName, ns, "eth0"},
					Value:       &runtimeapi.UInt64Value{Value: uint64((i + 1) * 1_048_576)},
				},
				{
					Name:        "container_network_transmit_bytes_total",
					Timestamp:   criTimestampNs,
					MetricType:  runtimeapi.MetricType_COUNTER,
					LabelValues: []string{podName, ns, "eth0"},
					Value:       &runtimeapi.UInt64Value{Value: uint64((i + 1) * 524_288)},
				},
			},
			ContainerMetrics: ctrMetrics,
		}
	}

	// Endpoint 1: /metrics/resource
	env.resourceRegistry = metrics.NewKubeRegistry()
	resCollector := collectors.NewResourceMetricsCollector(provider)
	env.resourceRegistry.CustomMustRegister(resCollector)
	env.resourceHandler = metrics.HandlerFor(env.resourceRegistry, metrics.HandlerOpts{
		ErrorHandling:     metrics.ContinueOnError,
		EnableOpenMetrics: true,
	})

	// Endpoint 2: Node custom collectors + high-cardinality metric vectors
	env.nodeRegistry = metrics.NewKubeRegistry()
	volCollector := collectors.NewVolumeStatsCollector(provider)
	logCollector := collectors.NewLogMetricsCollector(provider.ListPodStats)
	criCollector := collectors.NewCRIMetricsCollector(
		context.Background(),
		func(_ context.Context) ([]*runtimeapi.PodSandboxMetrics, error) { return criPodMetrics, nil },
		func(_ context.Context) ([]*runtimeapi.MetricDescriptor, error) { return criDescriptors, nil },
	)
	env.nodeRegistry.CustomMustRegister(volCollector, logCollector, criCollector)
	env.collectorsToClear = []metrics.StableCollector{resCollector, volCollector, logCollector, criCollector}

	env.podOpsCounterVec = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Name:           "kubelet_bench_pod_ops_total",
			Help:           "Cumulative pod lifecycle operations partitioned by namespace, operation, and allowlisted reason.",
			StabilityLevel: metrics.STABLE,
		},
		[]string{"namespace", "operation", "reason"},
	)
	env.podActiveGaugeVec = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Name:           "kubelet_bench_active_containers",
			Help:           "Current active containers partitioned by namespace and runtime_handler.",
			StabilityLevel: metrics.BETA,
		},
		[]string{"namespace", "runtime_handler"},
	)
	env.runtimeOpDurationVec = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Name:           "kubelet_bench_runtime_op_duration_seconds",
			Help:           "Latency in seconds of container runtime operations.",
			StabilityLevel: metrics.BETA,
			Buckets:        metrics.DefBuckets,
		},
		[]string{"operation_type", "runtime_handler"},
	)
	env.podQueueTimingVec = metrics.NewTestableTimingHistogramVec(
		func() time.Time {
			return time.Unix(0, env.clockNanos.Load())
		},
		&metrics.TimingHistogramOpts{
			Name:           "kubelet_bench_pod_queue_residency_seconds",
			Help:           "Time-weighted residency histogram of queued pods.",
			StabilityLevel: metrics.ALPHA,
			Buckets:        []float64{1, 2, 4, 8, 16, 32, 64},
			InitialValue:   0,
		},
		[]string{"namespace", "priority_bucket"},
	)
	env.deprecatedCounter = metrics.NewCounter(
		&metrics.CounterOpts{
			Name:              "kubelet_bench_deprecated_events_total",
			Help:              "Deprecated counter retained within deprecation window.",
			StabilityLevel:    metrics.BETA,
			DeprecatedVersion: "0.0.0",
		},
	)
	env.scalarCounter = metrics.NewCounter(
		&metrics.CounterOpts{
			Name:           "kubelet_bench_sync_loops_total",
			Help:           "Total sync loop iterations.",
			StabilityLevel: metrics.STABLE,
		},
	)
	env.scalarHistogram = metrics.NewHistogram(
		&metrics.HistogramOpts{
			Name:           "kubelet_bench_sync_duration_seconds",
			Help:           "Sync loop duration in seconds.",
			StabilityLevel: metrics.STABLE,
			Buckets:        metrics.DefBuckets,
		},
	)

	env.nodeRegistry.MustRegister(
		env.podOpsCounterVec,
		env.podActiveGaugeVec,
		env.runtimeOpDurationVec,
		env.podQueueTimingVec,
		env.deprecatedCounter,
		env.scalarCounter,
		env.scalarHistogram,
	)
	env.nodeHandler = metrics.HandlerWithReset(env.nodeRegistry, metrics.HandlerOpts{
		ErrorHandling:     metrics.ContinueOnError,
		EnableOpenMetrics: true,
	})

	allowedReasons := []string{"Scheduled", "Pulled", "Created", "Started", "ProbeSucceeded"}
	ops := []string{"syncPod", "startContainer", "probeContainer"}
	env.podOpEvents = make([]podOpEvent, 0, benchPodCount*benchContainersPerPod)
	for i := range benchPodCount * benchContainersPerPod {
		ns := fmt.Sprintf("tenant-ns-%02d", i%benchNamespaceCount)
		op := ops[i%len(ops)]
		var reason string
		if i%3 == 2 {
			// High-cardinality unbounded reason string rejected by SetLabelAllowList -> "unexpected".
			reason = fmt.Sprintf("DynamicRuntimeError-Pod-%03d", i)
		} else {
			reason = allowedReasons[i%len(allowedReasons)]
		}
		env.podOpEvents = append(env.podOpEvents, podOpEvent{
			namespace: ns,
			operation: op,
			reason:    reason,
		})
	}

	env.controllerNames = make([]string, 20)
	for i := range env.controllerNames {
		env.controllerNames[i] = fmt.Sprintf("controller-worker-%02d", i)
	}
	env.restHosts = []string{"apiserver.cluster.local:6443", "10.96.0.1:443", "auth-webhook.system.svc:8443"}
	env.restVerbs = []string{"GET", "LIST", "WATCH", "PATCH", "POST"}
	env.restCodes = []string{"200", "201", "429"}
	env.apiServerURL, _ = url.Parse("https://apiserver.cluster.local:6443/api/v1/pods")

	reqText, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:10250/metrics", nil)
	reqText.Header.Set("Accept", "text/plain; version=0.0.4; charset=utf-8")
	env.reqText = reqText

	reqOM, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:10250/metrics", nil)
	reqOM.Header.Set("Accept", "application/openmetrics-text; version=1.0.0; charset=utf-8")
	env.reqOpenMetrics = reqOM

	reqGzip, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:10250/metrics", nil)
	reqGzip.Header.Set("Accept", "application/vnd.google.protobuf; proto=io.prometheus.client.MetricFamily; encoding=delimited")
	reqGzip.Header.Set("Accept-Encoding", "gzip")
	env.reqGzipProto = reqGzip

	tb.Cleanup(func() {
		for _, c := range env.collectorsToClear {
			c.ClearState()
		}
	})

	return env
}

func (env *highCardinalityMetricsEnv) recordWaveMetrics() {
	runtimeHandlers := [3]string{"runc", "gvisor", "kata"}
	opTypes := [6]string{"run_podsandbox", "create_container", "start_container", "container_status", "list_containers", "exec_sync"}
	priorityBuckets := [3]string{"system-critical", "high", "normal"}

	// Advance deterministic clock so TimingHistogramVec integrates residency time across waves
	// while remaining byte-identical across multi-format scrapes within the same wave.
	env.clockNanos.Add(int64(100 * time.Millisecond))

	// 1. High-cardinality CounterVec updates with active MetricLabelAllowList.
	for i, ev := range env.podOpEvents {
		if i%4 == 0 {
			env.podOpsCounterVec.With(metrics.Labels{
				"namespace": ev.namespace,
				"operation": ev.operation,
				"reason":    ev.reason,
			}).Inc()
		} else {
			env.podOpsCounterVec.WithLabelValues(ev.namespace, ev.operation, ev.reason).Inc()
		}
	}

	// 2. GaugeVec & TimingHistogramVec updates across tenant namespaces.
	for nsIdx := range benchNamespaceCount {
		ns := fmt.Sprintf("tenant-ns-%02d", nsIdx)
		for hIdx, handler := range runtimeHandlers {
			env.podActiveGaugeVec.WithLabelValues(ns, handler).Set(float64(5 + nsIdx + hIdx))
		}
		for pIdx, prio := range priorityBuckets {
			env.podQueueTimingVec.WithLabelValues(ns, prio).Set(float64((nsIdx + 1) * (pIdx + 1)))
		}
	}

	// 3. HistogramVec & scalar exemplar-capable WithContext updates (unsampled + sampled).
	for i := range 180 {
		op := opTypes[i%len(opTypes)]
		rh := runtimeHandlers[i%len(runtimeHandlers)]
		dur := 0.002 + float64(i%25)*0.004
		if i%6 == 0 {
			env.runtimeOpDurationVec.WithContext(env.sampledCtx).WithLabelValues(op, rh).Observe(dur)
			env.scalarCounter.WithContext(env.sampledCtx).Inc()
			env.scalarHistogram.WithContext(env.sampledCtx).Observe(dur)
		} else {
			env.runtimeOpDurationVec.WithContext(env.unsampledCtx).WithLabelValues(op, rh).Observe(dur)
			env.scalarCounter.WithContext(env.unsampledCtx).Inc()
			env.scalarHistogram.WithContext(env.unsampledCtx).Observe(dur)
		}
	}

	env.deprecatedCounter.Inc()

	// 4. Client-go restclient & ControllerManager metrics updates.
	for i := range 60 {
		host := env.restHosts[i%len(env.restHosts)]
		verb := env.restVerbs[i%len(env.restVerbs)]
		code := env.restCodes[i%len(env.restCodes)]
		ctx := env.unsampledCtx
		if i%10 == 0 {
			ctx = env.sampledCtx
		}
		clientmetrics.RequestLatency.Observe(ctx, verb, *env.apiServerURL, 12*time.Millisecond)
		clientmetrics.RequestSize.Observe(ctx, verb, host, 1024)
		clientmetrics.ResponseSize.Observe(ctx, verb, host, 8192)
		clientmetrics.RequestResult.Increment(ctx, code, verb, host)
		clientmetrics.ResolverLatency.Observe(ctx, host, 2*time.Millisecond)
		clientmetrics.ExecPluginCalls.Increment(0, "no_error")
	}
	for i, name := range env.controllerNames {
		if i == len(env.controllerNames)-1 {
			ctrlMetrics.ControllerStopped(name)
		} else {
			ctrlMetrics.ControllerStarted(name)
		}
	}
}

func (env *highCardinalityMetricsEnv) runWave(tb testing.TB) {
	env.recordWaveMetrics()

	// Scrape 1: /metrics/resource in Prometheus text format (1,085 timestamped samples).
	env.respWriter.Reset()
	env.resourceHandler.ServeHTTP(env.respWriter, env.reqText)
	if env.respWriter.code != http.StatusOK || env.respWriter.body.Len() == 0 {
		tb.Fatalf("resourceHandler text scrape failed: status=%d len=%d", env.respWriter.code, env.respWriter.body.Len())
	}

	// Scrape 2: /metrics/resource in OpenMetrics text format.
	env.respWriter.Reset()
	env.resourceHandler.ServeHTTP(env.respWriter, env.reqOpenMetrics)
	if env.respWriter.code != http.StatusOK || env.respWriter.body.Len() == 0 {
		tb.Fatalf("resourceHandler openmetrics scrape failed: status=%d len=%d", env.respWriter.code, env.respWriter.body.Len())
	}

	// Scrape 3: Node custom collectors + high-cardinality vectors in Prometheus text format.
	env.respWriter.Reset()
	env.nodeHandler.ServeHTTP(env.respWriter, env.reqText)
	if env.respWriter.code != http.StatusOK || env.respWriter.body.Len() == 0 {
		tb.Fatalf("nodeHandler text scrape failed: status=%d len=%d", env.respWriter.code, env.respWriter.body.Len())
	}

	// Scrape 4: Node custom collectors + high-cardinality vectors in gzipped protobuf format.
	env.respWriter.Reset()
	env.nodeHandler.ServeHTTP(env.respWriter, env.reqGzipProto)
	if env.respWriter.code != http.StatusOK || env.respWriter.body.Len() == 0 {
		tb.Fatalf("nodeHandler gzip protobuf scrape failed: status=%d len=%d", env.respWriter.code, env.respWriter.body.Len())
	}

	// Scrape 5: Controller & VolumeManager collectors registered on legacyregistry (/metrics).
	env.respWriter.Reset()
	legacyHandler.ServeHTTP(env.respWriter, env.reqText)
	if env.respWriter.code != http.StatusOK || env.respWriter.body.Len() == 0 {
		tb.Fatalf("legacyHandler text scrape failed: status=%d len=%d", env.respWriter.code, env.respWriter.body.Len())
	}
}

func BenchmarkHighCardinalityMetrics(b *testing.B) {
	env := newHighCardinalityMetricsEnv(b)
	// Warm up lazy series registration and HTTP/gzip buffer pools before timed scrape/update waves.
	for range 3 {
		env.runWave(b)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		env.runWave(b)
	}
}

func scrapeString(t *testing.T, h http.Handler, accept, acceptEncoding string) (string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:10250/metrics", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	w := newBenchResponseWriter(64 * 1024)
	h.ServeHTTP(w, req)
	if w.code != http.StatusOK {
		t.Fatalf("unexpected HTTP status %d: %s", w.code, w.body.String())
	}
	raw := w.body.Bytes()
	if acceptEncoding == "gzip" {
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("gzip.NewReader: %v", err)
		}
		defer gz.Close()
		decoded, err := io.ReadAll(gz)
		if err != nil {
			t.Fatalf("gzip ReadAll: %v", err)
		}
		return string(decoded), w.header.Clone()
	}
	return string(raw), w.header.Clone()
}

// normalizeOpenMetricsWallClock canonicalizes OpenTelemetry exemplar label map order
// and replaces wall-clock time.Now() timestamps emitted by prometheus/client_golang on
// _created lines and exemplars ("# {...} val ts"), keeping metric names, labels, bucket
// values, exemplar values, and collector timestamps intact.
func normalizeOpenMetricsWallClock(om string) string {
	lines := strings.Split(om, "\n")
	for i, line := range lines {
		if idx := strings.Index(line, " # {"); idx != -1 {
			if closeBrace := strings.IndexByte(line[idx+4:], '}'); closeBrace != -1 {
				rawPairs := strings.Split(line[idx+4:idx+4+closeBrace], ",")
				slices.Sort(rawPairs)
				line = line[:idx+4] + strings.Join(rawPairs, ",") + line[idx+4+closeBrace:]
			}
			if lastSpace := strings.LastIndexByte(line, ' '); lastSpace > idx {
				lines[i] = line[:lastSpace] + " <EXEMPLAR_TS>"
			}
			continue
		}
		if strings.Contains(line, "_created") && !strings.HasPrefix(line, "#") {
			if lastSpace := strings.LastIndexByte(line, ' '); lastSpace != -1 {
				lines[i] = line[:lastSpace] + " <CREATED_TS>"
			}
		}
	}
	return strings.Join(lines, "\n")
}

func TestHighCardinalityMetricsEquivalence(t *testing.T) {
	t.Run("ByteForByteWireDeterminismAndFormatting", func(t *testing.T) {
		envA := newHighCardinalityMetricsEnv(t)
		envA.recordWaveMetrics()

		resTextA, _ := scrapeString(t, envA.resourceHandler, "text/plain; version=0.0.4; charset=utf-8", "")
		resOMA, _ := scrapeString(t, envA.resourceHandler, "application/openmetrics-text; version=1.0.0; charset=utf-8", "")
		nodeTextA, _ := scrapeString(t, envA.nodeHandler, "text/plain; version=0.0.4; charset=utf-8", "")
		nodeGzipTextA, gzHdr := scrapeString(t, envA.nodeHandler, "text/plain; version=0.0.4; charset=utf-8", "gzip")
		nodeOMA, _ := scrapeString(t, envA.nodeHandler, "application/openmetrics-text; version=1.0.0; charset=utf-8", "")

		if gotEnc := gzHdr.Get("Content-Encoding"); gotEnc != "gzip" {
			t.Fatalf("expected Content-Encoding=gzip, got %q", gotEnc)
		}
		if nodeGzipTextA != nodeTextA {
			t.Fatalf("decompressed gzip text output differs from uncompressed text output")
		}

		// Clean descriptor state and construct a second independent environment to verify
		// 100% deterministic byte-for-byte wire output across runs.
		for _, c := range envA.collectorsToClear {
			c.ClearState()
		}
		envB := newHighCardinalityMetricsEnv(t)
		envB.recordWaveMetrics()

		resTextB, _ := scrapeString(t, envB.resourceHandler, "text/plain; version=0.0.4; charset=utf-8", "")
		resOMB, _ := scrapeString(t, envB.resourceHandler, "application/openmetrics-text; version=1.0.0; charset=utf-8", "")
		nodeTextB, _ := scrapeString(t, envB.nodeHandler, "text/plain; version=0.0.4; charset=utf-8", "")
		nodeOMB, _ := scrapeString(t, envB.nodeHandler, "application/openmetrics-text; version=1.0.0; charset=utf-8", "")

		if resTextA != resTextB {
			t.Fatalf("/metrics/resource text wire output is non-deterministic")
		}
		if resOMA != resOMB {
			t.Fatalf("/metrics/resource OpenMetrics wire output is non-deterministic")
		}
		if nodeTextA != nodeTextB {
			t.Fatalf("nodeHandler text wire output is non-deterministic")
		}
		if normA, normB := normalizeOpenMetricsWallClock(nodeOMA), normalizeOpenMetricsWallClock(nodeOMB); normA != normB {
			linesA := strings.Split(normA, "\n")
			linesB := strings.Split(normB, "\n")
			for idx := 0; idx < len(linesA) && idx < len(linesB); idx++ {
				if linesA[idx] != linesB[idx] {
					t.Fatalf("nodeHandler OpenMetrics mismatch at line %d:\nA: %s\nB: %s", idx, linesA[idx], linesB[idx])
				}
			}
			t.Fatalf("nodeHandler OpenMetrics line count mismatch: %d vs %d", len(linesA), len(linesB))
		}

		// Verify stability annotations, timestamps, zero-CPU container filtering, and OpenMetrics EOF/exemplars.
		mustContainResource := []string{
			"# HELP node_cpu_usage_seconds_total [STABLE] Cumulative cpu time consumed by the node in core-seconds\n# TYPE node_cpu_usage_seconds_total counter\nnode_cpu_usage_seconds_total 987.654 1700000000123\n",
			"# HELP node_swap_usage_bytes [ALPHA] Current swap usage of the node in bytes. Reported only on non-windows systems\n# TYPE node_swap_usage_bytes gauge\nnode_swap_usage_bytes 5.36870912e+08 1700000010123\n",
			`container_cpu_usage_seconds_total{container="app",namespace="tenant-ns-00",pod="pod-00-deployment-7f8c9"} 0.25 1700000000123`,
			`pod_cpu_usage_seconds_total{namespace="tenant-ns-00",pod="pod-00-deployment-7f8c9"} 1.5 1700000000123`,
			"resource_scrape_error 0\n",
		}
		for _, wantSub := range mustContainResource {
			if !strings.Contains(resTextA, wantSub) {
				t.Errorf("missing expected /metrics/resource substring:\n%s", wantSub)
			}
		}
		// Pod 59 container "envoy" has UsageCoreNanoSeconds == 0 and must be omitted from container_cpu_usage_seconds_total.
		if strings.Contains(resTextA, `container_cpu_usage_seconds_total{container="envoy",namespace="tenant-ns-11",pod="pod-59-deployment-7f8c9"}`) {
			t.Errorf("expected zero-CPU container envoy on pod-59 to be omitted from container_cpu_usage_seconds_total")
		}
		if !strings.HasSuffix(resOMA, "# EOF\n") || !strings.HasSuffix(nodeOMA, "# EOF\n") {
			t.Errorf("expected OpenMetrics output to end with '# EOF\\n'")
		}
		if !strings.Contains(nodeOMA, `trace_id="4bf92f3577b34da6a3ce929d0e0e4736"`) {
			t.Errorf("expected sampled OpenTelemetry trace exemplar in OpenMetrics output")
		}

		// Verify allowlist cardinality enforcement, deprecation header, and PVC deduplication.
		mustContainNode := []string{
			"# HELP kubelet_bench_pod_ops_total [STABLE] Cumulative pod lifecycle operations partitioned by namespace, operation, and allowlisted reason.\n",
			"# HELP kubelet_bench_deprecated_events_total [BETA] (Deprecated since 0.0.0) Deprecated counter retained within deprecation window.\n",
			`kubelet_volume_stats_capacity_bytes{namespace="tenant-ns-00",persistentvolumeclaim="data-pvc-000"} 2.147483648e+10`,
			`kubelet_container_log_filesystem_used_bytes{container="app",namespace="tenant-ns-00",pod="pod-00-deployment-7f8c9",uid="uid-pod-0000"} 524288`,
			`container_network_receive_bytes_total{interface="eth0",namespace="tenant-ns-00",pod="pod-00-deployment-7f8c9"} 1.048576e+06 1700000000123`,
			`reason="unexpected"`,
		}
		for _, wantSub := range mustContainNode {
			if !strings.Contains(nodeTextA, wantSub) {
				t.Errorf("missing expected nodeHandler substring:\n%s", wantSub)
			}
		}
		if strings.Contains(nodeTextA, "DynamicRuntimeError-Pod-") {
			t.Errorf("unbounded reason label leaked past MetricLabelAllowList")
		}

		// Count exact kubelet_volume_stats_capacity_bytes sample lines: must equal 120 unique PVCs (shared RWX deduplicated).
		volCapLines := 0
		for _, line := range strings.Split(nodeTextA, "\n") {
			if strings.HasPrefix(line, "kubelet_volume_stats_capacity_bytes{") {
				volCapLines++
			}
		}
		if volCapLines != 120 {
			t.Fatalf("expected 120 deduplicated kubelet_volume_stats_capacity_bytes samples, got %d", volCapLines)
		}

		// Verify DELETE method on HandlerWithReset resets vector counters without affecting custom collectors.
		delReq, _ := http.NewRequest(http.MethodDelete, "http://127.0.0.1:10250/metrics", nil)
		delW := newBenchResponseWriter(128)
		envB.nodeHandler.ServeHTTP(delW, delReq)
		if delW.body.String() != "metrics reset\n" {
			t.Fatalf("unexpected DELETE response %q", delW.body.String())
		}
		afterResetText, _ := scrapeString(t, envB.nodeHandler, "text/plain; version=0.0.4; charset=utf-8", "")
		if strings.Contains(afterResetText, "kubelet_bench_pod_ops_total{") {
			t.Fatalf("expected CounterVec series to be cleared after HandlerWithReset DELETE")
		}
		if !strings.Contains(afterResetText, "kubelet_volume_stats_capacity_bytes{") {
			t.Fatalf("expected custom StableCollector series to remain active after HandlerWithReset DELETE")
		}
	})

	t.Run("LegacyRegistryControllerAndVolumeCollectors", func(t *testing.T) {
		env := newHighCardinalityMetricsEnv(t)
		env.recordWaveMetrics()

		expectedPVAndVolume := `
# HELP plugin_manager_total_plugins [ALPHA] Number of plugins in Plugin Manager
# TYPE plugin_manager_total_plugins gauge
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-00/csi.sock",state="actual_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-00/csi.sock",state="desired_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-01/csi.sock",state="actual_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-01/csi.sock",state="desired_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-02/csi.sock",state="actual_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-02/csi.sock",state="desired_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-03/csi.sock",state="actual_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-03/csi.sock",state="desired_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-04/csi.sock",state="actual_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-04/csi.sock",state="desired_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-05/csi.sock",state="actual_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-05/csi.sock",state="desired_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-06/csi.sock",state="actual_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-06/csi.sock",state="desired_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-07/csi.sock",state="actual_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-07/csi.sock",state="desired_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-08/csi.sock",state="actual_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-08/csi.sock",state="desired_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-09/csi.sock",state="actual_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-09/csi.sock",state="desired_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-10/csi.sock",state="actual_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-10/csi.sock",state="desired_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-11/csi.sock",state="actual_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-11/csi.sock",state="desired_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-12/csi.sock",state="actual_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-12/csi.sock",state="desired_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-13/csi.sock",state="actual_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-13/csi.sock",state="desired_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-14/csi.sock",state="desired_state_of_world"} 1
plugin_manager_total_plugins{socket_path="/var/lib/kubelet/plugins/driver-15/csi.sock",state="desired_state_of_world"} 1
# HELP pv_collector_bound_pv_count [ALPHA] Gauge measuring number of persistent volume currently bound
# TYPE pv_collector_bound_pv_count gauge
pv_collector_bound_pv_count{storage_class="archive-cold"} 50
pv_collector_bound_pv_count{storage_class="fast-ssd"} 50
pv_collector_bound_pv_count{storage_class="premium-rwo"} 50
pv_collector_bound_pv_count{storage_class="standard-rwo"} 50
# HELP pv_collector_total_pv_count [ALPHA] Gauge measuring total number of persistent volumes
# TYPE pv_collector_total_pv_count gauge
pv_collector_total_pv_count{plugin_name="kubernetes.io/csi:disk.csi.azure.com",volume_mode="Block"} 12
pv_collector_total_pv_count{plugin_name="kubernetes.io/csi:disk.csi.azure.com",volume_mode="Filesystem"} 48
pv_collector_total_pv_count{plugin_name="kubernetes.io/csi:ebs.csi.aws.com",volume_mode="Block"} 12
pv_collector_total_pv_count{plugin_name="kubernetes.io/csi:ebs.csi.aws.com",volume_mode="Filesystem"} 48
pv_collector_total_pv_count{plugin_name="kubernetes.io/csi:filestore.csi.storage.gke.io",volume_mode="Block"} 12
pv_collector_total_pv_count{plugin_name="kubernetes.io/csi:filestore.csi.storage.gke.io",volume_mode="Filesystem"} 48
pv_collector_total_pv_count{plugin_name="kubernetes.io/csi:pd.csi.storage.gke.io",volume_mode="Block"} 12
pv_collector_total_pv_count{plugin_name="kubernetes.io/csi:pd.csi.storage.gke.io",volume_mode="Filesystem"} 48
# HELP pv_collector_unbound_pv_count [ALPHA] Gauge measuring number of persistent volume currently unbound
# TYPE pv_collector_unbound_pv_count gauge
pv_collector_unbound_pv_count{storage_class="archive-cold"} 10
pv_collector_unbound_pv_count{storage_class="fast-ssd"} 10
pv_collector_unbound_pv_count{storage_class="premium-rwo"} 10
pv_collector_unbound_pv_count{storage_class="standard-rwo"} 10
# HELP volume_manager_total_volumes [ALPHA] Number of volumes in Volume Manager
# TYPE volume_manager_total_volumes gauge
volume_manager_total_volumes{plugin_name="kubernetes.io/fake-bench-plugin",state="actual_state_of_world"} 30
volume_manager_total_volumes{plugin_name="kubernetes.io/fake-bench-plugin",state="desired_state_of_world"} 30
`
		if err := promtestutil.GatherAndCompare(
			legacyregistry.DefaultGatherer,
			strings.NewReader(expectedPVAndVolume),
			"plugin_manager_total_plugins",
			"pv_collector_bound_pv_count",
			"pv_collector_total_pv_count",
			"pv_collector_unbound_pv_count",
			"volume_manager_total_volumes",
		); err != nil {
			t.Fatalf("legacyregistry GatherAndCompare mismatch: %v", err)
		}

		// Also verify AttachDetach in-use volumes, ControllerManager gauges, and restclient counters are present.
		mfs, err := legacyregistry.DefaultGatherer.Gather()
		if err != nil {
			t.Fatalf("legacyregistry Gather failed: %v", err)
		}
		foundFamilies := make(map[string]int, len(mfs))
		for _, mf := range mfs {
			foundFamilies[mf.GetName()] = len(mf.GetMetric())
		}
		for _, reqFamily := range []string{
			"storage_count_attachable_volumes_in_use",
			"attachdetach_controller_total_volumes",
			"pv_collector_bound_pvc_count",
			"pv_collector_unbound_pvc_count",
			"running_managed_controllers",
			"rest_client_requests_total",
			"rest_client_exec_plugin_call_total",
			"cardinality_enforcement_unexpected_categorizations_total",
		} {
			if foundFamilies[reqFamily] == 0 {
				t.Errorf("expected non-empty metric family %q in legacyregistry, got %d samples", reqFamily, foundFamilies[reqFamily])
			}
		}
	})

	t.Run("ConcurrentScrapesAndUpdatesRaceSafety", func(t *testing.T) {
		env := newHighCardinalityMetricsEnv(t)
		var wg sync.WaitGroup
		for worker := range 8 {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for iter := range 4 {
					if id%2 == 0 {
						env.recordWaveMetrics()
					} else {
						accept := "text/plain; version=0.0.4; charset=utf-8"
						if iter%2 == 1 {
							accept = "application/openmetrics-text; version=1.0.0; charset=utf-8"
						}
						_, _ = scrapeString(t, env.resourceHandler, accept, "")
						_, _ = scrapeString(t, env.nodeHandler, accept, "gzip")
						_, _ = scrapeString(t, legacyHandler, accept, "")
					}
				}
			}(worker)
		}
		wg.Wait()
	})
}

var _ labels.Selector = nil
var _ kubecontainer.Runtime = nil
