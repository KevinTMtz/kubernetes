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

package status

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	clientset "k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/workqueue"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/ktesting"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/pleg"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	statustest "k8s.io/kubernetes/pkg/kubelet/status/testing"
	kubeletutil "k8s.io/kubernetes/pkg/kubelet/util"
	"k8s.io/kubernetes/pkg/kubelet/util/queue"
	testingclock "k8s.io/utils/clock/testing"
)

const (
	benchNumPods       = 60
	benchChurnPods     = 4
	benchWakeupPods    = 12
	benchNoopSyncPods  = 20
	benchDriftedPods   = 2
	benchRuntimeName   = "containerd"
	benchCustomCondKey = v1.PodConditionType("cloud.google.com/load-balancer-ready")
)

type benchContainerRuntime struct {
	kubecontainer.Runtime
	pods       []*kubecontainer.Pod
	podByID    map[types.UID]*kubecontainer.Pod
	statusByID map[types.UID]*kubecontainer.PodStatus
}

func (r *benchContainerRuntime) Type() string {
	return benchRuntimeName
}

func (r *benchContainerRuntime) GetPods(_ context.Context, _ bool) ([]*kubecontainer.Pod, error) {
	return r.pods, nil
}

func (r *benchContainerRuntime) GetPod(_ context.Context, uid types.UID) (*kubecontainer.Pod, error) {
	if p, ok := r.podByID[uid]; ok {
		return p, nil
	}
	return nil, kubecontainer.ErrPodNotFound
}

func (r *benchContainerRuntime) GetPodStatus(_ context.Context, pod *kubecontainer.Pod) (*kubecontainer.PodStatus, error) {
	if st, ok := r.statusByID[pod.ID]; ok {
		return st, nil
	}
	return &kubecontainer.PodStatus{ID: pod.ID}, nil
}

type benchKubeClient struct {
	clientset.Interface
	coreV1 benchCoreV1
}

func (c *benchKubeClient) CoreV1() corev1client.CoreV1Interface {
	return &c.coreV1
}

type benchCoreV1 struct {
	corev1client.CoreV1Interface
	pods benchPodsClient
}

func (c *benchCoreV1) Pods(_ string) corev1client.PodInterface {
	return &c.pods
}

type benchPodsClient struct {
	corev1client.PodInterface
	byName            map[string]*v1.Pod
	expectedMerged    map[string]v1.PodStatus
	rvRing            [8]string
	rvIdx             int
	patchCalls        int
	strictPatchVerify bool
}

func (p *benchPodsClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*v1.Pod, error) {
	pod, ok := p.byName[name]
	if !ok {
		return nil, fmt.Errorf("pod %q not found", name)
	}
	return pod, nil
}

func (p *benchPodsClient) Patch(_ context.Context, name string, pt types.PatchType, data []byte, _ metav1.PatchOptions, _ ...string) (*v1.Pod, error) {
	if pt != types.StrategicMergePatchType {
		return nil, fmt.Errorf("unexpected patch type %q", pt)
	}
	if len(data) < 2 || data[0] != '{' || data[len(data)-1] != '}' {
		return nil, fmt.Errorf("invalid strategic merge patch payload: %q", string(data))
	}
	pod, ok := p.byName[name]
	if !ok {
		return nil, fmt.Errorf("pod %q not found", name)
	}
	p.patchCalls++

	if p.strictPatchVerify {
		origBytes, err := json.Marshal(pod)
		if err != nil {
			return nil, err
		}
		patchedBytes, err := strategicpatch.StrategicMergePatch(origBytes, data, v1.Pod{})
		if err != nil {
			return nil, err
		}
		var updated v1.Pod
		if err := json.Unmarshal(patchedBytes, &updated); err != nil {
			return nil, err
		}
		updated.ResourceVersion = p.rvRing[p.rvIdx&7]
		p.rvIdx++
		*pod = updated
		return pod, nil
	}

	if merged, hasExpected := p.expectedMerged[name]; hasExpected {
		pod.Status = merged
	}
	pod.ResourceVersion = p.rvRing[p.rvIdx&7]
	p.rvIdx++
	return pod, nil
}

