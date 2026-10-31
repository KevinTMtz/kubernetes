//go:build linux

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

package cm_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cadvisorapi "github.com/google/cadvisor/lib/model"
	libcontainercgroups "github.com/opencontainers/cgroups"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	criapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
	drahealthv1 "k8s.io/kubelet/pkg/apis/dra-health/v1"
	drapb "k8s.io/kubelet/pkg/apis/dra/v1"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/apis/scheduling"
	kubefeatures "k8s.io/kubernetes/pkg/features"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/pkg/kubelet/cm/containermap"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/dra"
	"k8s.io/kubernetes/pkg/kubelet/cm/memorymanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	kubelettypes "k8s.io/kubernetes/pkg/kubelet/types"
	testingclock "k8s.io/utils/clock/testing"
	"k8s.io/utils/cpuset"
	"k8s.io/utils/ptr"
)

const (
	benchSteadyPods  = 60
	benchNamespaces  = 10
	benchDRADriver   = "gpu.dra.example.com"
	benchDRAClass    = "gpu-slice"
	benchDRAPool     = "numa0-gpus"
	benchHugepage2Mi = v1.ResourceName("hugepages-2Mi")
)

type benchCgroupStub struct {
	delegate cm.CgroupManager
	mu       sync.RWMutex
	created  map[string]*cm.ResourceConfig
	updated  map[string]*cm.ResourceConfig
}

func newBenchCgroupStub(logger klog.Logger, driver string) *benchCgroupStub {
	subsystems := &cm.CgroupSubsystems{
		MountPoints: map[string]string{
			"cpu":     "/sys/fs/cgroup",
			"cpuset":  "/sys/fs/cgroup",
			"memory":  "/sys/fs/cgroup",
			"hugetlb": "/sys/fs/cgroup",
			"pids":    "/sys/fs/cgroup",
		},
	}
	return &benchCgroupStub{
		delegate: cm.NewCgroupV2Manager(logger, subsystems, driver),
		created:  make(map[string]*cm.ResourceConfig, 128),
		updated:  make(map[string]*cm.ResourceConfig, 16),
	}
}

func (s *benchCgroupStub) Create(_ klog.Logger, cfg *cm.CgroupConfig) error {
	key := s.delegate.Name(cfg.Name)
	rc := *cfg.ResourceParameters
	s.mu.Lock()
	s.created[key] = &rc
	s.mu.Unlock()
	return nil
}

func (s *benchCgroupStub) Destroy(_ klog.Logger, cfg *cm.CgroupConfig) error {
	key := s.delegate.Name(cfg.Name)
	s.mu.Lock()
	delete(s.created, key)
	s.mu.Unlock()
	return nil
}

func (s *benchCgroupStub) Update(_ klog.Logger, cfg *cm.CgroupConfig) error {
	key := s.delegate.Name(cfg.Name)
	rc := *cfg.ResourceParameters
	s.mu.Lock()
	s.updated[key] = &rc
	s.mu.Unlock()
	return nil
}

func (s *benchCgroupStub) Validate(_ cm.CgroupName) error { return nil }

func (s *benchCgroupStub) Exists(name cm.CgroupName) bool {
	key := s.delegate.Name(name)
	s.mu.RLock()
	_, ok := s.created[key]
	s.mu.RUnlock()
	return ok
}

func (s *benchCgroupStub) Name(name cm.CgroupName) string            { return s.delegate.Name(name) }
func (s *benchCgroupStub) CgroupName(name string) cm.CgroupName      { return s.delegate.CgroupName(name) }
func (s *benchCgroupStub) Pids(_ klog.Logger, _ cm.CgroupName) []int { return nil }
func (s *benchCgroupStub) ReduceCPULimits(_ klog.Logger, _ cm.CgroupName) error {
	return nil
}
func (s *benchCgroupStub) MemoryUsage(_ cm.CgroupName) (int64, error) {
	return 1024 * 1024 * 1024, nil
}
func (s *benchCgroupStub) GetCgroupConfig(name cm.CgroupName, _ v1.ResourceName) (*cm.ResourceConfig, error) {
	key := s.delegate.Name(name)
	s.mu.RLock()
	rc := s.created[key]
	s.mu.RUnlock()
	return rc, nil
}
func (s *benchCgroupStub) SetCgroupConfig(_ klog.Logger, name cm.CgroupName, rc *cm.ResourceConfig) error {
	key := s.delegate.Name(name)
	copied := *rc
	s.mu.Lock()
	s.created[key] = &copied
	s.mu.Unlock()
	return nil
}
func (s *benchCgroupStub) Version() int { return 2 }

type benchSourcesReady struct {
	active *atomic.Bool
}

func (s *benchSourcesReady) AddSource(_ string) {}
func (s *benchSourcesReady) AllReady() bool     { return s.active.Load() }

type benchCPUReconcileGate struct {
	foreground *atomic.Bool
	stepCh     chan struct{}
	parkedCh   chan struct{}
}

func (g *benchCPUReconcileGate) AddSource(_ string) {}
func (g *benchCPUReconcileGate) AllReady() bool {
	if g.foreground.Load() {
		return true
	}
	select {
	case g.parkedCh <- struct{}{}:
	default:
	}
	_, ok := <-g.stepCh
	return ok
}

type benchPodStatusProvider struct {
	mu       sync.RWMutex
	statuses map[types.UID]v1.PodStatus
}

func (p *benchPodStatusProvider) GetPodStatus(uid types.UID) (v1.PodStatus, bool) {
	p.mu.RLock()
	st, ok := p.statuses[uid]
	p.mu.RUnlock()
	return st, ok
}

func (p *benchPodStatusProvider) set(uid types.UID, st v1.PodStatus) {
	p.mu.Lock()
	p.statuses[uid] = st
	p.mu.Unlock()
}

func (p *benchPodStatusProvider) del(uid types.UID) {
	p.mu.Lock()
	delete(p.statuses, uid)
	p.mu.Unlock()
}

type benchRuntimeService struct {
	updates atomic.Int64
}

func (r *benchRuntimeService) UpdateContainerResources(_ context.Context, _ string, _ *criapi.ContainerResources) error {
	r.updates.Add(1)
	return nil
}

type benchDRAServer struct {
	drapb.UnimplementedDRAPluginServer
	drahealthv1.UnimplementedDRAResourceHealthServer
}

func (s *benchDRAServer) NodePrepareResources(_ context.Context, req *drapb.NodePrepareResourcesRequest) (*drapb.NodePrepareResourcesResponse, error) {
	claims := make(map[string]*drapb.NodePrepareResourceResponse, len(req.Claims))
	for _, c := range req.Claims {
		cdiID := benchDRADriver + "/" + benchDRAClass + "=claim-" + c.Uid
		claims[c.Uid] = &drapb.NodePrepareResourceResponse{
			Devices: []*drapb.Device{
				{
					PoolName:     benchDRAPool,
					DeviceName:   "gpu-0",
					RequestNames: []string{"gpu-req"},
					CdiDeviceIds: []string{cdiID},
				},
			},
		}
	}
	return &drapb.NodePrepareResourcesResponse{Claims: claims}, nil
}

func (s *benchDRAServer) NodeUnprepareResources(_ context.Context, req *drapb.NodeUnprepareResourcesRequest) (*drapb.NodeUnprepareResourcesResponse, error) {
	claims := make(map[string]*drapb.NodeUnprepareResourceResponse, len(req.Claims))
	for _, c := range req.Claims {
		claims[c.Uid] = &drapb.NodeUnprepareResourceResponse{}
	}
	return &drapb.NodeUnprepareResourcesResponse{Claims: claims}, nil
}

type benchDiskInfoProvider struct{}

func (benchDiskInfoProvider) HasDedicatedImageFs(_ context.Context) (bool, error) { return false, nil }
func (benchDiskInfoProvider) HasDedicatedContainerFs(_ context.Context) (bool, error) {
	return false, nil
}

type benchDiskGC struct {
	imageCalls     atomic.Int64
	containerCalls atomic.Int64
}

func (g *benchDiskGC) DeleteUnusedImages(_ context.Context) error {
	g.imageCalls.Add(1)
	return nil
}

func (g *benchDiskGC) DeleteAllUnusedContainers(_ context.Context) error {
	g.containerCalls.Add(1)
	return nil
}

type evictedPodRecord struct {
	Name        string
	GracePeriod int64
	Message     string
}

type benchSummaryProvider struct {
	mu      sync.RWMutex
	current *statsapi.Summary
}

func (p *benchSummaryProvider) Get(_ context.Context, _ bool) (*statsapi.Summary, error) {
	p.mu.RLock()
	cur := p.current
	p.mu.RUnlock()
	return cur, nil
}

func (p *benchSummaryProvider) GetCPUAndMemoryStats(_ context.Context) (*statsapi.Summary, error) {
	return p.Get(context.Background(), false)
}

