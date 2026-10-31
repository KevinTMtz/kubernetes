/*
Copyright 2017 The Kubernetes Authors.

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

package cpumanager

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	cadvisorapi "github.com/google/cadvisor/lib/model"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	resourcehelper "k8s.io/component-helpers/resource"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"

	kubefeatures "k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/cm/containermap"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	cmqos "k8s.io/kubernetes/pkg/kubelet/cm/qos"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/config"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/utils/cpuset"
)

// ActivePodsFunc is a function that returns a list of pods to reconcile.
type ActivePodsFunc func() []*v1.Pod

type runtimeService interface {
	UpdateContainerResources(ctx context.Context, id string, resources *runtimeapi.ContainerResources) error
}

type policyName string

// cpuManagerStateFileName is the file name where cpu manager stores its state
const cpuManagerStateFileName = "cpu_manager_state"

// Manager interface provides methods for Kubelet to manage pod cpus.
type Manager interface {
	// Start is called during Kubelet initialization.
	// Start takes a `Context` because it may possibly spin the reconcileState helper, which in turn
	// needs to update container state, which takes a context.
	Start(ctx context.Context, activePods ActivePodsFunc, sourcesReady config.SourcesReady, podStatusProvider status.PodStatusProvider, containerRuntime runtimeService, initialContainers containermap.ContainerMap) error

	// Called to trigger the allocation of CPUs to a container. This must be
	// called at some point prior to the AddContainer() call for a container,
	// e.g. at pod admission time.
	Allocate(ctx context.Context, pod *v1.Pod, container *v1.Container, operation lifecycle.Operation) error

	// AddContainer adds the mapping between container ID to pod UID and the container name
	// The mapping used to remove the CPU allocation during the container removal
	AddContainer(logger klog.Logger, p *v1.Pod, c *v1.Container, containerID string)

	// RemoveContainer is called after Kubelet decides to kill or delete a
	// container. After this call, the CPU manager stops trying to reconcile
	// that container and any CPUs dedicated to the container are freed.
	RemoveContainer(logger klog.Logger, containerID string) error

	// State returns a read-only interface to the internal CPU manager state.
	State() state.Reader

	// GetTopologyHints implements the topologymanager.HintProvider Interface
	// and is consulted to achieve NUMA aware resource alignment among this
	// and other resource controllers.
	GetTopologyHints(logger klog.Logger, pod *v1.Pod, container *v1.Container, operation lifecycle.Operation) map[string][]topologymanager.TopologyHint

	// GetExclusiveCPUs implements the podresources.CPUsProvider interface to provide
	// exclusively allocated cpus for the container
	GetExclusiveCPUs(podUID, containerName string) cpuset.CPUSet

	// GetPodCPUs implements the podresources.CPUsProvider interface to provide
	// the total cpuset allocated to the pod
	GetPodCPUs(podUID string) cpuset.CPUSet

	// GetPodTopologyHints implements the topologymanager.HintProvider Interface
	// and is consulted to achieve NUMA aware resource alignment per Pod
	// among this and other resource controllers.
	GetPodTopologyHints(logger klog.Logger, pod *v1.Pod, operation lifecycle.Operation) map[string][]topologymanager.TopologyHint

	// AllocatePod is called to trigger the allocation of CPUs to a pod.
	AllocatePod(logger klog.Logger, pod *v1.Pod, operation lifecycle.Operation) error

	// GetAllocatableCPUs returns the total set of CPUs available for allocation.
	GetAllocatableCPUs() cpuset.CPUSet

	// GetCPUAffinity returns cpuset which includes cpus from shared pools
	// as well as exclusively allocated cpus
	GetCPUAffinity(podUID, containerName string) cpuset.CPUSet

	// GetAllCPUs returns all the CPUs known by cpumanager, as reported by the
	// hardware discovery. Maps to the CPU capacity.
	GetAllCPUs() cpuset.CPUSet

	// GetResourceIsolationLevel returns the isolation level of the container.
	GetResourceIsolationLevel(pod *v1.Pod, container *v1.Container) cmqos.ResourceIsolationLevel

	// CanAllocateExclusively reports whether the configured policy supports exclusive
	// assignment of the resource for eligible workloads, regardless of available capacity.
	CanAllocateExclusively(res v1.ResourceName) bool
}

type manager struct {
	sync.Mutex
	policy Policy

	// reconcilePeriod is the duration between calls to reconcileState.
	reconcilePeriod time.Duration

	// state allows pluggable CPU assignment policies while sharing a common
	// representation of state for the system to inspect and reconcile.
	state state.State

	// lastUpdatedstate holds state for each container from the last time it was updated.
	lastUpdateState state.State

	// containerRuntime is the container runtime service interface needed
	// to make UpdateContainerResources() calls against the containers.
	containerRuntime runtimeService

	// activePods is a method for listing active pods on the node
	// so all the containers can be updated in the reconciliation loop.
	activePods ActivePodsFunc

	// podStatusProvider provides a method for obtaining pod statuses
	// and the containerID of their containers
	podStatusProvider status.PodStatusProvider

	// containerMap provides a mapping from (pod, container) -> containerID
	// for all containers a pod
	containerMap containermap.ContainerMap

	topology *topology.CPUTopology

	nodeAllocatableReservation v1.ResourceList

	// sourcesReady provides the readiness of kubelet configuration sources such as apiserver update readiness.
	// We use it to determine when we can purge inactive pods from checkpointed state.
	sourcesReady config.SourcesReady

	// stateFileDirectory holds the directory where the state file for checkpoints is held.
	stateFileDirectory string

	// allCPUs is the set of online CPUs as reported by the system
	allCPUs cpuset.CPUSet

	// allocatableCPUs is the set of online CPUs as reported by the system,
	// and available for allocation, minus the reserved set
	allocatableCPUs cpuset.CPUSet
}

var _ Manager = &manager{}

type sourcesReadyStub struct{}

func (s *sourcesReadyStub) AddSource(_ string) {}
func (s *sourcesReadyStub) AllReady() bool     { return true }

// NewManager creates new cpu manager based on provided policy
func NewManager(logger klog.Logger, cpuPolicyName string, cpuPolicyOptions map[string]string, reconcilePeriod time.Duration, machineInfo *cadvisorapi.MachineInfo, specificCPUs cpuset.CPUSet, nodeAllocatableReservation v1.ResourceList, stateFileDirectory string, affinity topologymanager.Store) (Manager, error) {
	var topo *topology.CPUTopology
	var policy Policy
	var err error

	topo, err = topology.Discover(logger, machineInfo)
	if err != nil {
		return nil, err
	}

	switch policyName(cpuPolicyName) {

	case PolicyNone:
		policy, err = NewNonePolicy(cpuPolicyOptions)
		if err != nil {
			return nil, fmt.Errorf("new none policy error: %w", err)
		}

	case PolicyStatic:
		logger.Info("Detected CPU topology", "topology", topo)

		reservedCPUs, ok := nodeAllocatableReservation[v1.ResourceCPU]
		if !ok {
			// The static policy cannot initialize without this information.
			return nil, fmt.Errorf("[cpumanager] unable to determine reserved CPU resources for static policy")
		}
		if reservedCPUs.IsZero() {
			// The static policy requires this to be nonzero. Zero CPU reservation
			// would allow the shared pool to be completely exhausted. At that point
			// either we would violate our guarantee of exclusivity or need to evict
			// any pod that has at least one container that requires zero CPUs.
			// See the comments in policy_static.go for more details.
			return nil, fmt.Errorf("[cpumanager] the static policy requires systemreserved.cpu + kubereserved.cpu to be greater than zero")
		}

		// Take the ceiling of the reservation, since fractional CPUs cannot be
		// exclusively allocated.
		reservedCPUsFloat := float64(reservedCPUs.MilliValue()) / 1000
		numReservedCPUs := int(math.Ceil(reservedCPUsFloat))
		policy, err = NewStaticPolicy(logger, topo, numReservedCPUs, specificCPUs, affinity, cpuPolicyOptions)
		if err != nil {
			return nil, fmt.Errorf("new static policy error: %w", err)
		}

	default:
		return nil, fmt.Errorf("unknown policy: \"%s\"", cpuPolicyName)
	}

	manager := &manager{
		policy:                     policy,
		reconcilePeriod:            reconcilePeriod,
		lastUpdateState:            state.NewMemoryState(logger),
		topology:                   topo,
		nodeAllocatableReservation: nodeAllocatableReservation,
		stateFileDirectory:         stateFileDirectory,
		allCPUs:                    topo.CPUDetails.CPUs(),
	}
	manager.sourcesReady = &sourcesReadyStub{}
	return manager, nil
}

func (m *manager) Start(ctx context.Context, activePods ActivePodsFunc, sourcesReady config.SourcesReady, podStatusProvider status.PodStatusProvider, containerRuntime runtimeService, initialContainers containermap.ContainerMap) error {
	logger := klog.FromContext(ctx)
	logger.Info("Starting", "policy", m.policy.Name())
	logger.Info("Reconciling", "reconcilePeriod", m.reconcilePeriod)
	m.sourcesReady = sourcesReady
	m.activePods = activePods
	m.podStatusProvider = podStatusProvider
	m.containerRuntime = containerRuntime
	m.containerMap = initialContainers

	stateImpl, err := state.NewCheckpointState(logger, m.stateFileDirectory, cpuManagerStateFileName, m.policy.Name(), m.containerMap)
	if err != nil {
		logger.Error(err, "Could not initialize checkpoint manager, please drain node and remove policy state file")
		return err
	}
	m.state = stateImpl

	err = m.policy.Start(logger, m.state)
	if err != nil {
		logger.Error(err, "Policy start error")
		return err
	}

	logger.V(4).Info("CPU manager started", "policy", m.policy.Name())

	m.allocatableCPUs = m.policy.GetAllocatableCPUs(m.state)

	if m.policy.Name() == string(PolicyNone) {
		return nil
	}
	// Periodically call m.reconcileState() to continue to keep the CPU sets of
	// all pods in sync with and guaranteed CPUs handed out among them.
	go wait.Until(func() { m.reconcileState(ctx) }, m.reconcilePeriod, wait.NeverStop)
	return nil
}

func (m *manager) Allocate(ctx context.Context, p *v1.Pod, c *v1.Container, operation lifecycle.Operation) error {
	logger := klog.FromContext(ctx)

	// Garbage collect any stranded resources before allocating CPUs.
	m.removeStaleState(logger)

	m.Lock()
	defer m.Unlock()

	// Call down into the policy to assign this container CPUs if required.
	err := m.policy.Allocate(logger, m.state, p, c, operation)
	if err != nil {
		logger.Error(err, "policy error")
		return err
	}

	return nil
}

func (m *manager) AllocatePod(logger klog.Logger, pod *v1.Pod, operation lifecycle.Operation) error {
	// Garbage collect any stranded resources before allocating CPUs.
	m.removeStaleState(logger)

	m.Lock()
	defer m.Unlock()

	// Call down into the policy to assign this container CPUs if required.
	if err := m.policy.AllocatePod(logger, m.state, pod, operation); err != nil {
		logger.Error(err, "AllocatePod error", "pod", klog.KObj(pod))
		return err
	}
	return nil
}

func (m *manager) AddContainer(logger klog.Logger, pod *v1.Pod, container *v1.Container, containerID string) {
	m.Lock()
	defer m.Unlock()
	if cset, exists := getCPUSetView(m.state, string(pod.UID), container.Name); exists {
		m.lastUpdateState.SetCPUSet(string(pod.UID), container.Name, cset)
	}
	m.containerMap.Add(string(pod.UID), container.Name, containerID)
	if logger.V(4).Enabled() {
		logger.V(4).Info("Added Container", "pod", klog.KObj(pod), "podUID", pod.UID, "containerName", container.Name, "containerID", containerID)
	}
}

func (m *manager) RemoveContainer(logger klog.Logger, containerID string) error {
	m.Lock()
	defer m.Unlock()

	err := m.policyRemoveContainerByID(logger, containerID)
	if err != nil {
		logger.Error(err, "RemoveContainer error")
		return err
	}

	return nil
}

func (m *manager) policyRemoveContainerByID(logger klog.Logger, containerID string) error {
	podUID, containerName, err := m.containerMap.GetContainerRef(containerID)
	if err != nil {
		return nil
	}

	err = m.policy.RemoveContainer(logger, m.state, podUID, containerName)
	if err == nil {
		m.lastUpdateState.Delete(podUID, containerName)
		m.containerMap.RemoveByContainerID(containerID)
	}

	return err
}

func (m *manager) policyRemoveContainerByRef(logger klog.Logger, podUID string, containerName string) error {
	err := m.policy.RemoveContainer(logger, m.state, podUID, containerName)
	if err == nil {
		m.lastUpdateState.Delete(podUID, containerName)
		m.containerMap.RemoveByContainerRef(podUID, containerName)
	}

	return err
}

func (m *manager) State() state.Reader {
	return m.state
}

func (m *manager) GetTopologyHints(logger klog.Logger, pod *v1.Pod, container *v1.Container, operation lifecycle.Operation) map[string][]topologymanager.TopologyHint {
	// Garbage collect any stranded resources before providing TopologyHints
	m.removeStaleState(logger)
	// Delegate to active policy
	return m.policy.GetTopologyHints(logger, m.state, pod, container, operation)
}

func (m *manager) GetPodTopologyHints(logger klog.Logger, pod *v1.Pod, operation lifecycle.Operation) map[string][]topologymanager.TopologyHint {
	// Garbage collect any stranded resources before providing TopologyHints
	m.removeStaleState(logger)
	// Delegate to active policy
	return m.policy.GetPodTopologyHints(logger, m.state, pod, operation)
}

func (m *manager) GetAllocatableCPUs() cpuset.CPUSet {
	return m.allocatableCPUs.Clone()
}

func (m *manager) GetAllCPUs() cpuset.CPUSet {
	return m.allCPUs.Clone()
}

type reconciledContainer struct {
	podName       string
	containerName string
	containerID   string
}

type containerRef struct {
	podUID        string
	containerName string
}

func findActivePod(activePods []*v1.Pod, podUID string) *v1.Pod {
	for _, pod := range activePods {
		if string(pod.UID) == podUID {
			return pod
		}
	}
	return nil
}

func podHasContainer(pod *v1.Pod, containerName string) bool {
	if pod == nil {
		return false
	}
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == containerName {
			return true
		}
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == containerName {
			return true
		}
	}
	return false
}

func getCPUSetView(r state.Reader, podUID, containerName string) (cpuset.CPUSet, bool) {
	if fr, ok := r.(state.FastReader); ok {
		return fr.GetCPUSetView(podUID, containerName)
	}
	return r.GetCPUSet(podUID, containerName)
}

func getDefaultCPUSetView(r state.Reader) cpuset.CPUSet {
	if fr, ok := r.(state.FastReader); ok {
		return fr.GetDefaultCPUSetView()
	}
	return r.GetDefaultCPUSet()
}

func getCPUSetOrDefaultView(r state.Reader, podUID, containerName string) cpuset.CPUSet {
	if fr, ok := r.(state.FastReader); ok {
		return fr.GetCPUSetOrDefaultView(podUID, containerName)
	}
	return r.GetCPUSetOrDefault(podUID, containerName)
}

func getPodCPUSetView(r state.Reader, podUID string) (cpuset.CPUSet, bool) {
	if fr, ok := r.(state.FastReader); ok {
		return fr.GetPodCPUSetView(podUID)
	}
	return r.GetPodCPUSet(podUID)
}

func getBaselineCPUSetView(r state.Reader, podUID, containerName string) (cpuset.CPUSet, bool) {
	if fr, ok := r.(state.FastReader); ok {
		return fr.GetBaselineCPUSetView(podUID, containerName)
	}
	return r.GetBaselineCPUSet(podUID, containerName)
}

func (m *manager) removeStaleState(rootLogger klog.Logger) {
	// Only once all sources are ready do we attempt to remove any stale state.
	// This ensures that the call to `m.activePods()` below will succeed with
	// the actual active pods list.
	if !m.sourcesReady.AllReady() {
		return
	}

	// We grab the lock to ensure that no new containers will grab CPUs while
	// executing the code below. Without this lock, its possible that we end up
	// removing state that is newly added by an asynchronous call to
	// AddContainer() during the execution of this code.
	m.Lock()
	defer m.Unlock()

	// Get the list of active pods.
	activePods := m.activePods()
	logV5 := rootLogger.V(5).Enabled()

	// Loop through the CPUManager state. Remove any state for containers not
	// in the `activePods` list.
	if fr, ok := m.state.(state.FastReader); ok {
		var staleRefs []containerRef
		var lastPodUID string
		var lastPod *v1.Pod
		hasLastPod := false
		fr.ForEachCPUAssignment(func(podUID, containerName string, _ cpuset.CPUSet) {
			if !hasLastPod || podUID != lastPodUID {
				lastPodUID = podUID
				lastPod = findActivePod(activePods, podUID)
				hasLastPod = true
			}
			if podHasContainer(lastPod, containerName) {
				if logV5 {
					klog.LoggerWithValues(rootLogger, "podUID", podUID, "containerName", containerName).V(5).Info("container still active")
				}
				return
			}
			staleRefs = append(staleRefs, containerRef{podUID: podUID, containerName: containerName})
		})
		for _, ref := range staleRefs {
			logger := klog.LoggerWithValues(rootLogger, "podUID", ref.podUID, "containerName", ref.containerName)
			logger.V(2).Info("removing container")
			err := m.policyRemoveContainerByRef(logger, ref.podUID, ref.containerName)
			if err != nil {
				logger.Error(err, "failed to remove container")
			}
		}
	} else {
		assignments := m.state.GetCPUAssignments()
		for podUID := range assignments {
			pod := findActivePod(activePods, podUID)
			for containerName := range assignments[podUID] {
				if podHasContainer(pod, containerName) {
					if logV5 {
						klog.LoggerWithValues(rootLogger, "podUID", podUID, "containerName", containerName).V(5).Info("container still active")
					}
					continue
				}

				logger := klog.LoggerWithValues(rootLogger, "podUID", podUID, "containerName", containerName)
				logger.V(2).Info("removing container")
				err := m.policyRemoveContainerByRef(logger, podUID, containerName)
				if err != nil {
					logger.Error(err, "failed to remove container")
				}
			}
		}
	}

	m.containerMap.Visit(func(podUID, containerName, containerID string) {
		if podHasContainer(findActivePod(activePods, podUID), containerName) {
			if logV5 {
				klog.LoggerWithValues(rootLogger, "podUID", podUID, "containerName", containerName).V(5).Info("containerMap: container still active")
			}
			return
		}
		logger := klog.LoggerWithValues(rootLogger, "podUID", podUID, "containerName", containerName)
		logger.V(2).Info("containerMap: removing container")
		err := m.policyRemoveContainerByRef(logger, podUID, containerName)
		if err != nil {
			logger.Error(err, "containerMap: failed to remove container")
		}
	})
}

func (m *manager) reconcileState(ctx context.Context) (success []reconciledContainer, failure []reconciledContainer) {
	rootLogger := klog.FromContext(ctx)

	m.removeStaleState(rootLogger)
	activePods := m.activePods()
	success = make([]reconciledContainer, 0, len(activePods))
	failure = []reconciledContainer{}

	logV2 := rootLogger.V(2).Enabled()
	logV4 := rootLogger.V(4).Enabled()
	logV5 := rootLogger.V(5).Enabled()
	inPlaceExclusiveCPUs := utilfeature.DefaultFeatureGate.Enabled(kubefeatures.InPlacePodVerticalScalingExclusiveCPUs)

	for _, pod := range activePods {
		pstatus, ok := m.podStatusProvider.GetPodStatus(pod.UID)
		if !ok {
			if logV5 {
				klog.LoggerWithValues(rootLogger, "pod", klog.KObj(pod)).V(5).Info("skipping pod; status not found")
			}
			failure = append(failure, reconciledContainer{pod.Name, "", ""})
			continue
		}

		podUID := string(pod.UID)
		for pass := 0; pass < 2; pass++ {
			containers := pod.Spec.InitContainers
			if pass == 1 {
				containers = pod.Spec.Containers
			}
			for i := range containers {
				containerName := containers[i].Name

				containerID, err := findContainerIDByName(&pstatus, containerName)
				if err != nil {
					if logV5 {
						klog.LoggerWithValues(rootLogger, "pod", klog.KObj(pod), "containerName", containerName).V(5).Info("skipping container; ID not found in pod status", "err", err)
					}
					failure = append(failure, reconciledContainer{pod.Name, containerName, ""})
					continue
				}

				cstatus, err := findContainerStatusByName(&pstatus, containerName)
				if err != nil {
					if logV5 {
						klog.LoggerWithValues(rootLogger, "pod", klog.KObj(pod), "containerName", containerName).V(5).Info("skipping container; container status not found in pod status", "err", err)
					}
					failure = append(failure, reconciledContainer{pod.Name, containerName, ""})
					continue
				}

				if cstatus.State.Waiting != nil ||
					(cstatus.State.Waiting == nil && cstatus.State.Running == nil && cstatus.State.Terminated == nil) {
					if logV4 {
						klog.LoggerWithValues(rootLogger, "pod", klog.KObj(pod), "containerName", containerName).V(4).Info("skipping container; container still in the waiting state", "err", err)
					}
					failure = append(failure, reconciledContainer{pod.Name, containerName, ""})
					continue
				}

				m.Lock()
				if cstatus.State.Terminated != nil {
					// The container is terminated but we can't call m.RemoveContainer()
					// here because it could remove the allocated cpuset for the container
					// which may be in the process of being restarted.  That would result
					// in the container losing any exclusively-allocated CPUs that it
					// was allocated.
					_, _, err := m.containerMap.GetContainerRef(containerID)
					if err == nil && logV4 {
						klog.LoggerWithValues(rootLogger, "pod", klog.KObj(pod), "containerName", containerName).V(4).Info("ignoring terminated container", "containerID", containerID)
					}
					m.Unlock()
					continue
				}

				// Once we make it here we know we have a running container.
				// Idempotently add it to the containerMap incase it is missing.
				// This can happen after a kubelet restart, for example.
				m.containerMap.Add(podUID, containerName, containerID)

				cset := getCPUSetOrDefaultView(m.state, podUID, containerName)
				var baselineCPUs cpuset.CPUSet
				var hasBaselineCPUs bool
				if inPlaceExclusiveCPUs {
					baselineCPUs, hasBaselineCPUs = getBaselineCPUSetView(m.state, podUID, containerName)
				}
				m.Unlock()

				if cset.IsEmpty() {
					// NOTE: This should not happen outside of tests.
					if logV2 {
						klog.LoggerWithValues(rootLogger, "pod", klog.KObj(pod), "containerName", containerName).V(2).Info("ReconcileState: skipping container; empty cpuset assigned")
					}
					failure = append(failure, reconciledContainer{pod.Name, containerName, containerID})
					continue
				}

				if inPlaceExclusiveCPUs {
					// this is less clear than it should be because of the constraints of the GetCPUSetOrDefault API which should
					// be revisited separately. A container can only have baseline CPUs set if it got an exclusive CPU assignment
					// the first time it was admitted. Baseline CPUs reflects the CPUs promised at that time, which no following
					// scaleup/down can ever mutate. In that case, we are also guaranteed that the `GetCPUSetOrDefault` will return
					// _an_ exclusive CPUSet and never the default set. A clear improvement would be get rid of GetCPUSetOrDefault
					// and make a new API which returns TWO cpusets: the exclusively allocated and the shared set.
					if hasBaselineCPUs && !baselineCPUs.Equals(cset) && logV4 {
						klog.LoggerWithValues(rootLogger, "pod", klog.KObj(pod), "containerName", containerName).V(4).Info("Current container CPU assignments differ from CPU assignments recorded at admission time",
							"currentCPUSet", cset, "baselineCPUSet", baselineCPUs)
					}
				}

				lcset := getCPUSetOrDefaultView(m.lastUpdateState, podUID, containerName)
				if !cset.Equals(lcset) {
					if logV5 {
						klog.LoggerWithValues(rootLogger, "pod", klog.KObj(pod), "containerName", containerName).V(5).Info("updating container", "containerID", containerID, "cpuSet", cset)
					}
					err = m.updateContainerCPUSet(ctx, containerID, cset)
					if err != nil {
						klog.LoggerWithValues(rootLogger, "pod", klog.KObj(pod), "containerName", containerName).Error(err, "failed to update container", "containerID", containerID, "cpuSet", cset)
						failure = append(failure, reconciledContainer{pod.Name, containerName, containerID})
						continue
					}
					m.lastUpdateState.SetCPUSet(podUID, containerName, cset)
				}
				success = append(success, reconciledContainer{pod.Name, containerName, containerID})
			}
		}
	}
	return success, failure
}

func findContainerIDByName(status *v1.PodStatus, name string) (string, error) {
	for i := range status.InitContainerStatuses {
		container := &status.InitContainerStatuses[i]
		if container.Name == name && container.ContainerID != "" {
			var cid kubecontainer.ContainerID
			err := cid.ParseString(container.ContainerID)
			if err != nil {
				return "", err
			}
			return cid.ID, nil
		}
	}
	for i := range status.ContainerStatuses {
		container := &status.ContainerStatuses[i]
		if container.Name == name && container.ContainerID != "" {
			var cid kubecontainer.ContainerID
			err := cid.ParseString(container.ContainerID)
			if err != nil {
				return "", err
			}
			return cid.ID, nil
		}
	}
	return "", fmt.Errorf("unable to find ID for container with name %v in pod status (it may not be running)", name)
}

func findContainerStatusByName(status *v1.PodStatus, name string) (*v1.ContainerStatus, error) {
	for i := range status.InitContainerStatuses {
		if status.InitContainerStatuses[i].Name == name {
			return &status.InitContainerStatuses[i], nil
		}
	}
	for i := range status.ContainerStatuses {
		if status.ContainerStatuses[i].Name == name {
			return &status.ContainerStatuses[i], nil
		}
	}
	return nil, fmt.Errorf("unable to find status for container with name %v in pod status (it may not be running)", name)
}

func (m *manager) GetExclusiveCPUs(podUID, containerName string) cpuset.CPUSet {
	if result, ok := m.state.GetCPUSet(podUID, containerName); ok {
		return result
	}
	return cpuset.New()
}

func (m *manager) GetPodCPUs(podUID string) cpuset.CPUSet {
	if result, ok := m.state.GetPodCPUSet(podUID); ok {
		return result
	}
	return cpuset.New()
}

func (m *manager) GetCPUAffinity(podUID, containerName string) cpuset.CPUSet {
	return m.state.GetCPUSetOrDefault(podUID, containerName)
}

func resourcesQualifyForExclusiveCPUs(container *v1.Container) bool {
	if !cmqos.IsContainerEquivalentQOSGuaranteed(container) {
		return false
	}

	cpuLimit := container.Resources.Limits[v1.ResourceCPU]
	return cpuLimit.Value()*1000 == cpuLimit.MilliValue()
}

func (m *manager) GetResourceIsolationLevel(pod *v1.Pod, container *v1.Container) cmqos.ResourceIsolationLevel {
	if _, ok := getCPUSetView(m.state, string(pod.UID), container.Name); !ok {
		return cmqos.ResourceIsolationHost
	}

	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.PodLevelResourceManagers) && resourcehelper.IsPodLevelResourcesSet(pod) && !resourcesQualifyForExclusiveCPUs(container) {
		return cmqos.ResourceIsolationPod
	}

	return cmqos.ResourceIsolationContainer
}

func (m *manager) CanAllocateExclusively(res v1.ResourceName) bool {
	if res != v1.ResourceCPU {
		return false
	}
	return m.policy.CanAllocateExclusively()
}