type syncLoopHarness struct {
	fakeClock       *testingclock.FakeClock
	kubeletQueue    queue.WorkQueue
	controllerQueue *workqueue.Typed[types.UID]
	podCache        kubecontainer.Cache
	runtime         *benchContainerRuntime
	genericPLEG     *pleg.GenericPLEG
	plegCh          <-chan *pleg.PodLifecycleEvent
	podManager      kubepod.Manager
	kubeClient      *benchKubeClient
	statusMgr       *manager

	pods             []*v1.Pod
	uids             []types.UID
	primaryCIDs      []kubecontainer.ContainerID
	runtimePodStateA []*kubecontainer.Pod
	runtimePodStateB []*kubecontainer.Pod
	runtimeStatusA   []*kubecontainer.PodStatus
	runtimeStatusB   []*kubecontainer.PodStatus

	// Pre-built distinct input PodStatus structs (with sub-second nanosecond timestamps
	// and independent backing slices so pointer-aliasing shortcuts cannot falsely match).
	steadyStatusInputA []v1.PodStatus
	steadyStatusInputB []v1.PodStatus
	churnStatusStep1A  []v1.PodStatus
	churnStatusStep1B  []v1.PodStatus
	driftedAPIStatus   []v1.PodStatus
	syncedAPIStatusA   []v1.PodStatus
	syncedAPIStatusB   []v1.PodStatus
}