func (p *benchSummaryProvider) set(s *statsapi.Summary) {
	p.mu.Lock()
	p.current = s
	p.mu.Unlock()
}

type nodeResourceEvictionWaveResult struct {
	SystemdPodPaths     []string
	CgroupfsPodPaths    []string
	ParsedUIDs          []types.UID
	PodResourceConfigs  map[string]*cm.ResourceConfig
	QoSUpdatedConfigs   map[string]*cm.ResourceConfig
	ContainerAdmitCPUs  []string
	ContainerAdmitMems  []string
	PodScopeAdmitCPUs   string
	PodScopeAdmitBlocks int
	SteadyCDIDevices    int
	ChurnedCDIDevices   []string
	EvictAdmitVerdicts  []bool
	EvictedPods         []evictedPodRecord
}

type nodeResourceEvictionHarness struct {
	ctx        context.Context
	cancel     context.CancelFunc
	logger     klog.Logger
	stateDir   string
	socketDir  string
	grpcServer *grpc.Server

	waveActive *atomic.Bool
	podsMu     sync.RWMutex
	steadyPods []*v1.Pod
	activePods []*v1.Pod

	cgroupSystemd  *benchCgroupStub
	cgroupCgroupfs *benchCgroupStub
	pcmSystemd     cm.PodContainerManager
	pcmCgroupfs    cm.PodContainerManager
	qosMgr         cm.QOSContainerManager

	topoContainer topologymanager.Manager
	cpuContainer  cpumanager.Manager
	memContainer  memorymanager.Manager
	lifecycleCont cm.InternalContainerLifecycle
	statusProv    *benchPodStatusProvider
	runtimeSvc    *benchRuntimeService
	cpuForeground *atomic.Bool
	cpuStepCh     chan struct{}
	cpuParkedCh   chan struct{}

	topoPod topologymanager.Manager
	cpuPod  cpumanager.Manager
	memPod  memorymanager.Manager

	kubeClient    *fake.Clientset
	draMgr        *dra.Manager
	steadyDRAPods []*v1.Pod

	evictMgr        eviction.Manager
	evictAdmit      lifecycle.PodAdmitHandler
	evictClock      *testingclock.FakeClock
	summaryProv     *benchSummaryProvider
	diskGC          *benchDiskGC
	evictStepCh     chan []*v1.Pod
	evictParkedCh   chan struct{}
	evictMu         sync.Mutex
	evictedRecords  []evictedPodRecord
	summaryNormal   *statsapi.Summary
	summaryMemPress *statsapi.Summary
	summaryDskPress *statsapi.Summary

	probeCriticalPod   *v1.Pod
	probeBurstablePod  *v1.Pod
	probeBestEffortPod *v1.Pod
	probeToleratingPod *v1.Pod
	waveSeq            uint64
}

