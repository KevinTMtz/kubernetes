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

package config

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/kubelet/configmap"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/secret"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
)

const (
	benchNamespaces    = 12
	benchPodsPerNS     = 5 // 60 regular pods total
	benchTotalPods     = benchNamespaces * benchPodsPerNS
	benchStaticPods    = 4
	benchObjectsPerNS  = 3 // 36 Secrets + 36 ConfigMaps (18 mutable + 18 immutable each)
	benchTotalObjects  = benchNamespaces * benchObjectsPerNS
	benchEphemeralPods = 2
	benchNodeName      = "node-bench-1"
	benchNodeIndex     = "spec.nodeName"
	benchInitialRV     = "100"
)

type nsNameKey struct {
	ns   string
	name string
}

// streamWatcher is a minimal, zero-overhead watch.Interface for benchmark and equivalence tests.
type streamWatcher struct {
	ch      chan watch.Event
	onStop  func(*streamWatcher)
	stopped atomic.Bool
}

func newStreamWatcher(buf int, onStop func(*streamWatcher)) *streamWatcher {
	return &streamWatcher{
		ch:     make(chan watch.Event, buf),
		onStop: onStop,
	}
}

func (w *streamWatcher) Stop() {
	if w.stopped.CompareAndSwap(false, true) {
		if w.onStop != nil {
			w.onStop(w)
		}
		close(w.ch)
	}
}

func (w *streamWatcher) ResultChan() <-chan watch.Event {
	return w.ch
}

func (w *streamWatcher) trySend(ev watch.Event) bool {
	if w.stopped.Load() {
		return false
	}
	select {
	case w.ch <- ev:
		return true
	default:
		return false
	}
}

// benchKubeClient implements the exact CoreV1 Secret and ConfigMap Get/List/Watch surface
// needed by Watching/Caching Secret & ConfigMap managers without fake.Clientset action-log growth.
type benchKubeClient struct {
	clientset.Interface
	core *benchCoreV1
}

func (c *benchKubeClient) CoreV1() corev1client.CoreV1Interface {
	return c.core
}

type benchCoreV1 struct {
	corev1client.CoreV1Interface
	mu             sync.RWMutex
	secrets        map[nsNameKey]*v1.Secret
	configMaps     map[nsNameKey]*v1.ConfigMap
	secretWatches  map[nsNameKey][]*streamWatcher
	cmWatches      map[nsNameKey][]*streamWatcher
	secretBookmark *v1.Secret
	cmBookmark     *v1.ConfigMap
}

func newBenchKubeClient() *benchKubeClient {
	core := &benchCoreV1{
		secrets:       make(map[nsNameKey]*v1.Secret, 64),
		configMaps:    make(map[nsNameKey]*v1.ConfigMap, 64),
		secretWatches: make(map[nsNameKey][]*streamWatcher, 64),
		cmWatches:     make(map[nsNameKey][]*streamWatcher, 64),
		secretBookmark: &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				ResourceVersion: benchInitialRV,
				Annotations:     map[string]string{metav1.InitialEventsAnnotationKey: "true"},
			},
		},
		cmBookmark: &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				ResourceVersion: benchInitialRV,
				Annotations:     map[string]string{metav1.InitialEventsAnnotationKey: "true"},
			},
		},
	}
	return &benchKubeClient{core: core}
}

func (c *benchCoreV1) Secrets(namespace string) corev1client.SecretInterface {
	return benchSecretClient{core: c, ns: namespace}
}

func (c *benchCoreV1) ConfigMaps(namespace string) corev1client.ConfigMapInterface {
	return benchConfigMapClient{core: c, ns: namespace}
}

func (c *benchCoreV1) setSecret(s *v1.Secret, broadcast bool) {
	key := nsNameKey{ns: s.Namespace, name: s.Name}
	c.mu.Lock()
	c.secrets[key] = s
	var watchers []*streamWatcher
	if broadcast {
		watchers = append(watchers, c.secretWatches[key]...)
	}
	c.mu.Unlock()
	for _, w := range watchers {
		w.trySend(watch.Event{Type: watch.Modified, Object: s})
	}
}

func (c *benchCoreV1) setConfigMap(cm *v1.ConfigMap, broadcast bool) {
	key := nsNameKey{ns: cm.Namespace, name: cm.Name}
	c.mu.Lock()
	c.configMaps[key] = cm
	var watchers []*streamWatcher
	if broadcast {
		watchers = append(watchers, c.cmWatches[key]...)
	}
	c.mu.Unlock()
	for _, w := range watchers {
		w.trySend(watch.Event{Type: watch.Modified, Object: cm})
	}
}

func (c *benchCoreV1) activeSecretWatchCount(ns, name string) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.secretWatches[nsNameKey{ns: ns, name: name}])
}

func (c *benchCoreV1) activeConfigMapWatchCount(ns, name string) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cmWatches[nsNameKey{ns: ns, name: name}])
}

func parseFieldSelectorName(fs string) string {
	if after, ok := strings.CutPrefix(fs, "metadata.name="); ok {
		return after
	}
	return fs
}

type benchSecretClient struct {
	corev1client.SecretInterface
	core *benchCoreV1
	ns   string
}

func (s benchSecretClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*v1.Secret, error) {
	s.core.mu.RLock()
	obj, ok := s.core.secrets[nsNameKey{ns: s.ns, name: name}]
	s.core.mu.RUnlock()
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
	}
	return obj, nil
}

func (s benchSecretClient) List(_ context.Context, opts metav1.ListOptions) (*v1.SecretList, error) {
	name := parseFieldSelectorName(opts.FieldSelector)
	s.core.mu.RLock()
	obj, ok := s.core.secrets[nsNameKey{ns: s.ns, name: name}]
	s.core.mu.RUnlock()
	out := &v1.SecretList{ListMeta: metav1.ListMeta{ResourceVersion: benchInitialRV}}
	if ok {
		out.Items = []v1.Secret{*obj}
	}
	return out, nil
}