func newSyncLoopHarness(tb testing.TB, strictPatchVerify bool) *syncLoopHarness {
	baseTime := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	unnormalizedTime := metav1.NewTime(baseTime.Add(456789123 * time.Nanosecond))
	normalizedTime := unnormalizedTime.Rfc3339Copy()
	transitionTime := metav1.NewTime(baseTime.Add(-5 * time.Minute)).Rfc3339Copy()

	fakeClock := testingclock.NewFakeClock(baseTime)
	kubeletQueue := queue.NewBasicWorkQueue(fakeClock)
	controllerQueue := workqueue.NewTypedWithConfig(workqueue.TypedQueueConfig[types.UID]{
		Clock: fakeClock,
	})

	rt := &benchContainerRuntime{
		pods:       make([]*kubecontainer.Pod, benchNumPods),
		podByID:    make(map[types.UID]*kubecontainer.Pod, benchNumPods),
		statusByID: make(map[types.UID]*kubecontainer.PodStatus, benchNumPods),
	}
	podCache := kubecontainer.NewCache()
	eventCh := make(chan *pleg.PodLifecycleEvent, benchNumPods*8)
	genericPLEG := pleg.NewGenericPLEG(
		rt,
		eventCh,
		&pleg.RelistDuration{RelistPeriod: time.Second, RelistThreshold: 3 * time.Minute},
		podCache,
		fakeClock,
	).(*pleg.GenericPLEG)

	podManager := kubepod.NewBasicPodManager()
	kubeClient := &benchKubeClient{
		coreV1: benchCoreV1{
			pods: benchPodsClient{
				byName:            make(map[string]*v1.Pod, benchNumPods),
				expectedMerged:    make(map[string]v1.PodStatus, benchNumPods),
				rvRing:            [8]string{"101", "102", "103", "104", "105", "106", "107", "108"},
				strictPatchVerify: strictPatchVerify,
			},
		},
	}

	statusMgr := NewManager(
		kubeClient,
		podManager,
		&statustest.FakePodDeletionSafetyProvider{},
		kubeletutil.NewPodStartupLatencyTracker(),
	).(*manager)

	h := &syncLoopHarness{
		fakeClock:          fakeClock,
		kubeletQueue:       kubeletQueue,
		controllerQueue:    controllerQueue,
		podCache:           podCache,
		runtime:            rt,
		genericPLEG:        genericPLEG,
		plegCh:             genericPLEG.Watch(),
		podManager:         podManager,
		kubeClient:         kubeClient,
		statusMgr:          statusMgr,
		pods:               make([]*v1.Pod, benchNumPods),
		uids:               make([]types.UID, benchNumPods),
		primaryCIDs:        make([]kubecontainer.ContainerID, benchNumPods),
		runtimePodStateA:   make([]*kubecontainer.Pod, benchNumPods),
		runtimePodStateB:   make([]*kubecontainer.Pod, benchNumPods),
		runtimeStatusA:     make([]*kubecontainer.PodStatus, benchNumPods),
		runtimeStatusB:     make([]*kubecontainer.PodStatus, benchNumPods),
		steadyStatusInputA: make([]v1.PodStatus, benchNumPods),
		steadyStatusInputB: make([]v1.PodStatus, benchNumPods),
		churnStatusStep1A:  make([]v1.PodStatus, benchChurnPods),
		churnStatusStep1B:  make([]v1.PodStatus, benchChurnPods),
		driftedAPIStatus:   make([]v1.PodStatus, benchDriftedPods),
		syncedAPIStatusA:   make([]v1.PodStatus, benchNumPods),
		syncedAPIStatusB:   make([]v1.PodStatus, benchNumPods),
	}

	ctx := context.Background()
	logger := klog.FromContext(ctx)
	if strictPatchVerify {
		logger, ctx = ktesting.NewTestContext(tb)
	}
	startedTrue := true

	for i := 0; i < benchNumPods; i++ {
		uid := types.UID(fmt.Sprintf("syncloop-pod-uid-%03d", i))
		name := fmt.Sprintf("syncloop-pod-%03d", i)
		ip := fmt.Sprintf("10.244.2.%d", (i%200)+10)
		initCID := kubecontainer.ContainerID{Type: benchRuntimeName, ID: fmt.Sprintf("init-%03d", i)}
		app0CID := kubecontainer.ContainerID{Type: benchRuntimeName, ID: fmt.Sprintf("app0-%03d", i)}
		app1CIDA := kubecontainer.ContainerID{Type: benchRuntimeName, ID: fmt.Sprintf("app1-%03d-a", i)}
		app1CIDB := kubecontainer.ContainerID{Type: benchRuntimeName, ID: fmt.Sprintf("app1-%03d-b", i)}
		sbCID := kubecontainer.ContainerID{Type: benchRuntimeName, ID: fmt.Sprintf("sb-%03d", i)}

		h.uids[i] = uid
		h.primaryCIDs[i] = app0CID

		claimName := fmt.Sprintf("claim-%03d", i)
		apiPod := &v1.Pod{
			TypeMeta: metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
			ObjectMeta: metav1.ObjectMeta{
				UID:             uid,
				Name:            name,
				Namespace:       "default",
				Generation:      3,
				ResourceVersion: "100",
			},
			Spec: v1.PodSpec{
				InitContainers: []v1.Container{{Name: "init-setup", Image: "registry.k8s.io/busybox:1.36"}},
				Containers: []v1.Container{
					{Name: "app-primary", Image: "registry.k8s.io/pause:3.10"},
					{Name: "app-sidecar", Image: "registry.k8s.io/pause:3.10"},
				},
				ReadinessGates: []v1.PodReadinessGate{{ConditionType: benchCustomCondKey}},
				RestartPolicy:  v1.RestartPolicyAlways,
			},
		}
		h.pods[i] = apiPod

		makeRuntimePod := func(sidecarID kubecontainer.ContainerID) *kubecontainer.Pod {
			return &kubecontainer.Pod{
				ID:        uid,
				Name:      name,
				Namespace: "default",
				Timestamp: baseTime,
				Containers: []*kubecontainer.Container{
					{ID: initCID, Name: "init-setup", State: kubecontainer.ContainerStateExited},
					{ID: app0CID, Name: "app-primary", State: kubecontainer.ContainerStateRunning},
					{ID: sidecarID, Name: "app-sidecar", State: kubecontainer.ContainerStateRunning},
				},
				Sandboxes: []*kubecontainer.Container{
					{ID: sbCID, Name: "sandbox", State: kubecontainer.ContainerStateRunning},
				},
			}
		}
		makeRuntimeStatus := func(sidecarID kubecontainer.ContainerID) *kubecontainer.PodStatus {
			return &kubecontainer.PodStatus{
				ID:        uid,
				Name:      name,
				Namespace: "default",
				IPs:       []string{ip},
				SandboxStatuses: []*runtimeapi.PodSandboxStatus{
					{Id: sbCID.ID, State: runtimeapi.PodSandboxState_SANDBOX_READY},
				},
				ContainerStatuses: []*kubecontainer.Status{
					{ID: initCID, Name: "init-setup", State: kubecontainer.ContainerStateExited, ExitCode: 0},
					{ID: app0CID, Name: "app-primary", State: kubecontainer.ContainerStateRunning},
					{ID: sidecarID, Name: "app-sidecar", State: kubecontainer.ContainerStateRunning},
				},
			}
		}

		h.runtimePodStateA[i] = makeRuntimePod(app1CIDA)
		h.runtimePodStateB[i] = makeRuntimePod(app1CIDB)
		h.runtimeStatusA[i] = makeRuntimeStatus(app1CIDA)
		h.runtimeStatusB[i] = makeRuntimeStatus(app1CIDB)

		rt.pods[i] = h.runtimePodStateA[i]
		rt.podByID[uid] = h.runtimePodStateA[i]
		rt.statusByID[uid] = h.runtimeStatusA[i]

		makeKubeletPodStatus := func(sidecarID kubecontainer.ContainerID, restartCount int32, ts metav1.Time) v1.PodStatus {
			stCopy := ts
			return v1.PodStatus{
				Phase:              v1.PodRunning,
				StartTime:          &stCopy,
				PodIP:              ip,
				PodIPs:             []v1.PodIP{{IP: ip}},
				HostIP:             "192.168.1.10",
				HostIPs:            []v1.HostIP{{IP: "192.168.1.10"}},
				QOSClass:           v1.PodQOSBestEffort,
				ObservedGeneration: 3,
				Conditions: []v1.PodCondition{
					{Type: benchCustomCondKey, ObservedGeneration: 3, Status: v1.ConditionTrue, LastTransitionTime: transitionTime},
					{Type: v1.PodScheduled, ObservedGeneration: 3, Status: v1.ConditionTrue, LastTransitionTime: transitionTime},
					{Type: v1.PodReadyToStartContainers, ObservedGeneration: 3, Status: v1.ConditionTrue, LastTransitionTime: transitionTime},
					{Type: v1.PodInitialized, ObservedGeneration: 3, Status: v1.ConditionTrue, LastTransitionTime: transitionTime},
					{Type: v1.PodReady, ObservedGeneration: 3, Status: v1.ConditionTrue, LastTransitionTime: transitionTime},
					{Type: v1.ContainersReady, ObservedGeneration: 3, Status: v1.ConditionTrue, LastTransitionTime: transitionTime},
				},
				InitContainerStatuses: []v1.ContainerStatus{
					{
						Name:        "init-setup",
						Image:       "registry.k8s.io/busybox:1.36",
						ContainerID: initCID.String(),
						Ready:       true,
						State: v1.ContainerState{
							Terminated: &v1.ContainerStateTerminated{
								ExitCode:   0,
								Reason:     "Completed",
								StartedAt:  transitionTime,
								FinishedAt: transitionTime,
							},
						},
					},
				},
				ContainerStatuses: []v1.ContainerStatus{
					{
						Name:        "app-primary",
						Image:       "registry.k8s.io/pause:3.10",
						ContainerID: app0CID.String(),
						Ready:       true,
						Started:     &startedTrue,
						State:       v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: ts}},
					},
					{
						Name:         "app-sidecar",
						Image:        "registry.k8s.io/pause:3.10",
						ContainerID:  sidecarID.String(),
						Ready:        true,
						Started:      &startedTrue,
						RestartCount: restartCount,
						State:        v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: ts}},
					},
				},
			}
		}

		// Two independent deep-copied inputs with un-normalized nanosecond timestamps.
		stSteadyA := makeKubeletPodStatus(app1CIDA, 0, unnormalizedTime)
		stSteadyB := makeKubeletPodStatus(app1CIDA, 0, unnormalizedTime)
		h.steadyStatusInputA[i] = *stSteadyA.DeepCopy()
		h.steadyStatusInputB[i] = *stSteadyB.DeepCopy()

		if i < benchChurnPods {
			stChurnA := makeKubeletPodStatus(app1CIDB, 1, unnormalizedTime)
			stChurnB := makeKubeletPodStatus(app1CIDA, 2, unnormalizedTime)
			h.churnStatusStep1A[i] = *stChurnA.DeepCopy()
			h.churnStatusStep1B[i] = *stChurnB.DeepCopy()
		}

		makeMergedAPIStatus := func(sidecarID kubecontainer.ContainerID, restartCount int32) v1.PodStatus {
			base := makeKubeletPodStatus(sidecarID, restartCount, normalizedTime)
			base.ResourceClaimStatuses = []v1.PodResourceClaimStatus{
				{Name: "gpu-claim", ResourceClaimName: &claimName},
			}
			return *base.DeepCopy()
		}

		if i < benchChurnPods {
			h.syncedAPIStatusB[i] = makeMergedAPIStatus(app1CIDB, 1)
			h.syncedAPIStatusA[i] = makeMergedAPIStatus(app1CIDA, 2)
		} else {
			h.syncedAPIStatusA[i] = makeMergedAPIStatus(app1CIDA, 0)
			h.syncedAPIStatusB[i] = makeMergedAPIStatus(app1CIDA, 0)
		}

		// Seed initial apiserver Pod (including non-kubelet conditions & ResourceClaimStatuses)
		apiPod.Status = makeMergedAPIStatus(app1CIDA, 0)
		kubeClient.coreV1.pods.byName[name] = apiPod
		kubeClient.coreV1.pods.expectedMerged[name] = apiPod.Status
		podManager.AddPod(apiPod)

		// Seed initial Kubelet status cache and sync to apiserver
		statusMgr.SetPodStatus(logger, apiPod, makeKubeletPodStatus(app1CIDA, 0, unnormalizedTime))
	}

	// Prime initial PLEG relist and flush initial status updates so wave 0 starts from steady state.
	genericPLEG.Relist(ctx)
	drainPLEGEvents(h.plegCh)
	statusMgr.consumeUpdates(ctx)

	// Prepare drifted status snapshots for two steady-state pods (pods[benchNumPods-2] and pods[benchNumPods-1])
	for d := 0; d < benchDriftedPods; d++ {
		idx := benchNumPods - benchDriftedPods + d
		drifted := *h.syncedAPIStatusA[idx].DeepCopy()
		for cIdx := range drifted.Conditions {
			if drifted.Conditions[cIdx].Type == v1.ContainersReady {
				drifted.Conditions[cIdx].Status = v1.ConditionFalse
				drifted.Conditions[cIdx].Reason = "StaleAPIServerView"
			}
		}
		h.driftedAPIStatus[d] = drifted
	}

	return h
}