func populateFixtures(tb testing.TB) *nodeResourceEvictionHarness {
	tb.Helper()

	logger := klog.Background()
	ctx, cancel := context.WithCancel(klog.NewContext(context.Background(), logger))

	stateDir, err := os.MkdirTemp("", "apo-cm-state-*")
	require.NoError(tb, err)
	socketDir, err := os.MkdirTemp("", "dra-*")
	require.NoError(tb, err)

	waveActive := &atomic.Bool{}
	waveActive.Store(false)

	h := &nodeResourceEvictionHarness{
		ctx:           ctx,
		cancel:        cancel,
		logger:        logger,
		stateDir:      stateDir,
		socketDir:     socketDir,
		waveActive:    waveActive,
		statusProv:    &benchPodStatusProvider{statuses: make(map[types.UID]v1.PodStatus, 96)},
		runtimeSvc:    &benchRuntimeService{},
		cpuForeground: &atomic.Bool{},
		cpuStepCh:     make(chan struct{}),
		cpuParkedCh:   make(chan struct{}, 1),
		summaryProv:   &benchSummaryProvider{},
		diskGC:        &benchDiskGC{},
		evictStepCh:   make(chan []*v1.Pod),
		evictParkedCh: make(chan struct{}, 1),
	}

	machineInfo := buildBenchMachineInfo()

	h.steadyPods, h.steadyDRAPods = buildSteadyBenchPods()
	h.activePods = slices.Clone(h.steadyPods)
	for _, pod := range h.steadyPods {
		h.statusProv.set(pod.UID, buildRunningPodStatus(pod))
	}

	activePodsFn := func() []*v1.Pod {
		if !h.waveActive.Load() {
			return nil
		}
		h.podsMu.RLock()
		pods := slices.Clone(h.activePods)
		h.podsMu.RUnlock()
		return pods
	}
	allPodsFn := func() []*v1.Pod {
		h.podsMu.RLock()
		pods := slices.Clone(h.activePods)
		h.podsMu.RUnlock()
		return pods
	}

	// 1. Cgroup v2 (systemd + cgroupfs), PodContainerManager & QOSContainerManager.
	h.cgroupSystemd = newBenchCgroupStub(logger, "systemd")
	h.cgroupCgroupfs = newBenchCgroupStub(logger, "cgroupfs")

	rootSys := cm.NewCgroupName(cm.RootCgroupName, cm.DefaultNodeAllocatableCgroupNameForTest)
	qosInfoSys := cm.QOSContainersInfo{
		Guaranteed: rootSys,
		Burstable:  cm.NewCgroupName(rootSys, "burstable"),
		BestEffort: cm.NewCgroupName(rootSys, "besteffort"),
	}
	throttlingFactor := 0.8

	// 2. Container-scope TopologyManager (single-numa-node) + CPUManager (static) + MemoryManager (Static).
	topoContainer, err := topologymanager.NewManager(logger, machineInfo.Topology, topologymanager.PolicySingleNumaNode, topologymanager.ContainerTopologyScope, nil)
	require.NoError(tb, err)
	h.topoContainer = topoContainer

	nodeReserved := v1.ResourceList{
		v1.ResourceCPU:    resource.MustParse("2"),
		v1.ResourceMemory: resource.MustParse("4Gi"),
	}
	memReservations := []kubeletconfig.MemoryReservation{
		{
			NumaNode: 0,
			Limits:   v1.ResourceList{v1.ResourceMemory: resource.MustParse("2Gi")},
		},
		{
			NumaNode: 1,
			Limits:   v1.ResourceList{v1.ResourceMemory: resource.MustParse("2Gi")},
		},
	}

	cpuContDir := filepath.Join(stateDir, "cpu-cont")
	memContDir := filepath.Join(stateDir, "mem-cont")
	require.NoError(tb, os.MkdirAll(cpuContDir, 0o755))
	require.NoError(tb, os.MkdirAll(memContDir, 0o755))

	cpuContainer, err := cpumanager.NewManager(logger, "static", nil, 20*time.Microsecond, machineInfo, cpuset.New(), nodeReserved, cpuContDir, topoContainer)
	require.NoError(tb, err)
	memContainer, err := memorymanager.NewManager(logger, "Static", machineInfo, nodeReserved, memReservations, memContDir, topoContainer)
	require.NoError(tb, err)
	h.cpuContainer = cpuContainer
	h.memContainer = memContainer
	topoContainer.AddHintProvider(logger, cpuContainer)
	topoContainer.AddHintProvider(logger, memContainer)

	sourcesReady := &benchSourcesReady{active: waveActive}
	cpuGate := &benchCPUReconcileGate{
		foreground: h.cpuForeground,
		stepCh:     h.cpuStepCh,
		parkedCh:   h.cpuParkedCh,
	}
	initMap := buildInitialContainerMap(h.steadyPods)
	require.NoError(tb, cpuContainer.Start(ctx, activePodsFn, cpuGate, h.statusProv, h.runtimeSvc, initMap.Clone()))
	<-h.cpuParkedCh
	require.NoError(tb, memContainer.Start(ctx, activePodsFn, sourcesReady, h.statusProv, h.runtimeSvc, initMap.Clone()))

	// Pre-admit the 8 container-scoped Guaranteed steady pods so exclusive CPU & NUMA memory state is realistic.
	waveActive.Store(true)
	h.cpuForeground.Store(true)
	for i := 0; i < 8; i++ {
		pod := h.steadyPods[i]
		res := topoContainer.Admit(ctx, &lifecycle.PodAdmitAttributes{Pod: pod, Operation: lifecycle.AddOperation})
		require.True(tb, res.Admit, "failed pre-admitting steady guaranteed pod %s: %s", pod.Name, res.Message)
		for ci := range pod.Spec.InitContainers {
			c := &pod.Spec.InitContainers[ci]
			cid := fmt.Sprintf("%s-init-%d", pod.UID, ci)
			topoContainer.AddContainer(logger, pod, c, cid)
			cpuContainer.AddContainer(logger, pod, c, cid)
			memContainer.AddContainer(logger, pod, c, cid)
			_ = cpuContainer.RemoveContainer(logger, cid)
			_ = memContainer.RemoveContainer(logger, cid)
			_ = topoContainer.RemoveContainer(logger, cid)
		}
		for ci := range pod.Spec.Containers {
			c := &pod.Spec.Containers[ci]
			cid := fmt.Sprintf("%s-app-%d", pod.UID, ci)
			topoContainer.AddContainer(logger, pod, c, cid)
			cpuContainer.AddContainer(logger, pod, c, cid)
			memContainer.AddContainer(logger, pod, c, cid)
		}
	}
	h.cpuForeground.Store(false)
	waveActive.Store(false)

	h.lifecycleCont = cm.NewInternalContainerLifecycleForTest(cpuContainer, memContainer, topoContainer)
	h.pcmSystemd = cm.NewPodContainerManagerForTest(
		qosInfoSys,
		h.cgroupSystemd,
		cpuContainer,
		true,
		kubeletconfig.TieredReservationMemoryReservationPolicy,
		&throttlingFactor,
	)
	h.pcmCgroupfs = cm.NewPodContainerManagerForTest(
		qosInfoSys,
		h.cgroupCgroupfs,
		cpuContainer,
		true,
		kubeletconfig.TieredReservationMemoryReservationPolicy,
		&throttlingFactor,
	)

	allocatableNodeMem := v1.ResourceList{
		v1.ResourceCPU:    resource.MustParse("62"),
		v1.ResourceMemory: resource.MustParse("252Gi"),
	}
	h.qosMgr = cm.NewQOSContainerManagerForTest(
		qosInfoSys,
		rootSys,
		h.cgroupSystemd,
		allPodsFn,
		func() v1.ResourceList { return allocatableNodeMem },
		map[v1.ResourceName]int64{v1.ResourceMemory: 50},
		kubeletconfig.TieredReservationMemoryReservationPolicy,
	)

	// 3. Pod-scope TopologyManager (restricted) + CPUManager (static) + MemoryManager (Static) for PodLevelResourceManagers.
	topoPod, err := topologymanager.NewManager(logger, machineInfo.Topology, topologymanager.PolicyRestricted, topologymanager.PodTopologyScope, nil)
	require.NoError(tb, err)
	h.topoPod = topoPod

	cpuPodDir := filepath.Join(stateDir, "cpu-pod")
	memPodDir := filepath.Join(stateDir, "mem-pod")
	require.NoError(tb, os.MkdirAll(cpuPodDir, 0o755))
	require.NoError(tb, os.MkdirAll(memPodDir, 0o755))

	cpuPod, err := cpumanager.NewManager(logger, "static", nil, 10*time.Minute, machineInfo, cpuset.New(), nodeReserved, cpuPodDir, topoPod)
	require.NoError(tb, err)
	memPod, err := memorymanager.NewManager(logger, "Static", machineInfo, nodeReserved, memReservations, memPodDir, topoPod)
	require.NoError(tb, err)
	h.cpuPod = cpuPod
	h.memPod = memPod
	topoPod.AddHintProvider(logger, cpuPod)
	topoPod.AddHintProvider(logger, memPod)

	idleSources := &benchSourcesReady{active: &atomic.Bool{}}
	require.NoError(tb, cpuPod.Start(ctx, func() []*v1.Pod { return h.steadyPods[8:12] }, idleSources, h.statusProv, h.runtimeSvc, containermap.NewContainerMap()))
	require.NoError(tb, memPod.Start(ctx, func() []*v1.Pod { return h.steadyPods[8:12] }, idleSources, h.statusProv, h.runtimeSvc, containermap.NewContainerMap()))

	for i := 8; i < 12; i++ {
		pod := h.steadyPods[i]
		res := topoPod.Admit(ctx, &lifecycle.PodAdmitAttributes{Pod: pod, Operation: lifecycle.AddOperation})
		require.True(tb, res.Admit, "failed pre-admitting pod-level guaranteed pod %s: %s", pod.Name, res.Message)
	}

	// 4. DRAManager + local Unix-socket gRPC plugin + 8 pre-prepared steady ResourceClaims.
	h.kubeClient = fake.NewSimpleClientset()
	sockPath := filepath.Join(socketDir, "dra.sock")
	lis, err := net.Listen("unix", sockPath)
	require.NoError(tb, err)
	h.grpcServer = grpc.NewServer()
	draSrv := &benchDRAServer{}
	drapb.RegisterDRAPluginServer(h.grpcServer, draSrv)
	drahealthv1.RegisterDRAResourceHealthServer(h.grpcServer, draSrv)
	go func() {
		_ = h.grpcServer.Serve(lis)
	}()

	draDir := filepath.Join(stateDir, "dra")
	require.NoError(tb, os.MkdirAll(draDir, 0o755))
	draMgr, err := dra.NewManager(logger, h.kubeClient, draDir)
	require.NoError(tb, err)
	h.draMgr = draMgr
	nodeObj := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "bench-node-01"}}
	require.NoError(tb, draMgr.Start(ctx, allPodsFn, func(context.Context) (*v1.Node, error) { return nodeObj, nil }, idleSources))
	require.NoError(tb, draMgr.GetWatcherHandler().RegisterPlugin(ctx, benchDRADriver, sockPath, []string{drapb.DRAPluginService}, nil))

	for i, pod := range h.steadyDRAPods {
		claim := buildDRAClaimForPod(pod, fmt.Sprintf("steady-claim-%02d", i))
		_, err := h.kubeClient.ResourceV1().ResourceClaims(pod.Namespace).Create(ctx, claim, metav1.CreateOptions{})
		require.NoError(tb, err)
		require.NoError(tb, draMgr.PrepareResources(ctx, pod))
	}

	// 5. EvictionManager with localStorageCapacityIsolation & hard/soft Memory + NodeFs + Inodes + PID thresholds.
	baseTime := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	h.evictClock = testingclock.NewFakeClock(baseTime)
	h.summaryNormal = buildBenchSummary(h.steadyPods, baseTime, 64*1024*1024*1024, 120*1024*1024*1024)
	h.summaryMemPress = buildBenchSummary(h.steadyPods, baseTime.Add(1*time.Minute), 256*1024*1024, 120*1024*1024*1024)
	h.summaryDskPress = buildBenchSummary(h.steadyPods, baseTime.Add(12*time.Minute), 64*1024*1024*1024, 512*1024*1024)
	h.summaryProv.set(h.summaryNormal)

	evictCfg := eviction.Config{
		PressureTransitionPeriod: 5 * time.Minute,
		MaxPodGracePeriodSeconds: 30,
		Thresholds: []evictionapi.Threshold{
			{
				Signal:   evictionapi.SignalMemoryAvailable,
				Operator: evictionapi.OpLessThan,
				Value:    evictionapi.ThresholdValue{Quantity: ptr.To(resource.MustParse("1Gi"))},
			},
			{
				Signal:      evictionapi.SignalMemoryAvailable,
				Operator:    evictionapi.OpLessThan,
				Value:       evictionapi.ThresholdValue{Quantity: ptr.To(resource.MustParse("4Gi"))},
				GracePeriod: 2 * time.Minute,
			},
			{
				Signal:   evictionapi.SignalNodeFsAvailable,
				Operator: evictionapi.OpLessThan,
				Value:    evictionapi.ThresholdValue{Quantity: ptr.To(resource.MustParse("2Gi"))},
			},
			{
				Signal:   evictionapi.SignalNodeFsInodesFree,
				Operator: evictionapi.OpLessThan,
				Value:    evictionapi.ThresholdValue{Percentage: 0.05},
			},
			{
				Signal:   evictionapi.SignalPIDAvailable,
				Operator: evictionapi.OpLessThan,
				Value:    evictionapi.ThresholdValue{Percentage: 0.05},
			},
		},
	}

	killPodFn := func(pod *v1.Pod, _ bool, gracePeriodOverride *int64, statusFn func(*v1.PodStatus)) error {
		st := pod.Status
		if statusFn != nil {
			statusFn(&st)
		}
		gp := int64(0)
		if gracePeriodOverride != nil {
			gp = *gracePeriodOverride
		}
		h.evictMu.Lock()
		h.evictedRecords = append(h.evictedRecords, evictedPodRecord{
			Name:        pod.Name,
			GracePeriod: gp,
			Message:     st.Message,
		})
		h.evictMu.Unlock()
		return nil
	}

	fakeRec := &record.FakeRecorder{Events: make(chan string, 1024)}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-fakeRec.Events:
			}
		}
	}()

	evictMgr, evictAdmit := eviction.NewManager(
		h.summaryProv,
		evictCfg,
		killPodFn,
		h.diskGC,
		h.diskGC,
		fakeRec,
		&v1.ObjectReference{Kind: "Node", Name: "bench-node-01", UID: "bench-node-01"},
		h.evictClock,
		true, // localStorageCapacityIsolation
	)
	h.evictMgr = evictMgr
	h.evictAdmit = evictAdmit

	h.probeCriticalPod, h.probeBurstablePod, h.probeBestEffortPod, h.probeToleratingPod = buildEvictionAdmissionProbePods()

	evictPodFunc := func() []*v1.Pod {
		select {
		case h.evictParkedCh <- struct{}{}:
		default:
		}
		pods, ok := <-h.evictStepCh
		if !ok {
			return nil
		}
		return pods
	}
	evictMgr.Start(ctx, benchDiskInfoProvider{}, evictPodFunc, func(*v1.Pod) bool { return true }, 20*time.Microsecond)
	<-h.evictParkedCh

	tb.Cleanup(func() {
		h.close()
	})
	return h
}