func (s benchSecretClient) Watch(_ context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	name := parseFieldSelectorName(opts.FieldSelector)
	key := nsNameKey{ns: s.ns, name: name}
	var w *streamWatcher
	w = newStreamWatcher(8, func(target *streamWatcher) {
		s.core.mu.Lock()
		list := s.core.secretWatches[key]
		for i, item := range list {
			if item == target {
				s.core.secretWatches[key] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(s.core.secretWatches[key]) == 0 {
			delete(s.core.secretWatches, key)
		}
		s.core.mu.Unlock()
	})
	s.core.mu.Lock()
	obj, ok := s.core.secrets[key]
	if opts.SendInitialEvents != nil && *opts.SendInitialEvents {
		if ok {
			w.ch <- watch.Event{Type: watch.Added, Object: obj}
		}
		w.ch <- watch.Event{Type: watch.Bookmark, Object: s.core.secretBookmark}
	}
	s.core.secretWatches[key] = append(s.core.secretWatches[key], w)
	s.core.mu.Unlock()
	return w, nil
}

type benchConfigMapClient struct {
	corev1client.ConfigMapInterface
	core *benchCoreV1
	ns   string
}

func (c benchConfigMapClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*v1.ConfigMap, error) {
	c.core.mu.RLock()
	obj, ok := c.core.configMaps[nsNameKey{ns: c.ns, name: name}]
	c.core.mu.RUnlock()
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
	}
	return obj, nil
}

func (c benchConfigMapClient) List(_ context.Context, opts metav1.ListOptions) (*v1.ConfigMapList, error) {
	name := parseFieldSelectorName(opts.FieldSelector)
	c.core.mu.RLock()
	obj, ok := c.core.configMaps[nsNameKey{ns: c.ns, name: name}]
	c.core.mu.RUnlock()
	out := &v1.ConfigMapList{ListMeta: metav1.ListMeta{ResourceVersion: benchInitialRV}}
	if ok {
		out.Items = []v1.ConfigMap{*obj}
	}
	return out, nil
}

func (c benchConfigMapClient) Watch(_ context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	name := parseFieldSelectorName(opts.FieldSelector)
	key := nsNameKey{ns: c.ns, name: name}
	var w *streamWatcher
	w = newStreamWatcher(8, func(target *streamWatcher) {
		c.core.mu.Lock()
		list := c.core.cmWatches[key]
		for i, item := range list {
			if item == target {
				c.core.cmWatches[key] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(c.core.cmWatches[key]) == 0 {
			delete(c.core.cmWatches, key)
		}
		c.core.mu.Unlock()
	})
	c.core.mu.Lock()
	obj, ok := c.core.configMaps[key]
	if opts.SendInitialEvents != nil && *opts.SendInitialEvents {
		if ok {
			w.ch <- watch.Event{Type: watch.Added, Object: obj}
		}
		w.ch <- watch.Event{Type: watch.Bookmark, Object: c.core.cmBookmark}
	}
	c.core.cmWatches[key] = append(c.core.cmWatches[key], w)
	c.core.mu.Unlock()
	return w, nil
}

// podStreamLW implements cache.ListerWatcherWithContext supporting WatchList streaming.
type podStreamLW struct {
	initialPods []*v1.Pod
	bookmark    *v1.Pod
	watcher     *streamWatcher
}

func newPodStreamLW(initial []*v1.Pod, buf int) *podStreamLW {
	return &podStreamLW{
		initialPods: initial,
		bookmark: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				ResourceVersion: benchInitialRV,
				Annotations:     map[string]string{metav1.InitialEventsAnnotationKey: "true"},
			},
		},
		watcher: newStreamWatcher(buf, nil),
	}
}

func (lw *podStreamLW) List(opts metav1.ListOptions) (runtime.Object, error) {
	return lw.ListWithContext(context.Background(), opts)
}

func (lw *podStreamLW) ListWithContext(_ context.Context, _ metav1.ListOptions) (runtime.Object, error) {
	items := make([]v1.Pod, len(lw.initialPods))
	for i, p := range lw.initialPods {
		items[i] = *p.DeepCopy()
	}
	return &v1.PodList{
		ListMeta: metav1.ListMeta{ResourceVersion: benchInitialRV},
		Items:    items,
	}, nil
}

func (lw *podStreamLW) Watch(opts metav1.ListOptions) (watch.Interface, error) {
	return lw.WatchWithContext(context.Background(), opts)
}

func (lw *podStreamLW) WatchWithContext(_ context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	if opts.SendInitialEvents != nil && *opts.SendInitialEvents {
		for _, p := range lw.initialPods {
			lw.watcher.ch <- watch.Event{Type: watch.Added, Object: p.DeepCopy()}
		}
		lw.watcher.ch <- watch.Event{Type: watch.Bookmark, Object: lw.bookmark}
	}
	return lw.watcher, nil
}

func podNodeIndexFunc(obj interface{}) ([]string, error) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		return nil, fmt.Errorf("not a pod: %T", obj)
	}
	if pod.Spec.NodeName == "" {
		return nil, nil
	}
	return []string{pod.Spec.NodeName}, nil
}

type watchCachesBenchEnv struct {
	ctx        context.Context
	cancel     context.CancelFunc
	stopCh     chan struct{}
	kubeClient *benchKubeClient

	watchingSecretMgr    secret.Manager
	watchingConfigMapMgr configmap.Manager
	cachingSecretMgr     secret.Manager
	cachingConfigMapMgr  configmap.Manager

	namespaces  []string
	secretKeys  []nsNameKey
	cmKeys      []nsNameKey
	secretsRevA []*v1.Secret
	secretsRevB []*v1.Secret
	cmsRevA     []*v1.ConfigMap
	cmsRevB     []*v1.ConfigMap

	mgrPodsRevA []*v1.Pod
	mgrPodsRevB []*v1.Pod

	ephemeralSecrets [benchEphemeralPods]*v1.Secret
	ephemeralCMs     [benchEphemeralPods]*v1.ConfigMap
	ephemeralMgrPods [benchEphemeralPods]*v1.Pod
	ephemeralCfgPods [benchEphemeralPods]*v1.Pod
	ephemeralInfPods [benchEphemeralPods]*v1.Pod

	podConfig    *PodConfig
	apiserverLW  *podStreamLW
	podManager   kubepod.Manager
	cfgSemanticA []*v1.Pod
	cfgSemanticB []*v1.Pod
	cfgStatusA   []*v1.Pod
	cfgStatusB   []*v1.Pod
	staticPods   []*v1.Pod
	mirrorPods   []*v1.Pod

	informer        cache.SharedIndexInformer
	informerLW      *podStreamLW
	genericLister   cache.GenericLister
	infPodsRevA     []*v1.Pod
	infPodsRevB     []*v1.Pod
	infBookmarkRevA *v1.Pod
	infBookmarkRevB *v1.Pod
	handlerPending  atomic.Int32
	handlerDoneCh   chan struct{}

	deltaKnownStore cache.Store
	deltaFIFO       *cache.DeltaFIFO
}