func drainPLEGEvents(ch <-chan *pleg.PodLifecycleEvent) int {
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			return n
		}
	}
}

func (h *syncLoopHarness) runSyncLoopWave(ctx context.Context, logger klog.Logger, wave int) (readyCount, plegEvents, updatedCount, reconciledCount int) {
	oddWave := (wave & 1) == 1
	h.kubeClient.coreV1.pods.patchCalls = 0

	// 1. Queued pod wakeup burst: 4 back-to-back events for 12 pods -> coalesced to 12 unique pod keys
	// across both Kubelet's delayed WorkQueue and client-go's deduplicating Typed workqueue.
	for p := 0; p < benchWakeupPods; p++ {
		uid := h.uids[p]
		h.kubeletQueue.Enqueue(uid, 50*time.Millisecond)
		h.kubeletQueue.Enqueue(uid, 20*time.Millisecond)
		h.kubeletQueue.Enqueue(uid, 10*time.Millisecond)
		h.kubeletQueue.Enqueue(uid, 0)

		h.controllerQueue.Add(uid)
		h.controllerQueue.Add(uid)
		h.controllerQueue.Add(uid)
		h.controllerQueue.Add(uid)
	}
	h.fakeClock.Step(100 * time.Millisecond)

	readyItems := h.kubeletQueue.GetWork()
	readyCount = len(readyItems)

	for i := 0; i < benchWakeupPods; i++ {
		item, shutdown := h.controllerQueue.Get()
		if shutdown {
			break
		}
		// Simulate a concurrent re-add while the item is in `processing` for the first 4 churning pods,
		// followed by immediate coalesced completion.
		if i < benchChurnPods {
			h.controllerQueue.Add(item)
		}
		h.controllerQueue.Done(item)
	}
	for i := 0; i < benchChurnPods; i++ {
		item, shutdown := h.controllerQueue.Get()
		if shutdown {
			break
		}
		h.controllerQueue.Done(item)
	}

	// 2. PLEG Relist across 60 pods (56 unchanged pods + 4 churning pods whose sidecar recreated),
	// followed by pod worker cache queries (`podCache.GetNewerThan`).
	lastSyncTime := h.fakeClock.Now()
	h.fakeClock.Step(time.Second)
	relistNow := h.fakeClock.Now()
	for p := 0; p < benchChurnPods; p++ {
		uid := h.uids[p]
		if !oddWave {
			h.runtimePodStateB[p].Timestamp = relistNow
			h.runtime.pods[p] = h.runtimePodStateB[p]
			h.runtime.podByID[uid] = h.runtimePodStateB[p]
			h.runtime.statusByID[uid] = h.runtimeStatusB[p]
		} else {
			h.runtimePodStateA[p].Timestamp = relistNow
			h.runtime.pods[p] = h.runtimePodStateA[p]
			h.runtime.podByID[uid] = h.runtimePodStateA[p]
			h.runtime.statusByID[uid] = h.runtimeStatusA[p]
		}
	}
	h.genericPLEG.Relist(ctx)
	plegEvents = drainPLEGEvents(h.plegCh)

	for p := 0; p < benchWakeupPods; p++ {
		if _, err := h.podCache.GetNewerThan(ctx, h.uids[p], lastSyncTime); err != nil {
			panic(err)
		}
	}

	// 3. Back-to-back queued status & readiness mutations before syncBatch(ctx, false) drains podStatusChannel:
	// - 4 churning pods receive 3 rapid updates each (SetPodStatus -> SetContainerReadiness(false) -> SetContainerReadiness(true)),
	//   coalescing 12 local mutations into 4 API server PatchPodStatus operations.
	// - 20 steady-state pods receive periodic worker SetPodStatus calls with unchanged semantic state
	//   (distinct backing slices + sub-second timestamps that normalize to identical RFC3339 values).
	for p := 0; p < benchChurnPods; p++ {
		pod := h.pods[p]
		if !oddWave {
			h.kubeClient.coreV1.pods.expectedMerged[pod.Name] = h.syncedAPIStatusB[p]
			h.statusMgr.SetPodStatus(logger, pod, h.churnStatusStep1A[p])
		} else {
			h.kubeClient.coreV1.pods.expectedMerged[pod.Name] = h.syncedAPIStatusA[p]
			h.statusMgr.SetPodStatus(logger, pod, h.churnStatusStep1B[p])
		}
		h.statusMgr.SetContainerReadiness(logger, h.uids[p], h.primaryCIDs[p], false)
		h.statusMgr.SetContainerReadiness(logger, h.uids[p], h.primaryCIDs[p], true)
	}

	steadyInputs := h.steadyStatusInputA
	if oddWave {
		steadyInputs = h.steadyStatusInputB
	}
	for p := benchChurnPods; p < benchChurnPods+benchNoopSyncPods; p++ {
		h.statusMgr.SetPodStatus(logger, h.pods[p], steadyInputs[p])
	}

	updatedCount = h.statusMgr.consumeUpdates(ctx)

	// 4. Periodic bulk reconciliation pass (`syncBatch(ctx, true)`) across all 60 pods:
	// 58 pods match apiserver status; 2 pods have apiserver status drift and must be reconciled.
	for d := 0; d < benchDriftedPods; d++ {
		idx := benchNumPods - benchDriftedPods + d
		pod := h.pods[idx]
		pod.Status = h.driftedAPIStatus[d]
		h.kubeClient.coreV1.pods.expectedMerged[pod.Name] = h.syncedAPIStatusA[idx]
	}

	reconciledCount = h.statusMgr.syncBatch(ctx, true)
	return readyCount, plegEvents, updatedCount, reconciledCount
}