func (h *nodeResourceEvictionHarness) close() {
	h.waveActive.Store(false)
	h.cancel()
	close(h.evictStepCh)
	if h.draMgr != nil {
		h.draMgr.Stop()
	}
	if h.grpcServer != nil {
		h.grpcServer.Stop()
	}
	time.Sleep(10 * time.Millisecond)
	_ = os.RemoveAll(h.socketDir)
	_ = os.RemoveAll(h.stateDir)
}

func (h *nodeResourceEvictionHarness) stepCPUReconcile() {
	h.cpuStepCh <- struct{}{}
	<-h.cpuParkedCh
}

func (h *nodeResourceEvictionHarness) stepEvictionTick(summary *statsapi.Summary, pods []*v1.Pod, expectsEviction bool) {
	h.summaryProv.set(summary)
	h.evictStepCh <- pods
	if !expectsEviction {
		<-h.evictParkedCh
		return
	}
	for {
		select {
		case <-h.evictParkedCh:
			return
		default:
			if h.evictClock.HasWaiters() {
				h.evictClock.Step(2 * time.Second)
			}
			runtime.Gosched()
		}
	}
}

func (h *nodeResourceEvictionHarness) runWave(captureDetails bool) nodeResourceEvictionWaveResult {
	h.waveSeq++
	seq := h.waveSeq
	h.waveActive.Store(true)
	defer h.waveActive.Store(false)

	var out nodeResourceEvictionWaveResult
	if captureDetails {
		out.PodResourceConfigs = make(map[string]*cm.ResourceConfig, len(h.steadyPods))
		out.QoSUpdatedConfigs = make(map[string]*cm.ResourceConfig, 4)
	}

	// Phase 1: Cgroup v2 path translation (systemd & cgroupfs), IsPodCgroup, ResourceConfigForPod
	// (Guaranteed, Burstable, BestEffort, PodLevelResources, MemoryQoS) & QOSContainerManager.UpdateCgroups.
	h.cgroupSystemd.mu.Lock()
	clear(h.cgroupSystemd.created)
	h.cgroupSystemd.mu.Unlock()

	for i, pod := range h.steadyPods {
		cgSys, sysPath := h.pcmSystemd.GetPodContainerName(pod)
		cgFs, fsPath := h.pcmCgroupfs.GetPodContainerName(pod)
		parsedSys := cm.ParseSystemdToCgroupName(sysPath)
		parsedFs := cm.ParseCgroupfsToCgroupName(fsPath)
		_, uidSys := h.pcmSystemd.IsPodCgroup(sysPath)
		_, uidFs := h.pcmCgroupfs.IsPodCgroup(fsPath)
		if err := h.pcmSystemd.EnsureExists(h.logger, pod); err != nil {
			panic(err)
		}
		// Simulate in-place vertical pod resize cgroup config recomputation on 6 pods per wave.
		if i%10 == 0 {
			rc := cm.ResourceConfigForPod(pod, true, cm.QuotaPeriod, libcontainercgroups.IsCgroup2UnifiedMode(), kubeletconfig.TieredReservationMemoryReservationPolicy)
			cm.ApplyPodLevelMemoryHigh(pod, rc, 0.8)
			_ = h.pcmSystemd.SetPodCgroupConfig(h.logger, pod, rc)
		}
		if captureDetails && i < 15 {
			out.SystemdPodPaths = append(out.SystemdPodPaths, parsedSys.ToSystemd())
			out.CgroupfsPodPaths = append(out.CgroupfsPodPaths, parsedFs.ToCgroupfs())
			out.ParsedUIDs = append(out.ParsedUIDs, uidSys, uidFs)
			_ = cgSys
			_ = cgFs
			h.cgroupSystemd.mu.RLock()
			out.PodResourceConfigs[pod.Name] = h.cgroupSystemd.created[sysPath]
			h.cgroupSystemd.mu.RUnlock()
		}
	}

	for tick := 0; tick < 4; tick++ {
		if err := h.qosMgr.UpdateCgroups(h.logger); err != nil {
			panic(err)
		}
	}
	if captureDetails {
		h.cgroupSystemd.mu.RLock()
		for k, v := range h.cgroupSystemd.updated {
			copied := *v
			out.QoSUpdatedConfigs[k] = &copied
		}
		h.cgroupSystemd.mu.RUnlock()
	}

	// Phase 2: TopologyManager + CPUManager (static) + MemoryManager (Static) + InternalContainerLifecycle.
	// 2a. Admit, PreCreateContainer, AddContainer, query state, and tear down 2 container-scoped Guaranteed pods.
	h.cpuForeground.Store(true)
	for bIdx := 0; bIdx < 2; bIdx++ {
		batchPod := buildChurnedContainerGuaranteedPod(seq, bIdx)
		h.podsMu.Lock()
		h.activePods = append(h.activePods, batchPod)
		h.podsMu.Unlock()
		h.statusProv.set(batchPod.UID, buildRunningPodStatus(batchPod))

		admitRes := h.topoContainer.Admit(h.ctx, &lifecycle.PodAdmitAttributes{Pod: batchPod, Operation: lifecycle.AddOperation})
		if !admitRes.Admit {
			panic("unexpected container-scope topology reject: " + admitRes.Message)
		}

		for ci := range batchPod.Spec.InitContainers {
			c := &batchPod.Spec.InitContainers[ci]
			cid := fmt.Sprintf("%s-init-%d", batchPod.UID, ci)
			criCfg := &criapi.ContainerConfig{Linux: &criapi.LinuxContainerConfig{Resources: &criapi.LinuxContainerResources{}}}
			_ = h.lifecycleCont.PreCreateContainer(h.logger, batchPod, c, criCfg)
			h.topoContainer.AddContainer(h.logger, batchPod, c, cid)
			h.cpuContainer.AddContainer(h.logger, batchPod, c, cid)
			h.memContainer.AddContainer(h.logger, batchPod, c, cid)
			_ = h.cpuContainer.RemoveContainer(h.logger, cid)
			_ = h.memContainer.RemoveContainer(h.logger, cid)
			_ = h.topoContainer.RemoveContainer(h.logger, cid)
		}
		var cids []string
		for ci := range batchPod.Spec.Containers {
			c := &batchPod.Spec.Containers[ci]
			cid := fmt.Sprintf("%s-app-%d", batchPod.UID, ci)
			cids = append(cids, cid)
			criCfg := &criapi.ContainerConfig{Linux: &criapi.LinuxContainerConfig{Resources: &criapi.LinuxContainerResources{}}}
			_ = h.lifecycleCont.PreCreateContainer(h.logger, batchPod, c, criCfg)
			h.topoContainer.AddContainer(h.logger, batchPod, c, cid)
			h.cpuContainer.AddContainer(h.logger, batchPod, c, cid)
			h.memContainer.AddContainer(h.logger, batchPod, c, cid)
			_ = h.cpuContainer.GetExclusiveCPUs(string(batchPod.UID), c.Name)
			_ = h.cpuContainer.GetResourceIsolationLevel(batchPod, c)
			_ = h.memContainer.GetMemory(h.logger, string(batchPod.UID), c.Name)
			if captureDetails && bIdx == 0 {
				out.ContainerAdmitCPUs = append(out.ContainerAdmitCPUs, criCfg.Linux.Resources.CpusetCpus)
				out.ContainerAdmitMems = append(out.ContainerAdmitMems, criCfg.Linux.Resources.CpusetMems)
			}
		}
		_ = h.cpuContainer.GetAllocatableCPUs()
		_ = h.memContainer.GetAllocatableMemory(h.logger)

		h.podsMu.Lock()
		h.activePods = h.activePods[:len(h.activePods)-1]
		h.podsMu.Unlock()
		h.statusProv.del(batchPod.UID)
		for _, cid := range cids {
			_ = h.cpuContainer.RemoveContainer(h.logger, cid)
			_ = h.memContainer.RemoveContainer(h.logger, cid)
			_ = h.topoContainer.RemoveContainer(h.logger, cid)
		}
	}
	h.cpuForeground.Store(false)

	// Run 6 deterministic 10s CPUManager reconcileState passes across the 60 active pods.
	for r := 0; r < 6; r++ {
		h.stepCPUReconcile()
	}

	// 2b. Pod-scope Guaranteed pod with PodLevelResources (AllocatePod, GetPodTopologyHints, GetPodCPUs, GetPodMemory, DeletePod).
	podLevelBatch := buildChurnedPodLevelGuaranteedPod(seq)
	podAdmitRes := h.topoPod.Admit(h.ctx, &lifecycle.PodAdmitAttributes{Pod: podLevelBatch, Operation: lifecycle.AddOperation})
	if !podAdmitRes.Admit {
		panic("unexpected pod-scope topology reject: " + podAdmitRes.Message)
	}
	podCPUs := h.cpuPod.GetPodCPUs(string(podLevelBatch.UID))
	podMemBlocks := h.memPod.GetPodMemory(string(podLevelBatch.UID))
	if captureDetails {
		out.PodScopeAdmitCPUs = podCPUs.String()
		out.PodScopeAdmitBlocks = len(podMemBlocks)
	}
	for ci := range podLevelBatch.Spec.InitContainers {
		c := &podLevelBatch.Spec.InitContainers[ci]
		cid := fmt.Sprintf("%s-podinit-%d", podLevelBatch.UID, ci)
		h.topoPod.AddContainer(h.logger, podLevelBatch, c, cid)
		h.cpuPod.AddContainer(h.logger, podLevelBatch, c, cid)
		h.memPod.AddContainer(h.logger, podLevelBatch, c, cid)
		_ = h.cpuPod.RemoveContainer(h.logger, cid)
		_ = h.memPod.RemoveContainer(h.logger, cid)
		_ = h.topoPod.RemoveContainer(h.logger, cid)
	}
	var podCIDs []string
	for ci := range podLevelBatch.Spec.Containers {
		c := &podLevelBatch.Spec.Containers[ci]
		cid := fmt.Sprintf("%s-podapp-%d", podLevelBatch.UID, ci)
		podCIDs = append(podCIDs, cid)
		h.topoPod.AddContainer(h.logger, podLevelBatch, c, cid)
		h.cpuPod.AddContainer(h.logger, podLevelBatch, c, cid)
		h.memPod.AddContainer(h.logger, podLevelBatch, c, cid)
	}
	for _, cid := range podCIDs {
		_ = h.cpuPod.RemoveContainer(h.logger, cid)
		_ = h.memPod.RemoveContainer(h.logger, cid)
		_ = h.topoPod.RemoveContainer(h.logger, cid)
	}

	// Phase 3: DRAManager steady-state GetResources + churned PrepareResources / GetResources / UnprepareResources.
	steadyCDI := 0
	for _, draPod := range h.steadyDRAPods {
		for ci := range draPod.Spec.Containers {
			info, err := h.draMgr.GetResources(draPod, &draPod.Spec.Containers[ci])
			if err != nil {
				panic(err)
			}
			if info != nil {
				steadyCDI += len(info.CDIDevices)
			}
		}
	}
	out.SteadyCDIDevices = steadyCDI

	churnDRAPod := buildChurnedDRAPod(seq)
	churnClaimName := fmt.Sprintf("churn-claim-%d", seq)
	churnClaim := buildDRAClaimForPod(churnDRAPod, churnClaimName)
	if _, err := h.kubeClient.ResourceV1().ResourceClaims(churnDRAPod.Namespace).Create(h.ctx, churnClaim, metav1.CreateOptions{}); err != nil {
		panic(err)
	}
	if err := h.draMgr.PrepareResources(h.ctx, churnDRAPod); err != nil {
		panic(err)
	}
	draInfo, err := h.draMgr.GetResources(churnDRAPod, &churnDRAPod.Spec.Containers[0])
	if err != nil {
		panic(err)
	}
	if captureDetails && draInfo != nil {
		for _, dev := range draInfo.CDIDevices {
			out.ChurnedCDIDevices = append(out.ChurnedCDIDevices, dev.Name)
		}
	}
	if err := h.draMgr.UnprepareResources(h.ctx, churnDRAPod); err != nil {
		panic(err)
	}
	_ = h.kubeClient.ResourceV1().ResourceClaims(churnDRAPod.Namespace).Delete(h.ctx, churnClaimName, metav1.DeleteOptions{})

	// Phase 4: EvictionManager steady-state housekeeping (localStorageCapacityIsolation), Admission & Memory/Disk Pressure Ranking.
	h.evictMu.Lock()
	h.evictedRecords = h.evictedRecords[:0]
	h.evictMu.Unlock()

	evictPods := slices.Clone(h.steadyPods)

	// 4 steady-state ticks (localStorageEviction across 60 pods x 4 = 240 pod checks).
	for t := 0; t < 4; t++ {
		h.evictClock.Step(10 * time.Second)
		stampSummaryTime(h.summaryNormal, h.evictClock.Now())
		h.stepEvictionTick(h.summaryNormal, evictPods, false)
	}
	if captureDetails {
		out.EvictAdmitVerdicts = append(out.EvictAdmitVerdicts,
			h.evictAdmit.Admit(h.ctx, &lifecycle.PodAdmitAttributes{Pod: h.probeBestEffortPod}).Admit,
			h.evictAdmit.Admit(h.ctx, &lifecycle.PodAdmitAttributes{Pod: h.probeBurstablePod}).Admit,
		)
	}

	// Tick 5: MemoryAvailable hard threshold breach -> rankMemoryPressure across 60 pods & evict top candidate.
	h.evictClock.Step(10 * time.Second)
	stampSummaryTime(h.summaryMemPress, h.evictClock.Now())
	memCandidatePods := slices.Clone(evictPods)
	h.stepEvictionTick(h.summaryMemPress, memCandidatePods, true)
	if captureDetails {
		out.EvictAdmitVerdicts = append(out.EvictAdmitVerdicts,
			h.evictAdmit.Admit(h.ctx, &lifecycle.PodAdmitAttributes{Pod: h.probeCriticalPod}).Admit,
			h.evictAdmit.Admit(h.ctx, &lifecycle.PodAdmitAttributes{Pod: h.probeBurstablePod}).Admit,
			h.evictAdmit.Admit(h.ctx, &lifecycle.PodAdmitAttributes{Pod: h.probeBestEffortPod}).Admit,
			h.evictAdmit.Admit(h.ctx, &lifecycle.PodAdmitAttributes{Pod: h.probeToleratingPod}).Admit,
		)
	}

	// Advance past PressureTransitionPeriod (5m) and run Tick 6: NodeFsAvailable hard breach -> GC + rankDiskPressureFunc.
	h.evictClock.Step(6 * time.Minute)
	stampSummaryTime(h.summaryDskPress, h.evictClock.Now())
	diskCandidatePods := slices.Clone(evictPods)
	h.stepEvictionTick(h.summaryDskPress, diskCandidatePods, true)
	if captureDetails {
		out.EvictAdmitVerdicts = append(out.EvictAdmitVerdicts,
			h.evictAdmit.Admit(h.ctx, &lifecycle.PodAdmitAttributes{Pod: h.probeCriticalPod}).Admit,
			h.evictAdmit.Admit(h.ctx, &lifecycle.PodAdmitAttributes{Pod: h.probeBurstablePod}).Admit,
		)
	}

	// Recovery tick: clear conditions so next wave starts clean.
	h.evictClock.Step(6 * time.Minute)
	stampSummaryTime(h.summaryNormal, h.evictClock.Now())
	h.stepEvictionTick(h.summaryNormal, evictPods, false)

	if captureDetails {
		h.evictMu.Lock()
		out.EvictedPods = slices.Clone(h.evictedRecords)
		h.evictMu.Unlock()
	}
	return out
}