func makeRealisticManagedFields() []metav1.ManagedFieldsEntry {
	now := metav1.NewTime(time.Unix(1700000000, 0))
	return []metav1.ManagedFieldsEntry{
		{
			Manager:    "kube-controller-manager",
			Operation:  metav1.ManagedFieldsOperationUpdate,
			APIVersion: "v1",
			Time:       &now,
			FieldsType: "FieldsV1",
			FieldsV1:   metav1.NewFieldsV1(`{"f:metadata":{"f:annotations":{},"f:labels":{}},"f:spec":{"f:containers":{},"f:volumes":{}}}`),
		},
		{
			Manager:     "kubelet",
			Operation:   metav1.ManagedFieldsOperationUpdate,
			APIVersion:  "v1",
			Time:        &now,
			FieldsType:  "FieldsV1",
			Subresource: "status",
			FieldsV1:    metav1.NewFieldsV1(`{"f:status":{"f:conditions":{},"f:containerStatuses":{},"f:phase":{},"f:podIP":{}}}`),
		},
	}
}

func buildWorkloadPod(ns, name string, uid types.UID, rv string, secretA, secretB, cmA, cmB string, revLabel, statusMsg string) *v1.Pod {
	optional := false
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       ns,
			Name:            name,
			UID:             uid,
			ResourceVersion: rv,
			Generation:      1,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "workload",
				"app.kubernetes.io/instance":  name,
				"topology.kubernetes.io/zone": "us-central1-a",
				"revision":                    revLabel,
			},
			Annotations: map[string]string{
				"kubernetes.io/psp":                 "restricted",
				"prometheus.io/scrape":              "true",
				"prometheus.io/port":                "9090",
				"workload.example.io/config-digest": revLabel,
			},
			ManagedFields: makeRealisticManagedFields(),
		},
		Spec: v1.PodSpec{
			NodeName:      benchNodeName,
			RestartPolicy: v1.RestartPolicyAlways,
			DNSPolicy:     v1.DNSClusterFirst,
			ImagePullSecrets: []v1.LocalObjectReference{
				{Name: secretA},
			},
			InitContainers: []v1.Container{
				{
					Name:  "init-config",
					Image: "registry.k8s.io/busybox:1.36",
					EnvFrom: []v1.EnvFromSource{
						{ConfigMapRef: &v1.ConfigMapEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: cmA}}},
					},
				},
			},
			Containers: []v1.Container{
				{
					Name:  "app",
					Image: "registry.k8s.io/pause:3.10",
					Env: []v1.EnvVar{
						{
							Name: "DB_PASSWORD",
							ValueFrom: &v1.EnvVarSource{
								SecretKeyRef: &v1.SecretKeySelector{
									LocalObjectReference: v1.LocalObjectReference{Name: secretB},
									Key:                  "password",
									Optional:             &optional,
								},
							},
						},
						{
							Name: "LOG_LEVEL",
							ValueFrom: &v1.EnvVarSource{
								ConfigMapKeyRef: &v1.ConfigMapKeySelector{
									LocalObjectReference: v1.LocalObjectReference{Name: cmB},
									Key:                  "log-level",
									Optional:             &optional,
								},
							},
						},
					},
					VolumeMounts: []v1.VolumeMount{
						{Name: "tls-vol", MountPath: "/etc/tls", ReadOnly: true},
						{Name: "cfg-vol", MountPath: "/etc/cfg", ReadOnly: true},
						{Name: "projected-vol", MountPath: "/var/run/projected", ReadOnly: true},
					},
				},
				{
					Name:  "sidecar",
					Image: "registry.k8s.io/pause:3.10",
					EnvFrom: []v1.EnvFromSource{
						{SecretRef: &v1.SecretEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: secretA}}},
					},
				},
			},
			Volumes: []v1.Volume{
				{
					Name: "tls-vol",
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{SecretName: secretA},
					},
				},
				{
					Name: "cfg-vol",
					VolumeSource: v1.VolumeSource{
						ConfigMap: &v1.ConfigMapVolumeSource{LocalObjectReference: v1.LocalObjectReference{Name: cmA}},
					},
				},
				{
					Name: "projected-vol",
					VolumeSource: v1.VolumeSource{
						Projected: &v1.ProjectedVolumeSource{
							Sources: []v1.VolumeProjection{
								{Secret: &v1.SecretProjection{LocalObjectReference: v1.LocalObjectReference{Name: secretB}}},
								{ConfigMap: &v1.ConfigMapProjection{LocalObjectReference: v1.LocalObjectReference{Name: cmB}}},
							},
						},
					},
				},
			},
		},
		Status: v1.PodStatus{
			Phase:   v1.PodRunning,
			PodIP:   "10.244.1.15",
			HostIP:  "10.128.0.10",
			Message: statusMsg,
			Conditions: []v1.PodCondition{
				{Type: v1.PodReady, Status: v1.ConditionTrue},
				{Type: v1.ContainersReady, Status: v1.ConditionTrue},
			},
		},
	}
}