func TestSyncLoopReconciliationEquivalence(t *testing.T) {
	logger, ctx := ktesting.NewTestContext(t)
	h := newSyncLoopHarness(t, true)
	defer h.controllerQueue.ShutDown()

	// Verify nil-vs-empty slice equivalence matches apiequality.Semantic.DeepEqual.
	nilVsEmptyA := *h.steadyStatusInputA[benchChurnPods].DeepCopy()
	nilVsEmptyB := *h.steadyStatusInputA[benchChurnPods].DeepCopy()
	nilVsEmptyA.InitContainerStatuses = nil
	nilVsEmptyB.InitContainerStatuses = []v1.ContainerStatus{}
	nilVsEmptyA.EphemeralContainerStatuses = nil
	nilVsEmptyB.EphemeralContainerStatuses = []v1.ContainerStatus{}
	if !isPodStatusByKubeletEqual(&nilVsEmptyA, &nilVsEmptyB) {
		t.Fatalf("expected isPodStatusByKubeletEqual=true when comparing nil vs empty container status slices")
	}

	// Verify caller-slice and timestamp immutability on SetPodStatus and needsReconcile.
	callerPod := h.pods[benchChurnPods]
	origNano := h.steadyStatusInputA[benchChurnPods].StartTime.Nanosecond()
	if origNano == 0 {
		t.Fatalf("expected non-zero nanosecond timestamp in test fixture")
	}
	unsortedInput := *h.steadyStatusInputA[benchChurnPods].DeepCopy()
	unsortedInput.ContainerStatuses[0], unsortedInput.ContainerStatuses[1] = unsortedInput.ContainerStatuses[1], unsortedInput.ContainerStatuses[0]
	firstNameBefore := unsortedInput.ContainerStatuses[0].Name

	_, changed := h.statusMgr.SetPodStatus(logger, callerPod, unsortedInput)
	if changed {
		t.Fatalf("expected unchanged=false for semantically identical status with unsorted containers and sub-second timestamps")
	}
	if unsortedInput.StartTime.Nanosecond() != origNano {
		t.Fatalf("SetPodStatus mutated caller StartTime pointer in place: got %d ns, want %d ns", unsortedInput.StartTime.Nanosecond(), origNano)
	}
	if unsortedInput.ContainerStatuses[0].Name != firstNameBefore {
		t.Fatalf("SetPodStatus mutated caller ContainerStatuses backing slice order in place")
	}

	// Verify needsReconcile does not mutate podManager's Pod.Status in place even if Pod.Status has sub-second timestamps.
	apiPodWithNano := callerPod.DeepCopy()
	nanoStart := metav1.NewTime(callerPod.Status.StartTime.Time.Add(987654321 * time.Nanosecond))
	apiPodWithNano.Status.StartTime = &nanoStart
	apiPodWithNano.Status.ContainerStatuses[0], apiPodWithNano.Status.ContainerStatuses[1] = apiPodWithNano.Status.ContainerStatuses[1], apiPodWithNano.Status.ContainerStatuses[0]
	h.podManager.UpdatePod(apiPodWithNano)

	if h.statusMgr.needsReconcile(logger, callerPod.UID, h.statusMgr.podStatuses[callerPod.UID].status) {
		t.Fatalf("expected needsReconcile=false when apiserver pod status differs only in sub-second precision and slice order")
	}
	if apiPodWithNano.Status.StartTime.Nanosecond() != 987654321 {
		t.Fatalf("needsReconcile mutated podManager Pod.Status.StartTime in place")
	}
	if apiPodWithNano.Status.ContainerStatuses[0].Name != "app-sidecar" {
		t.Fatalf("needsReconcile mutated podManager Pod.Status.ContainerStatuses order in place")
	}
	h.podManager.UpdatePod(callerPod)

	// Run 4 full waves with strict StrategicMergePatch verification enabled and check exact converged state.
	const expectedPLEGEventsPerChurnPod = 3 // ContainerDied + ContainerRemoved + ContainerStarted
	for wave := 0; wave < 4; wave++ {
		readyCount, plegEvents, updatedCount, reconciledCount := h.runSyncLoopWave(ctx, logger, wave)
		if readyCount != benchWakeupPods {
			t.Fatalf("wave %d: expected %d coalesced workqueue items, got %d", wave, benchWakeupPods, readyCount)
		}
		if plegEvents != benchChurnPods*expectedPLEGEventsPerChurnPod {
			t.Fatalf("wave %d: expected %d PLEG events (%d churning pods), got %d", wave, benchChurnPods*expectedPLEGEventsPerChurnPod, benchChurnPods, plegEvents)
		}
		if updatedCount != benchChurnPods {
			t.Fatalf("wave %d: expected %d coalesced status updates, got %d", wave, benchChurnPods, updatedCount)
		}
		if reconciledCount != benchDriftedPods {
			t.Fatalf("wave %d: expected %d reconciled drifted pods, got %d", wave, benchDriftedPods, reconciledCount)
		}
		if h.kubeClient.coreV1.pods.patchCalls != benchChurnPods+benchDriftedPods {
			t.Fatalf("wave %d: expected %d API server patches, got %d", wave, benchChurnPods+benchDriftedPods, h.kubeClient.coreV1.pods.patchCalls)
		}

		// Verify all 60 pods converged to Ready=True and preserved non-kubelet conditions & ResourceClaimStatuses.
		oddWave := (wave & 1) == 1
		for p := 0; p < benchNumPods; p++ {
			gotAPI := *h.pods[p].Status.DeepCopy()
			wantAPI := h.syncedAPIStatusB[p]
			if p < benchChurnPods && oddWave {
				wantAPI = h.syncedAPIStatusA[p]
			}
			// Toggling readiness false->true updates LastTransitionTime to wall-clock metav1.Now() on churning pods.
			if p < benchChurnPods {
				for idx := range gotAPI.Conditions {
					if gotAPI.Conditions[idx].LastTransitionTime.IsZero() {
						t.Fatalf("wave %d pod %d condition %s missing LastTransitionTime", wave, p, gotAPI.Conditions[idx].Type)
					}
					gotAPI.Conditions[idx].LastTransitionTime = wantAPI.Conditions[0].LastTransitionTime
				}
			}
			if !apiequality.Semantic.DeepEqual(gotAPI, wantAPI) {
				t.Fatalf("wave %d pod %d converged apiserver status mismatch:\ngot:  %+v\nwant: %+v", wave, p, gotAPI, wantAPI)
			}
		}
	}
}

func BenchmarkSyncLoopReconciliation(b *testing.B) {
	ctx := context.Background()
	logger := klog.FromContext(ctx)
	h := newSyncLoopHarness(b, false)
	defer h.controllerQueue.ShutDown()

	const expectedPLEGEvents = benchChurnPods * 3

	// Warm up two waves (even + odd) so steady-state reconciliation & churn are measured cleanly.
	for warm := 0; warm < 2; warm++ {
		ready, events, updated, reconciled := h.runSyncLoopWave(ctx, logger, warm)
		if ready != benchWakeupPods || events != expectedPLEGEvents || updated != benchChurnPods || reconciled != benchDriftedPods {
			b.Fatalf("warmup %d invariant failure: ready=%d events=%d updated=%d reconciled=%d", warm, ready, events, updated, reconciled)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ready, events, updated, reconciled := h.runSyncLoopWave(ctx, logger, i)
		if ready != benchWakeupPods || events != expectedPLEGEvents || updated != benchChurnPods || reconciled != benchDriftedPods {
			b.Fatalf("wave %d invariant failure: ready=%d events=%d updated=%d reconciled=%d", i, ready, events, updated, reconciled)
		}
	}
}