func TestNodeResourceCgroupAndEvictionEquivalence(t *testing.T) {
	featuregatetesting.SetFeatureGatesDuringTest(t, utilfeature.DefaultFeatureGate, featuregatetesting.FeatureOverrides{
		kubefeatures.PodLevelResources:           true,
		kubefeatures.PodLevelResourceManagers:    true,
		kubefeatures.MemoryQoS:                   true,
		kubefeatures.QOSReserved:                 true,
		kubefeatures.DRANodeAllocatableResources: true,
	})

	h := populateFixtures(t)
	r1 := h.runWave(true)
	r2 := h.runWave(true)

	// 1. Cgroup v2 systemd & cgroupfs round-trip and UID extraction parity.
	require.Len(t, r1.SystemdPodPaths, 15)
	require.Equal(t, r1.SystemdPodPaths, r2.SystemdPodPaths)
	require.Equal(t, r1.CgroupfsPodPaths, r2.CgroupfsPodPaths)
	require.Equal(t, r1.ParsedUIDs, r2.ParsedUIDs)
	for i := 0; i < 15; i++ {
		expectedUID := h.steadyPods[i].UID
		require.Equal(t, expectedUID, r1.ParsedUIDs[2*i])
		require.Equal(t, expectedUID, r1.ParsedUIDs[2*i+1])
	}

	// Verify Guaranteed, Pod-Level Guaranteed, and Burstable ResourceConfigForPod invariants.
	guarCfg := r1.PodResourceConfigs["pod-guar-00"]
	require.NotNil(t, guarCfg)
	require.NotNil(t, guarCfg.CPUShares)
	require.NotNil(t, guarCfg.CPUQuota)
	require.NotNil(t, guarCfg.Memory)
	require.Equal(t, int64(2*1024*1024*1024), *guarCfg.Memory)
	require.Equal(t, int64(64*1024*1024), guarCfg.HugePageLimit[2*1024*1024])

	podLevelCfg := r1.PodResourceConfigs["pod-guar-podlevel-08"]
	require.NotNil(t, podLevelCfg)
	require.NotNil(t, podLevelCfg.Memory)
	require.Equal(t, int64(2*1024*1024*1024), *podLevelCfg.Memory)

	// Verify QoS container updates are deterministic across waves.
	require.NotEmpty(t, r1.QoSUpdatedConfigs)
	for k, cfg1 := range r1.QoSUpdatedConfigs {
		cfg2, ok := r2.QoSUpdatedConfigs[k]
		require.True(t, ok)
		if cfg1.CPUShares != nil || cfg2.CPUShares != nil {
			require.NotNil(t, cfg1.CPUShares)
			require.NotNil(t, cfg2.CPUShares)
			require.Equal(t, *cfg1.CPUShares, *cfg2.CPUShares)
		}
		if cfg1.Memory != nil || cfg2.Memory != nil {
			require.NotNil(t, cfg1.Memory)
			require.NotNil(t, cfg2.Memory)
			require.Equal(t, *cfg1.Memory, *cfg2.Memory)
		}
		require.Equal(t, cfg1.HugePageLimit, cfg2.HugePageLimit)
		require.Equal(t, cfg1.Unified, cfg2.Unified)
	}

	// 2. Topology + CPU + Memory + Pod-level Resource Manager parity (and zero leaked allocations across waves).
	require.Len(t, r1.ContainerAdmitCPUs, 2)
	require.Equal(t, r1.ContainerAdmitCPUs, r2.ContainerAdmitCPUs)
	require.Equal(t, r1.ContainerAdmitMems, r2.ContainerAdmitMems)
	require.NotEmpty(t, r1.ContainerAdmitCPUs[0])
	require.NotEmpty(t, r1.ContainerAdmitMems[0])
	require.NotEmpty(t, r1.PodScopeAdmitCPUs)
	require.Equal(t, r1.PodScopeAdmitCPUs, r2.PodScopeAdmitCPUs)
	require.Equal(t, r1.PodScopeAdmitBlocks, r2.PodScopeAdmitBlocks)
	require.Greater(t, r1.PodScopeAdmitBlocks, 0)

	// 3. DRA steady & churned claim preparation/unpreparation parity.
	require.Equal(t, 16, r1.SteadyCDIDevices)
	require.Equal(t, 16, r2.SteadyCDIDevices)
	require.Len(t, r1.ChurnedCDIDevices, 1)
	require.Len(t, r2.ChurnedCDIDevices, 1)

	// 4. EvictionManager admission & ranking determinism across waves.
	expectedVerdicts := []bool{true, true, true, true, false, true, true, false}
	require.Equal(t, expectedVerdicts, r1.EvictAdmitVerdicts)
	require.Equal(t, expectedVerdicts, r2.EvictAdmitVerdicts)

	require.Len(t, r1.EvictedPods, 2)
	require.Equal(t, r1.EvictedPods, r2.EvictedPods)
	require.Equal(t, "pod-be-59", r1.EvictedPods[0].Name)
	require.Equal(t, int64(1), r1.EvictedPods[0].GracePeriod)
	require.Equal(t, "pod-be-58", r1.EvictedPods[1].Name)
	require.Equal(t, int64(1), r1.EvictedPods[1].GracePeriod)
	require.False(t, h.evictMgr.IsUnderMemoryPressure())
	require.False(t, h.evictMgr.IsUnderDiskPressure())
}