func newWatchCachesBenchEnv(tb testing.TB) *watchCachesBenchEnv {
	tb.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopCh := make(chan struct{})
	tb.Cleanup(func() {
		close(stopCh)
		cancel()
	})

	kc := newBenchKubeClient()
	env := &watchCachesBenchEnv{
		ctx:                  ctx,
		cancel:               cancel,
		stopCh:               stopCh,
		kubeClient:           kc,
		watchingSecretMgr:    secret.NewWatchingSecretManager(kc, time.Minute),
		watchingConfigMapMgr: configmap.NewWatchingConfigMapManager(kc, time.Minute),
		cachingSecretMgr:     secret.NewCachingSecretManager(kc, func() (time.Duration, bool) { return time.Minute, true }),
		cachingConfigMapMgr:  configmap.NewCachingConfigMapManager(kc, func() (time.Duration, bool) { return time.Minute, true }),
		namespaces:           make([]string, benchNamespaces),
		secretKeys:           make([]nsNameKey, 0, benchTotalObjects),
		cmKeys:               make([]nsNameKey, 0, benchTotalObjects),
		secretsRevA:          make([]*v1.Secret, 0, benchTotalObjects),
		secretsRevB:          make([]*v1.Secret, 0, benchTotalObjects),
		cmsRevA:              make([]*v1.ConfigMap, 0, benchTotalObjects),
		cmsRevB:              make([]*v1.ConfigMap, 0, benchTotalObjects),
		mgrPodsRevA:          make([]*v1.Pod, 0, benchTotalPods),
		mgrPodsRevB:          make([]*v1.Pod, 0, benchTotalPods),
		cfgSemanticA:         make([]*v1.Pod, 0, benchTotalPods),
		cfgSemanticB:         make([]*v1.Pod, 0, benchTotalPods),
		cfgStatusA:           make([]*v1.Pod, 0, benchTotalPods),
		cfgStatusB:           make([]*v1.Pod, 0, benchTotalPods),
		infPodsRevA:          make([]*v1.Pod, 0, benchTotalPods),
		infPodsRevB:          make([]*v1.Pod, 0, benchTotalPods),
		handlerDoneCh:        make(chan struct{}, 1),
	}

	immutableTrue := true
	immutableFalse := false
	for nsIdx := 0; nsIdx < benchNamespaces; nsIdx++ {
		ns := fmt.Sprintf("ns-%02d", nsIdx)
		env.namespaces[nsIdx] = ns
		for objIdx := 0; objIdx < benchObjectsPerNS; objIdx++ {
			isImm := (objIdx == 2) // objIdx 0,1 mutable (24); objIdx 2 immutable (12)
			immPtr := &immutableFalse
			if isImm {
				immPtr = &immutableTrue
			}
			secName := fmt.Sprintf("sec-%02d", objIdx)
			cmName := fmt.Sprintf("cm-%02d", objIdx)
			sA := &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:       ns,
					Name:            secName,
					UID:             types.UID(fmt.Sprintf("uid-%s-%s", ns, secName)),
					ResourceVersion: "101",
				},
				Immutable: immPtr,
				Data: map[string][]byte{
					"password": []byte("s3cr3t-rev-a-" + ns + "-" + secName),
					"tls.crt":  []byte("-----BEGIN CERTIFICATE-----\nMIIB...revA\n-----END CERTIFICATE-----"),
				},
			}
			sB := sA.DeepCopy()
			sB.ResourceVersion = "102"
			sB.Data["password"] = []byte("s3cr3t-rev-b-" + ns + "-" + secName)

			cmA := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:       ns,
					Name:            cmName,
					UID:             types.UID(fmt.Sprintf("uid-%s-%s", ns, cmName)),
					ResourceVersion: "101",
				},
				Immutable: immPtr,
				Data: map[string]string{
					"log-level": "info-rev-a",
					"app.yaml":  "server:\n  port: 8080\n  mode: rev-a\n",
				},
			}
			cmB := cmA.DeepCopy()
			cmB.ResourceVersion = "102"
			cmB.Data["log-level"] = "debug-rev-b"

			kc.core.setSecret(sA, false)
			kc.core.setConfigMap(cmA, false)
			env.secretKeys = append(env.secretKeys, nsNameKey{ns: ns, name: secName})
			env.cmKeys = append(env.cmKeys, nsNameKey{ns: ns, name: cmName})
			env.secretsRevA = append(env.secretsRevA, sA)
			env.secretsRevB = append(env.secretsRevB, sB)
			env.cmsRevA = append(env.cmsRevA, cmA)
			env.cmsRevB = append(env.cmsRevB, cmB)
		}
	}

	initialApiserverPods := make([]*v1.Pod, 0, benchTotalPods+benchStaticPods)
	initialInformerPods := make([]*v1.Pod, 0, benchTotalPods)

	for nsIdx := 0; nsIdx < benchNamespaces; nsIdx++ {
		ns := env.namespaces[nsIdx]
		for pIdx := 0; pIdx < benchPodsPerNS; pIdx++ {
			name := fmt.Sprintf("pod-%02d", pIdx)
			uid := types.UID(fmt.Sprintf("uid-%s-%s", ns, name))
			secA := fmt.Sprintf("sec-%02d", pIdx%benchObjectsPerNS)
			secB := fmt.Sprintf("sec-%02d", (pIdx+1)%benchObjectsPerNS)
			cmA := fmt.Sprintf("cm-%02d", pIdx%benchObjectsPerNS)
			cmB := fmt.Sprintf("cm-%02d", (pIdx+1)%benchObjectsPerNS)

			pA := buildWorkloadPod(ns, name, uid, "201", secA, secB, cmA, cmB, "rev-a", "healthy-a")
			pB := buildWorkloadPod(ns, name, uid, "202", secA, secB, cmA, cmB, "rev-b", "healthy-b")
			env.mgrPodsRevA = append(env.mgrPodsRevA, pA)
			env.mgrPodsRevB = append(env.mgrPodsRevB, pB)

			// Separate copies for PodConfig (which mutates Annotations in-place)
			cfgInit := pA.DeepCopy()
			initialApiserverPods = append(initialApiserverPods, cfgInit)
			env.cfgSemanticA = append(env.cfgSemanticA, pA.DeepCopy())
			env.cfgSemanticB = append(env.cfgSemanticB, pB.DeepCopy())

			stA := pA.DeepCopy()
			stA.ResourceVersion = "203"
			stA.Status.Message = "status-check-a"
			stB := pA.DeepCopy()
			stB.ResourceVersion = "204"
			stB.Status.Message = "status-check-b"
			env.cfgStatusA = append(env.cfgStatusA, stA)
			env.cfgStatusB = append(env.cfgStatusB, stB)

			// Separate copies for SharedIndexInformer
			initialInformerPods = append(initialInformerPods, pA.DeepCopy())
			env.infPodsRevA = append(env.infPodsRevA, pA.DeepCopy())
			env.infPodsRevB = append(env.infPodsRevB, pB.DeepCopy())

			// Register steady-state pods across all 4 managers and warm initial watches/caches
			env.watchingSecretMgr.RegisterPod(pA)
			env.watchingConfigMapMgr.RegisterPod(pA)
			env.cachingSecretMgr.RegisterPod(pA)
			env.cachingConfigMapMgr.RegisterPod(pA)
		}
	}

	// Warm all steady-state secret & configmap watches so reflectors complete initial sync before ResetTimer
	for _, key := range env.secretKeys {
		if _, err := env.watchingSecretMgr.GetSecret(key.ns, key.name); err != nil {
			tb.Fatalf("warm watchingSecretMgr %v: %v", key, err)
		}
		if _, err := env.cachingSecretMgr.GetSecret(key.ns, key.name); err != nil {
			tb.Fatalf("warm cachingSecretMgr %v: %v", key, err)
		}
	}
	for _, key := range env.cmKeys {
		if _, err := env.watchingConfigMapMgr.GetConfigMap(key.ns, key.name); err != nil {
			tb.Fatalf("warm watchingConfigMapMgr %v: %v", key, err)
		}
		if _, err := env.cachingConfigMapMgr.GetConfigMap(key.ns, key.name); err != nil {
			tb.Fatalf("warm cachingConfigMapMgr %v: %v", key, err)
		}
	}

	// Ephemeral Job pod fixtures (registered & unregistered inside each wave)
	for i := 0; i < benchEphemeralPods; i++ {
		ns := env.namespaces[i]
		secName := fmt.Sprintf("ephemeral-sec-%d", i)
		cmName := fmt.Sprintf("ephemeral-cm-%d", i)
		podName := fmt.Sprintf("ephemeral-job-%d", i)
		uid := types.UID(fmt.Sprintf("uid-ephemeral-%d", i))
		es := &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: secName, UID: types.UID("uid-" + secName), ResourceVersion: "301"},
			Immutable:  &immutableTrue,
			Data:       map[string][]byte{"password": []byte("job-token")},
		}
		ecm := &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: cmName, UID: types.UID("uid-" + cmName), ResourceVersion: "301"},
			Immutable:  &immutableTrue,
			Data:       map[string]string{"log-level": "warn"},
		}
		kc.core.setSecret(es, false)
		kc.core.setConfigMap(ecm, false)
		env.ephemeralSecrets[i] = es
		env.ephemeralCMs[i] = ecm
		ep := buildWorkloadPod(ns, podName, uid, "301", secName, secName, cmName, cmName, "job", "running")
		env.ephemeralMgrPods[i] = ep
		env.ephemeralCfgPods[i] = ep.DeepCopy()
		env.ephemeralInfPods[i] = ep.DeepCopy()
	}

	// Static pods + mirror pods for PodConfig + pod.Manager
	for i := 0; i < benchStaticPods; i++ {
		hash := fmt.Sprintf("static-hash-%d", i)
		spName := fmt.Sprintf("kube-static-%d-%s", i, benchNodeName)
		sp := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:       "kube-system",
				Name:            spName,
				UID:             types.UID(fmt.Sprintf("static-uid-%d", i)),
				ResourceVersion: "100",
				Annotations: map[string]string{
					kubetypes.ConfigHashAnnotationKey:   hash,
					kubetypes.ConfigSourceAnnotationKey: kubetypes.FileSource,
				},
			},
			Spec: v1.PodSpec{
				NodeName:   benchNodeName,
				Containers: []v1.Container{{Name: "ctrl", Image: "registry.k8s.io/pause:3.10"}},
			},
		}
		mp := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:       "kube-system",
				Name:            spName,
				UID:             types.UID(fmt.Sprintf("mirror-uid-%d", i)),
				ResourceVersion: "100",
				Annotations: map[string]string{
					kubetypes.ConfigMirrorAnnotationKey: hash,
				},
			},
			Spec: sp.Spec,
		}
		env.staticPods = append(env.staticPods, sp)
		env.mirrorPods = append(env.mirrorPods, mp)
		initialApiserverPods = append(initialApiserverPods, mp.DeepCopy())
	}

	// Initialize PodConfig (ApiserverSource via newSourceApiserverFromLW + FileSource) & pod.Manager
	broadcaster := record.NewBroadcaster(record.WithContext(ctx))
	recorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "kubelet"})
	env.podConfig = NewPodConfig(recorder, &mockPodStartupSLIObserver{})
	env.podManager = kubepod.NewBasicPodManager()

	fileCh := env.podConfig.Channel(ctx, kubetypes.FileSource)
	apiCh := env.podConfig.Channel(ctx, kubetypes.ApiserverSource)
	env.apiserverLW = newPodStreamLW(initialApiserverPods, 256)
	newSourceApiserverFromLW(env.apiserverLW, apiCh)

	staticSend := make([]*v1.Pod, len(env.staticPods))
	for i, sp := range env.staticPods {
		staticSend[i] = sp.DeepCopy()
	}
	fileCh <- sourceUpdate{Pods: staticSend}

	// Drain initial ADD updates from both FileSource and ApiserverSource into podManager
	for seenPods := 0; seenPods < len(initialApiserverPods)+len(env.staticPods); {
		select {
		case upd := <-env.podConfig.Updates():
			for _, p := range upd.Pods {
				env.podManager.AddPod(p)
				seenPods++
			}
		case <-time.After(5 * time.Second):
			tb.Fatalf("timed out draining initial PodConfig updates (got %d)", seenPods)
		}
	}

	// Initialize SharedIndexInformer with NamespaceIndex + NodeName index + 2 ResourceEventHandlers
	env.informerLW = newPodStreamLW(initialInformerPods, 256)
	env.informer = cache.NewSharedIndexInformerWithOptions(
		env.informerLW,
		&v1.Pod{},
		cache.SharedIndexInformerOptions{
			ResyncPeriod: 0,
			Indexers: cache.Indexers{
				cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
				benchNodeIndex:       podNodeIndexFunc,
			},
		},
	)
	notifyEvent := func() {
		if env.handlerPending.Add(-1) == 0 {
			select {
			case env.handlerDoneCh <- struct{}{}:
			default:
			}
		}
	}
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ interface{}) { notifyEvent() },
		UpdateFunc: func(_, _ interface{}) { notifyEvent() },
		DeleteFunc: func(_ interface{}) { notifyEvent() },
	}
	if _, err := env.informer.AddEventHandler(handler); err != nil {
		tb.Fatalf("AddEventHandler 1: %v", err)
	}
	if _, err := env.informer.AddEventHandler(handler); err != nil {
		tb.Fatalf("AddEventHandler 2: %v", err)
	}
	go env.informer.Run(stopCh)
	if !cache.WaitForCacheSync(stopCh, env.informer.HasSynced) {
		tb.Fatal("SharedIndexInformer failed initial sync")
	}
	// Drain any leftover signal from initial sync
	select {
	case <-env.handlerDoneCh:
	default:
	}

	env.genericLister = cache.NewGenericLister(env.informer.GetIndexer(), v1.SchemeGroupVersion.WithResource("pods").GroupResource())
	env.infBookmarkRevA = &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "500"}}
	env.infBookmarkRevB = &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "501"}}

	// Initialize DeltaFIFO + KnownObjects store for direct DeltaFIFO coverage
	env.deltaKnownStore = cache.NewStore(cache.MetaNamespaceKeyFunc)
	for _, p := range env.infPodsRevA {
		_ = env.deltaKnownStore.Add(p)
	}
	env.deltaFIFO = cache.NewDeltaFIFOWithOptions(cache.DeltaFIFOOptions{
		KeyFunction:  cache.DeletionHandlingMetaNamespaceKeyFunc,
		KnownObjects: env.deltaKnownStore,
	})

	return env
}