func BenchmarkNodeResourceCgroupAndEviction(b *testing.B) {
	klogState := klog.CaptureState()
	b.Cleanup(klogState.Restore)
	klog.LogToStderr(false)
	klog.SetOutput(io.Discard)

	featuregatetesting.SetFeatureGatesDuringTest(b, utilfeature.DefaultFeatureGate, featuregatetesting.FeatureOverrides{
		kubefeatures.PodLevelResources:           true,
		kubefeatures.PodLevelResourceManagers:    true,
		kubefeatures.MemoryQoS:                   true,
		kubefeatures.QOSReserved:                 true,
		kubefeatures.DRANodeAllocatableResources: true,
	})

	h := populateFixtures(b)
	_ = h.runWave(false)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = h.runWave(false)
	}
	b.StopTimer()
}

func buildBenchMachineInfo() *cadvisorapi.MachineInfo {
	const (
		numNUMA          = 2
		coresPerNUMA     = 16
		threadsPerCore   = 2
		nodeMemBytes     = uint64(128) * 1024 * 1024 * 1024
		nodeHuge2MiPages = uint64(2048)
	)
	nodes := make([]cadvisorapi.Node, numNUMA)
	for n := 0; n < numNUMA; n++ {
		cores := make([]cadvisorapi.Core, coresPerNUMA)
		for c := 0; c < coresPerNUMA; c++ {
			t0 := n*coresPerNUMA*threadsPerCore + c
			t1 := t0 + coresPerNUMA
			cores[c] = cadvisorapi.Core{
				Id:       c,
				Threads:  []int{t0, t1},
				SocketID: n,
			}
		}
		dists := []uint64{10, 21}
		if n == 1 {
			dists = []uint64{21, 10}
		}
		nodes[n] = cadvisorapi.Node{
			Id:     n,
			Memory: nodeMemBytes,
			HugePages: []cadvisorapi.HugePagesInfo{
				{PageSize: 2048, NumPages: nodeHuge2MiPages},
			},
			Cores:     cores,
			Distances: dists,
		}
	}
	return &cadvisorapi.MachineInfo{
		NumCores:       numNUMA * coresPerNUMA * threadsPerCore,
		MemoryCapacity: numNUMA * nodeMemBytes,
		Topology:       nodes,
	}
}

func buildSteadyBenchPods() ([]*v1.Pod, []*v1.Pod) {
	pods := make([]*v1.Pod, 0, benchSteadyPods)
	draPods := make([]*v1.Pod, 0, 8)
	defaultPrio := int32(0)
	lowPrio := int32(-10)
	highPrio := int32(100)

	// 0..7: 8 container-scoped Guaranteed pods (2 exclusive CPUs, 2Gi memory, 64Mi hugepages-2Mi, DRA claims).
	for i := 0; i < 8; i++ {
		ns := fmt.Sprintf("ns-%02d", i%benchNamespaces)
		name := fmt.Sprintf("pod-guar-%02d", i)
		uid := types.UID(fmt.Sprintf("00000000-0000-0000-0001-%012d", i))
		claimName := fmt.Sprintf("steady-claim-%02d", i)
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
				UID:       uid,
			},
			Spec: v1.PodSpec{
				Priority: &highPrio,
				ResourceClaims: []v1.PodResourceClaim{
					{Name: "gpu", ResourceClaimName: &claimName},
				},
				InitContainers: []v1.Container{
					{
						Name: "init-cfg",
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("1"),
								v1.ResourceMemory: resource.MustParse("512Mi"),
							},
							Limits: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("1"),
								v1.ResourceMemory: resource.MustParse("512Mi"),
							},
						},
					},
				},
				Containers: []v1.Container{
					{
						Name: "app-main",
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("1"),
								v1.ResourceMemory: resource.MustParse("1Gi"),
								benchHugepage2Mi:  resource.MustParse("64Mi"),
							},
							Limits: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("1"),
								v1.ResourceMemory: resource.MustParse("1Gi"),
								benchHugepage2Mi:  resource.MustParse("64Mi"),
							},
							Claims: []v1.ResourceClaim{{Name: "gpu"}},
						},
					},
					{
						Name: "app-sidecar",
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("1"),
								v1.ResourceMemory: resource.MustParse("1Gi"),
							},
							Limits: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("1"),
								v1.ResourceMemory: resource.MustParse("1Gi"),
							},
							Claims: []v1.ResourceClaim{{Name: "gpu"}},
						},
					},
				},
			},
		}
		pods = append(pods, pod)
		draPods = append(draPods, pod)
	}

	// 8..11: 4 Pod-Level Guaranteed pods (Pod.Spec.Resources set, shared + exclusive container mix).
	for i := 8; i < 12; i++ {
		ns := fmt.Sprintf("ns-%02d", i%benchNamespaces)
		name := fmt.Sprintf("pod-guar-podlevel-%02d", i)
		uid := types.UID(fmt.Sprintf("00000000-0000-0000-0002-%012d", i))
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
				UID:       uid,
			},
			Spec: v1.PodSpec{
				Priority: &highPrio,
				Resources: &v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("2Gi"),
					},
					Limits: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("2Gi"),
					},
				},
				InitContainers: []v1.Container{
					{Name: "init-shared"},
				},
				Containers: []v1.Container{
					{
						Name: "app-pinned",
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("1"),
								v1.ResourceMemory: resource.MustParse("1Gi"),
							},
							Limits: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("1"),
								v1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					},
					{Name: "app-shared"},
				},
			},
		}
		pods = append(pods, pod)
	}

	// 12..47: 36 Burstable pods (with EmptyDir SizeLimit, ephemeral-storage limits, PodLevel burstable, and DRA node-allocatable statuses).
	for i := 12; i < 48; i++ {
		ns := fmt.Sprintf("ns-%02d", i%benchNamespaces)
		name := fmt.Sprintf("pod-burst-%02d", i)
		uid := types.UID(fmt.Sprintf("00000000-0000-0000-0003-%012d", i))
		emptyDirLimit := resource.MustParse("512Mi")
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
				UID:       uid,
			},
			Spec: v1.PodSpec{
				Priority: &defaultPrio,
				Volumes: []v1.Volume{
					{
						Name: "scratch",
						VolumeSource: v1.VolumeSource{
							EmptyDir: &v1.EmptyDirVolumeSource{SizeLimit: &emptyDirLimit},
						},
					},
					{
						Name: "cache",
						VolumeSource: v1.VolumeSource{
							EmptyDir: &v1.EmptyDirVolumeSource{},
						},
					},
				},
				InitContainers: []v1.Container{
					{
						Name: "init-prep",
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("100m"),
								v1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					},
				},
				Containers: []v1.Container{
					{
						Name: "web",
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceCPU:              resource.MustParse("250m"),
								v1.ResourceMemory:           resource.MustParse("256Mi"),
								v1.ResourceEphemeralStorage: resource.MustParse("256Mi"),
							},
							Limits: v1.ResourceList{
								v1.ResourceCPU:              resource.MustParse("1"),
								v1.ResourceMemory:           resource.MustParse("512Mi"),
								v1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
							},
						},
					},
					{
						Name: "metrics-agent",
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("100m"),
								v1.ResourceMemory: resource.MustParse("128Mi"),
							},
							Limits: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("500m"),
								v1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
					},
				},
			},
		}
		if i < 18 {
			pod.Spec.Resources = &v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("400m"),
					v1.ResourceMemory: resource.MustParse("400Mi"),
				},
				Limits: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("1500m"),
					v1.ResourceMemory: resource.MustParse("800Mi"),
				},
			}
		}
		if i >= 18 && i < 24 {
			draMem := resource.MustParse("64Mi")
			pod.Status.NodeAllocatableResourceClaimStatuses = []v1.NodeAllocatableResourceClaimStatus{
				{
					ResourceClaimName: fmt.Sprintf("alloc-claim-%02d", i),
					Containers:        []string{"web"},
					Mapping: []v1.NodeAllocatableMappedResources{
						{Name: v1.ResourceMemory, Quantity: &draMem},
					},
				},
			}
		}
		pods = append(pods, pod)
	}

	// 48..59: 12 BestEffort pods (lower priority on 58 & 59 so ranking order is strictly deterministic).
	for i := 48; i < benchSteadyPods; i++ {
		ns := fmt.Sprintf("ns-%02d", i%benchNamespaces)
		name := fmt.Sprintf("pod-be-%02d", i)
		uid := types.UID(fmt.Sprintf("00000000-0000-0000-0004-%012d", i))
		prio := defaultPrio
		if i >= 56 {
			prio = lowPrio
		}
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
				UID:       uid,
			},
			Spec: v1.PodSpec{
				Priority: &prio,
				Volumes: []v1.Volume{
					{
						Name:         "tmp",
						VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}},
					},
				},
				InitContainers: []v1.Container{{Name: "init-be"}},
				Containers: []v1.Container{
					{Name: "worker-a"},
					{Name: "worker-b"},
				},
			},
		}
		pods = append(pods, pod)
	}

	return pods, draPods
}

func buildRunningPodStatus(pod *v1.Pod) v1.PodStatus {
	now := metav1.Now()
	initStatuses := make([]v1.ContainerStatus, len(pod.Spec.InitContainers))
	for i, c := range pod.Spec.InitContainers {
		initStatuses[i] = v1.ContainerStatus{
			Name:        c.Name,
			ContainerID: fmt.Sprintf("containerd://%s-init-%d", pod.UID, i),
			State: v1.ContainerState{
				Terminated: &v1.ContainerStateTerminated{ExitCode: 0, FinishedAt: now},
			},
		}
	}
	appStatuses := make([]v1.ContainerStatus, len(pod.Spec.Containers))
	for i, c := range pod.Spec.Containers {
		appStatuses[i] = v1.ContainerStatus{
			Name:        c.Name,
			ContainerID: fmt.Sprintf("containerd://%s-app-%d", pod.UID, i),
			State: v1.ContainerState{
				Running: &v1.ContainerStateRunning{StartedAt: now},
			},
		}
	}
	st := pod.Status
	st.Phase = v1.PodRunning
	st.InitContainerStatuses = initStatuses
	st.ContainerStatuses = appStatuses
	return st
}

func buildInitialContainerMap(pods []*v1.Pod) containermap.ContainerMap {
	cmap := containermap.NewContainerMap()
	for _, pod := range pods {
		for i, c := range pod.Spec.Containers {
			cmap.Add(string(pod.UID), c.Name, fmt.Sprintf("%s-app-%d", pod.UID, i))
		}
	}
	return cmap
}

func buildChurnedContainerGuaranteedPod(seq uint64, idx int) *v1.Pod {
	uid := types.UID(fmt.Sprintf("00000000-0000-0099-%04d-%012d", idx, seq))
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("batch-guar-%d-%d", seq, idx),
			Namespace: "ns-00",
			UID:       uid,
		},
		Spec: v1.PodSpec{
			InitContainers: []v1.Container{
				{
					Name: "init-numa",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("2"),
							v1.ResourceMemory: resource.MustParse("512Mi"),
						},
						Limits: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("2"),
							v1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				},
			},
			Containers: []v1.Container{
				{
					Name: "compute",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("2"),
							v1.ResourceMemory: resource.MustParse("1Gi"),
							benchHugepage2Mi:  resource.MustParse("32Mi"),
						},
						Limits: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("2"),
							v1.ResourceMemory: resource.MustParse("1Gi"),
							benchHugepage2Mi:  resource.MustParse("32Mi"),
						},
					},
				},
				{
					Name: "helper",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("1"),
							v1.ResourceMemory: resource.MustParse("512Mi"),
						},
						Limits: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("1"),
							v1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				},
			},
		},
	}
}

func buildChurnedPodLevelGuaranteedPod(seq uint64) *v1.Pod {
	uid := types.UID(fmt.Sprintf("00000000-0000-0098-0000-%012d", seq))
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("batch-podlevel-%d", seq),
			Namespace: "ns-01",
			UID:       uid,
		},
		Spec: v1.PodSpec{
			Resources: &v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("4"),
					v1.ResourceMemory: resource.MustParse("2Gi"),
				},
				Limits: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("4"),
					v1.ResourceMemory: resource.MustParse("2Gi"),
				},
			},
			InitContainers: []v1.Container{
				{Name: "init-shared"},
			},
			Containers: []v1.Container{
				{
					Name: "engine",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("2"),
							v1.ResourceMemory: resource.MustParse("1Gi"),
						},
						Limits: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("2"),
							v1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				},
				{Name: "logger-shared"},
			},
		},
	}
}

func buildChurnedDRAPod(seq uint64) *v1.Pod {
	claimName := fmt.Sprintf("churn-claim-%d", seq)
	uid := types.UID(fmt.Sprintf("00000000-0000-0097-0000-%012d", seq))
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("batch-dra-%d", seq),
			Namespace: "ns-02",
			UID:       uid,
		},
		Spec: v1.PodSpec{
			ResourceClaims: []v1.PodResourceClaim{
				{Name: "gpu", ResourceClaimName: &claimName},
			},
			Containers: []v1.Container{
				{
					Name: "infer",
					Resources: v1.ResourceRequirements{
						Claims: []v1.ResourceClaim{{Name: "gpu"}},
					},
				},
			},
		},
	}
}