func (env *watchCachesBenchEnv) applyPodUpdateToManager(upd kubetypes.PodUpdate) {
	switch upd.Op {
	case kubetypes.ADD:
		for _, p := range upd.Pods {
			env.podManager.AddPod(p)
		}
	case kubetypes.UPDATE, kubetypes.RECONCILE, kubetypes.DELETE:
		for _, p := range upd.Pods {
			env.podManager.UpdatePod(p)
		}
	case kubetypes.REMOVE:
		for _, p := range upd.Pods {
			env.podManager.RemovePod(p)
		}
	}
}

func (env *watchCachesBenchEnv) runWave(tb testing.TB, wave int) {
	flip := (wave & 1) == 1

	mgrPods := env.mgrPodsRevA
	secrets := env.secretsRevA
	cms := env.cmsRevA
	cfgSem := env.cfgSemanticB // wave 0 transitions from initial RevA -> RevB!
	cfgStat := env.cfgStatusA
	infPods := env.infPodsRevB
	infBookmark := env.infBookmarkRevB
	if flip {
		mgrPods = env.mgrPodsRevB
		secrets = env.secretsRevB
		cms = env.cmsRevB
		cfgSem = env.cfgSemanticA
		cfgStat = env.cfgStatusB
		infPods = env.infPodsRevA
		infBookmark = env.infBookmarkRevA
	}

	// 1. Mounted Object Managers: RegisterPod updates, watch broadcasts, high-frequency GetSecret/GetConfigMap, & ephemeral Job pod churn
	offset := (wave * 7) % 30
	for i := 0; i < 30; i++ {
		p := mgrPods[(offset+i)%benchTotalPods]
		env.watchingSecretMgr.RegisterPod(p)
		env.watchingConfigMapMgr.RegisterPod(p)
		if i < 12 {
			env.cachingSecretMgr.RegisterPod(p)
			env.cachingConfigMapMgr.RegisterPod(p)
		}
	}

	// Broadcast 4 mutable Secret & 4 mutable ConfigMap updates
	for i := 0; i < 4; i++ {
		// even objIdx (0, 2) in each namespace are mutable; step by 3 to hit mutable entries
		idx := ((wave + i) * benchObjectsPerNS) % benchTotalObjects
		env.kubeClient.core.setSecret(secrets[idx], true)
		env.kubeClient.core.setConfigMap(cms[idx], true)
	}

	// High-frequency mounted volume & env secret/configmap reads across all 60 pods
	for round := 0; round < 2; round++ {
		for _, key := range env.secretKeys {
			if _, err := env.watchingSecretMgr.GetSecret(key.ns, key.name); err != nil {
				tb.Fatalf("watchingSecretMgr.GetSecret(%v): %v", key, err)
			}
		}
		for _, key := range env.cmKeys {
			if _, err := env.watchingConfigMapMgr.GetConfigMap(key.ns, key.name); err != nil {
				tb.Fatalf("watchingConfigMapMgr.GetConfigMap(%v): %v", key, err)
			}
		}
	}
	for i := 0; i < 24; i++ {
		keyS := env.secretKeys[i]
		if _, err := env.cachingSecretMgr.GetSecret(keyS.ns, keyS.name); err != nil {
			tb.Fatalf("cachingSecretMgr.GetSecret(%v): %v", keyS, err)
		}
		keyC := env.cmKeys[i]
		if _, err := env.cachingConfigMapMgr.GetConfigMap(keyC.ns, keyC.name); err != nil {
			tb.Fatalf("cachingConfigMapMgr.GetConfigMap(%v): %v", keyC, err)
		}
	}

	// Short-lived Job pod registration, watch sync, read, and unregistration
	for i := 0; i < benchEphemeralPods; i++ {
		ep := env.ephemeralMgrPods[i]
		es := env.ephemeralSecrets[i]
		ecm := env.ephemeralCMs[i]
		env.watchingSecretMgr.RegisterPod(ep)
		env.watchingConfigMapMgr.RegisterPod(ep)
		if _, err := env.watchingSecretMgr.GetSecret(es.Namespace, es.Name); err != nil {
			tb.Fatalf("ephemeral GetSecret: %v", err)
		}
		if _, err := env.watchingConfigMapMgr.GetConfigMap(ecm.Namespace, ecm.Name); err != nil {
			tb.Fatalf("ephemeral GetConfigMap: %v", err)
		}
		env.watchingSecretMgr.UnregisterPod(ep)
		env.watchingConfigMapMgr.UnregisterPod(ep)
	}

	// 2. Kubelet UndeltaStore + PodConfig + pod.Manager sync-loop reads
	// Send 4 semantic updates, 2 status-only reconciles, + 1 ephemeral Add + 1 ephemeral Delete
	for i := 0; i < 4; i++ {
		env.apiserverLW.watcher.ch <- watch.Event{Type: watch.Modified, Object: cfgSem[i]}
		upd := <-env.podConfig.Updates()
		env.applyPodUpdateToManager(upd)
	}
	for i := 4; i < 6; i++ {
		env.apiserverLW.watcher.ch <- watch.Event{Type: watch.Modified, Object: cfgStat[i]}
		upd := <-env.podConfig.Updates()
		env.applyPodUpdateToManager(upd)
	}
	ephCfg := env.ephemeralCfgPods[wave&1]
	env.apiserverLW.watcher.ch <- watch.Event{Type: watch.Added, Object: ephCfg}
	env.applyPodUpdateToManager(<-env.podConfig.Updates())
	env.apiserverLW.watcher.ch <- watch.Event{Type: watch.Deleted, Object: ephCfg}
	env.applyPodUpdateToManager(<-env.podConfig.Updates())

	if pods := env.podManager.GetPods(); len(pods) != benchTotalPods+benchStaticPods {
		tb.Fatalf("unexpected GetPods len: %d", len(pods))
	}
	allPods, allMirror, orphans := env.podManager.GetPodsAndMirrorPods()
	if len(allPods) != benchTotalPods+benchStaticPods || len(allMirror) != benchStaticPods || len(orphans) != 0 {
		tb.Fatalf("unexpected GetPodsAndMirrorPods: %d, %d, %d", len(allPods), len(allMirror), len(orphans))
	}
	if sm := env.podManager.GetStaticPodToMirrorPodMap(); len(sm) != benchStaticPods {
		tb.Fatalf("unexpected GetStaticPodToMirrorPodMap len: %d", len(sm))
	}
	p2m, m2p := env.podManager.GetUIDTranslations()
	if len(p2m) != benchStaticPods || len(m2p) != benchStaticPods {
		tb.Fatalf("unexpected GetUIDTranslations lens: %d, %d", len(p2m), len(m2p))
	}
	for _, p := range mgrPods {
		if _, ok := env.podManager.GetPodByName(p.Namespace, p.Name); !ok {
			tb.Fatalf("GetPodByName missing %s/%s", p.Namespace, p.Name)
		}
		if _, ok := env.podManager.GetPodByUID(p.UID); !ok {
			tb.Fatalf("GetPodByUID missing %s", p.UID)
		}
	}
	for i := 0; i < benchStaticPods; i++ {
		resolved := env.podManager.TranslatePodUID(env.mirrorPods[i].UID)
		if types.UID(resolved) != env.staticPods[i].UID {
			tb.Fatalf("TranslatePodUID mismatch: got %s want %s", resolved, env.staticPods[i].UID)
		}
		if _, ok := env.podManager.GetPodByMirrorPod(env.mirrorPods[i]); !ok {
			tb.Fatalf("GetPodByMirrorPod missing %s", env.mirrorPods[i].Name)
		}
		if _, ok := env.podManager.GetMirrorPodByPod(env.staticPods[i]); !ok {
			tb.Fatalf("GetMirrorPodByPod missing %s", env.staticPods[i].Name)
		}
	}

	// 3. client-go/tools/cache SharedIndexInformer (RealFIFO + PopBatch + Transaction) + GenericLister + Indexer + DeltaFIFO
	// 24 Modified + 1 Added + 1 Deleted = 26 events * 2 handlers = 52 callbacks
	env.handlerPending.Store(26 * 2)
	for i := 0; i < 24; i++ {
		env.informerLW.watcher.ch <- watch.Event{Type: watch.Modified, Object: infPods[i]}
	}
	env.informerLW.watcher.ch <- watch.Event{Type: watch.Added, Object: env.ephemeralInfPods[0]}
	env.informerLW.watcher.ch <- watch.Event{Type: watch.Deleted, Object: env.ephemeralInfPods[0]}
	env.informerLW.watcher.ch <- watch.Event{Type: watch.Bookmark, Object: infBookmark}

	<-env.handlerDoneCh

	indexer := env.informer.GetIndexer()
	nodeItems, err := indexer.ByIndex(benchNodeIndex, benchNodeName)
	if err != nil || len(nodeItems) != benchTotalPods {
		tb.Fatalf("ByIndex(node): len=%d err=%v", len(nodeItems), err)
	}
	allListed, err := env.genericLister.List(labels.Everything())
	if err != nil || len(allListed) != benchTotalPods {
		tb.Fatalf("genericLister.List: len=%d err=%v", len(allListed), err)
	}
	for _, ns := range env.namespaces {
		nsItems, err := env.genericLister.ByNamespace(ns).List(labels.Everything())
		if err != nil || len(nsItems) != benchPodsPerNS {
			tb.Fatalf("ByNamespace(%s).List: len=%d err=%v", ns, len(nsItems), err)
		}
		keys, err := indexer.IndexKeys(cache.NamespaceIndex, ns)
		if err != nil || len(keys) != benchPodsPerNS {
			tb.Fatalf("IndexKeys(%s): len=%d err=%v", ns, len(keys), err)
		}
	}
	for i := 0; i < 24; i++ {
		p := infPods[i]
		if _, err := env.genericLister.ByNamespace(p.Namespace).Get(p.Name); err != nil {
			tb.Fatalf("ByNamespace(%s).Get(%s): %v", p.Namespace, p.Name, err)
		}
	}

	// Direct DeltaFIFO batch update/add/delete/pop cycle
	for i := 0; i < 12; i++ {
		p := infPods[i]
		_ = env.deltaKnownStore.Update(p)
		_ = env.deltaFIFO.Update(p)
	}
	_ = env.deltaFIFO.Add(env.ephemeralInfPods[1])
	_ = env.deltaFIFO.Delete(env.ephemeralInfPods[1])
	_ = env.deltaKnownStore.ListKeys()
	for k := 0; k < 13; k++ {
		_, err := env.deltaFIFO.Pop(func(_ interface{}, _ bool) error {
			return nil
		})
		if err != nil {
			tb.Fatalf("deltaFIFO.Pop: %v", err)
		}
	}
}