func buildDRAClaimForPod(pod *v1.Pod, claimName string) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: pod.Namespace,
			UID:       types.UID("claim-uid-" + claimName),
		},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{
				Requests: []resourceapi.DeviceRequest{
					{
						Name: "gpu-req",
						Exactly: &resourceapi.ExactDeviceRequest{
							DeviceClassName: benchDRAClass,
						},
					},
				},
			},
		},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{
							Request: "gpu-req",
							Driver:  benchDRADriver,
							Pool:    benchDRAPool,
							Device:  "gpu-0",
						},
					},
				},
			},
			ReservedFor: []resourceapi.ResourceClaimConsumerReference{
				{Resource: "pods", Name: pod.Name, UID: pod.UID},
			},
		},
	}
}

func buildBenchSummary(pods []*v1.Pod, ts time.Time, availMemBytes, availFsBytes uint64) *statsapi.Summary {
	metaTime := metav1.NewTime(ts)
	totalMem := uint64(256) * 1024 * 1024 * 1024
	workingSet := totalMem - availMemBytes
	totalFs := uint64(500) * 1024 * 1024 * 1024
	usedFs := totalFs - availFsBytes
	inodes := uint64(10_000_000)
	inodesFree := uint64(8_500_000)
	inodesUsed := inodes - inodesFree
	maxPID := int64(4_194_304)
	runningProcs := int64(2_400)

	podStats := make([]statsapi.PodStats, len(pods))
	for i, pod := range pods {
		memWS := uint64(128+i*4) * 1024 * 1024
		rootUsed := uint64(32+i*2) * 1024 * 1024
		logsUsed := uint64(8+i) * 1024 * 1024
		volUsed := uint64(64) * 1024 * 1024
		if pod.Name == "pod-be-59" {
			memWS = 1600 * 1024 * 1024
			rootUsed = 40 * 1024 * 1024
		} else if pod.Name == "pod-be-58" {
			memWS = 200 * 1024 * 1024
			rootUsed = 900 * 1024 * 1024
			volUsed = 800 * 1024 * 1024
		}

		cStats := make([]statsapi.ContainerStats, len(pod.Spec.Containers))
		perContMem := memWS / uint64(len(pod.Spec.Containers))
		perContRoot := rootUsed / uint64(len(pod.Spec.Containers))
		perContLogs := logsUsed / uint64(len(pod.Spec.Containers))
		for ci, c := range pod.Spec.Containers {
			mWS := perContMem
			rUsed := perContRoot
			lUsed := perContLogs
			cInodes := uint64(200 + ci)
			cStats[ci] = statsapi.ContainerStats{
				Name:      c.Name,
				StartTime: metaTime,
				Memory: &statsapi.MemoryStats{
					Time:            metaTime,
					UsageBytes:      ptr.To(mWS + 16*1024*1024),
					WorkingSetBytes: ptr.To(mWS),
				},
				Rootfs: &statsapi.FsStats{
					Time:       metaTime,
					UsedBytes:  ptr.To(rUsed),
					InodesUsed: ptr.To(cInodes),
				},
				Logs: &statsapi.FsStats{
					Time:       metaTime,
					UsedBytes:  ptr.To(lUsed),
					InodesUsed: ptr.To(uint64(16)),
				},
			}
		}

		var volStats []statsapi.VolumeStats
		for _, v := range pod.Spec.Volumes {
			vu := volUsed
			volStats = append(volStats, statsapi.VolumeStats{
				Name: v.Name,
				FsStats: statsapi.FsStats{
					Time:       metaTime,
					UsedBytes:  ptr.To(vu),
					InodesUsed: ptr.To(uint64(64)),
				},
			})
		}

		ephemUsed := rootUsed + logsUsed + volUsed*uint64(len(volStats))
		podStats[i] = statsapi.PodStats{
			PodRef: statsapi.PodReference{
				Name:      pod.Name,
				Namespace: pod.Namespace,
				UID:       string(pod.UID),
			},
			StartTime:  metaTime,
			Containers: cStats,
			Memory: &statsapi.MemoryStats{
				Time:            metaTime,
				UsageBytes:      ptr.To(memWS + 32*1024*1024),
				WorkingSetBytes: ptr.To(memWS),
			},
			VolumeStats: volStats,
			EphemeralStorage: &statsapi.FsStats{
				Time:       metaTime,
				UsedBytes:  ptr.To(ephemUsed),
				InodesUsed: ptr.To(uint64(512)),
			},
			ProcessStats: &statsapi.ProcessStats{
				ProcessCount: ptr.To(uint64(12 + (i % 8))),
			},
		}
	}

	return &statsapi.Summary{
		Node: statsapi.NodeStats{
			NodeName:  "bench-node-01",
			StartTime: metaTime,
			Memory: &statsapi.MemoryStats{
				Time:            metaTime,
				AvailableBytes:  ptr.To(availMemBytes),
				UsageBytes:      ptr.To(workingSet + 2*1024*1024*1024),
				WorkingSetBytes: ptr.To(workingSet),
			},
			Fs: &statsapi.FsStats{
				Time:           metaTime,
				AvailableBytes: ptr.To(availFsBytes),
				CapacityBytes:  ptr.To(totalFs),
				UsedBytes:      ptr.To(usedFs),
				InodesFree:     ptr.To(inodesFree),
				Inodes:         ptr.To(inodes),
				InodesUsed:     ptr.To(inodesUsed),
			},
			Runtime: &statsapi.RuntimeStats{
				ImageFs: &statsapi.FsStats{
					Time:           metaTime,
					AvailableBytes: ptr.To(availFsBytes),
					CapacityBytes:  ptr.To(totalFs),
					UsedBytes:      ptr.To(usedFs),
					InodesFree:     ptr.To(inodesFree),
					Inodes:         ptr.To(inodes),
					InodesUsed:     ptr.To(inodesUsed),
				},
			},
			Rlimit: &statsapi.RlimitStats{
				Time:                  metaTime,
				MaxPID:                ptr.To(maxPID),
				NumOfRunningProcesses: ptr.To(runningProcs),
			},
			SystemContainers: []statsapi.ContainerStats{
				{
					Name: statsapi.SystemContainerPods,
					Memory: &statsapi.MemoryStats{
						Time:            metaTime,
						UsageBytes:      ptr.To(workingSet),
						WorkingSetBytes: ptr.To(workingSet),
					},
				},
				{
					Name: statsapi.SystemContainerKubelet,
					Memory: &statsapi.MemoryStats{
						Time:            metaTime,
						WorkingSetBytes: ptr.To(uint64(256 * 1024 * 1024)),
					},
				},
			},
		},
		Pods: podStats,
	}
}

func stampSummaryTime(s *statsapi.Summary, ts time.Time) {
	mt := metav1.NewTime(ts)
	s.Node.Memory.Time = mt
	s.Node.Fs.Time = mt
	s.Node.Runtime.ImageFs.Time = mt
	s.Node.Rlimit.Time = mt
	for i := range s.Node.SystemContainers {
		if s.Node.SystemContainers[i].Memory != nil {
			s.Node.SystemContainers[i].Memory.Time = mt
		}
	}
}

func buildEvictionAdmissionProbePods() (criticalPod, burstablePod, bestEffortPod, toleratingPod *v1.Pod) {
	critPrio := scheduling.SystemCriticalPriority
	criticalPod = &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "probe-critical",
			Namespace:   "kube-system",
			UID:         "probe-critical-uid",
			Annotations: map[string]string{kubelettypes.ConfigSourceAnnotationKey: kubelettypes.FileSource},
		},
		Spec: v1.PodSpec{
			PriorityClassName: "system-node-critical",
			Priority:          &critPrio,
			Containers:        []v1.Container{{Name: "crit"}},
		},
	}
	burstablePod = &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "probe-burstable", Namespace: "ns-00", UID: "probe-burstable-uid"},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name: "app",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("100m"),
							v1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
		},
	}
	bestEffortPod = &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "probe-besteffort", Namespace: "ns-00", UID: "probe-besteffort-uid"},
		Spec: v1.PodSpec{
			Containers: []v1.Container{{Name: "be"}},
		},
	}
	toleratingPod = &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "probe-tolerating-be", Namespace: "ns-00", UID: "probe-tolerating-uid"},
		Spec: v1.PodSpec{
			Tolerations: []v1.Toleration{
				{
					Key:      v1.TaintNodeMemoryPressure,
					Operator: v1.TolerationOpExists,
					Effect:   v1.TaintEffectNoSchedule,
				},
			},
			Containers: []v1.Container{{Name: "be-tol"}},
		},
	}
	return criticalPod, burstablePod, bestEffortPod, toleratingPod
}