func BenchmarkWatchCachesInformersAndManagers(b *testing.B) {
	env := newWatchCachesBenchEnv(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		env.runWave(b, i)
	}
}

func TestWatchCachesInformersAndManagersEquivalence(t *testing.T) {
	env := newWatchCachesBenchEnv(t)

	// 1. Verify Immutable Secret/ConfigMap stopped their watches after initial Get, while mutable ones remain open
	for idx, key := range env.secretKeys {
		wantWatches := 1
		if idx%benchObjectsPerNS == 2 {
			wantWatches = 0
		}
		err := wait.PollUntilContextTimeout(env.ctx, 5*time.Millisecond, time.Second, true, func(_ context.Context) (bool, error) {
			return env.kubeClient.core.activeSecretWatchCount(key.ns, key.name) == wantWatches &&
				env.kubeClient.core.activeConfigMapWatchCount(env.cmKeys[idx].ns, env.cmKeys[idx].name) == wantWatches, nil
		})
		if err != nil {
			t.Fatalf("idx %d (%v) expected %d active watches, got secret=%d cm=%d",
				idx, key, wantWatches,
				env.kubeClient.core.activeSecretWatchCount(key.ns, key.name),
				env.kubeClient.core.activeConfigMapWatchCount(env.cmKeys[idx].ns, env.cmKeys[idx].name))
		}
	}

	// 2. Verify unregistered vs NotFound error contracts
	if _, err := env.watchingSecretMgr.GetSecret("ns-00", "unregistered-sec"); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("expected not registered error, got: %v", err)
	}
	missingPod := buildWorkloadPod("ns-00", "pod-missing-obj", "uid-missing-obj", "1", "nonexistent-sec", "nonexistent-sec", "nonexistent-cm", "nonexistent-cm", "v1", "ok")
	env.watchingSecretMgr.RegisterPod(missingPod)
	env.watchingConfigMapMgr.RegisterPod(missingPod)
	if _, err := env.watchingSecretMgr.GetSecret("ns-00", "nonexistent-sec"); !apierrors.IsNotFound(err) {
		t.Fatalf("expected IsNotFound for missing secret, got: %v", err)
	}
	if _, err := env.watchingConfigMapMgr.GetConfigMap("ns-00", "nonexistent-cm"); !apierrors.IsNotFound(err) {
		t.Fatalf("expected IsNotFound for missing configmap, got: %v", err)
	}
	env.watchingSecretMgr.UnregisterPod(missingPod)
	env.watchingConfigMapMgr.UnregisterPod(missingPod)

	// 3. Run multiple waves and verify live watch payload propagation & manager/informer parity
	for w := 0; w < 4; w++ {
		env.runWave(t, w)
	}

	// Verify live watch update delivery on a mutable Secret & ConfigMap
	targetSecKey := env.secretKeys[0]
	updatedSec := env.secretsRevB[0].DeepCopy()
	updatedSec.ResourceVersion = "999"
	updatedSec.Data["password"] = []byte("rotated-live-secret")
	env.kubeClient.core.setSecret(updatedSec, true)

	err := wait.PollUntilContextTimeout(env.ctx, 5*time.Millisecond, 2*time.Second, true, func(_ context.Context) (bool, error) {
		got, err := env.watchingSecretMgr.GetSecret(targetSecKey.ns, targetSecKey.name)
		if err != nil {
			return false, err
		}
		return string(got.Data["password"]) == "rotated-live-secret", nil
	})
	if err != nil {
		t.Fatalf("live secret watch propagation failed: %v", err)
	}

	targetCMKey := env.cmKeys[0]
	updatedCM := env.cmsRevB[0].DeepCopy()
	updatedCM.ResourceVersion = "999"
	updatedCM.Data["log-level"] = "trace-live-cm"
	env.kubeClient.core.setConfigMap(updatedCM, true)

	err = wait.PollUntilContextTimeout(env.ctx, 5*time.Millisecond, 2*time.Second, true, func(_ context.Context) (bool, error) {
		got, err := env.watchingConfigMapMgr.GetConfigMap(targetCMKey.ns, targetCMKey.name)
		if err != nil {
			return false, err
		}
		return got.Data["log-level"] == "trace-live-cm", nil
	})
	if err != nil {
		t.Fatalf("live configmap watch propagation failed: %v", err)
	}

	// 4. Verify PodConfig local annotations & pod.Manager mirror/static state
	p0, ok := env.podManager.GetPodByName("ns-00", "pod-00")
	if !ok {
		t.Fatal("missing ns-00/pod-00 in podManager")
	}
	if p0.Annotations[kubetypes.ConfigSourceAnnotationKey] != kubetypes.ApiserverSource {
		t.Fatalf("missing ConfigSourceAnnotationKey: %v", p0.Annotations)
	}
	if p0.Annotations[kubetypes.ConfigFirstSeenAnnotationKey] == "" {
		t.Fatalf("missing ConfigFirstSeenAnnotationKey: %v", p0.Annotations)
	}

	// 5. Concurrent -race stress across readers and writers
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				keyS := env.secretKeys[(worker+j)%len(env.secretKeys)]
				_, _ = env.watchingSecretMgr.GetSecret(keyS.ns, keyS.name)
				_, _ = env.cachingSecretMgr.GetSecret(keyS.ns, keyS.name)
				keyC := env.cmKeys[(worker+j)%len(env.cmKeys)]
				_, _ = env.watchingConfigMapMgr.GetConfigMap(keyC.ns, keyC.name)
				_, _ = env.cachingConfigMapMgr.GetConfigMap(keyC.ns, keyC.name)
				ns := env.namespaces[(worker+j)%len(env.namespaces)]
				_, _ = env.genericLister.ByNamespace(ns).List(labels.Everything())
				_, _ = env.podManager.GetPodByName(ns, "pod-00")
				_ = env.podManager.GetStaticPodToMirrorPodMap()
			}
		}(g)
	}
	wg.Wait()
}
