/*
Copyright 2014 The Kubernetes Authors.

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

//go:generate mockery
package status

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	clientset "k8s.io/client-go/kubernetes"

	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/features"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	kubeutil "k8s.io/kubernetes/pkg/kubelet/util"
	statusutil "k8s.io/kubernetes/pkg/util/pod"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
)

// A wrapper around v1.PodStatus that includes a version to enforce that stale pod statuses are
// not sent to the API server.
type versionedPodStatus struct {
	// version is a monotonically increasing version number (per pod).
	version uint64
	// Pod name & namespace, for sending updates to API server.
	podName      string
	podNamespace string
	// at is the time at which the most recent status update was detected
	at time.Time

	// True if the status is generated at the end of SyncTerminatedPod, or after it is completed.
	podIsFinished bool

	status v1.PodStatus
}

// Updates pod statuses in apiserver. Writes only when new status has changed.
// All methods are thread-safe.
type manager struct {
	kubeClient clientset.Interface
	podManager PodManager
	// Map from pod UID to sync status of the corresponding pod.
	podStatuses         map[types.UID]versionedPodStatus
	podResizeConditions map[types.UID]podResizeConditions
	podStatusesLock     sync.RWMutex
	podStatusChannel    chan struct{}
	// Map from (mirror) pod UID to latest status version successfully sent to the API server.
	// apiStatusVersions must only be accessed from the sync thread.
	apiStatusVersions map[kubetypes.MirrorPodUID]uint64
	podDeletionSafety PodDeletionSafetyProvider

	podStartupLatencyHelper PodStartupLatencyStateHelper
	notifiers               []PodUpdateNotifier
}

type podResizeConditions struct {
	PodResizePending    *v1.PodCondition
	PodResizeInProgress *v1.PodCondition
}

func (prc podResizeConditions) List() []*v1.PodCondition {
	var conditions []*v1.PodCondition
	if prc.PodResizePending != nil {
		conditions = append(conditions, prc.PodResizePending)
	}
	if prc.PodResizeInProgress != nil {
		conditions = append(conditions, prc.PodResizeInProgress)
	}
	return conditions
}

// PodManager is the subset of methods the manager needs to observe the actual state of the kubelet.
// See pkg/k8s.io/kubernetes/pkg/kubelet/pod.Manager for method godoc.
type PodManager interface {
	GetPodByUID(types.UID) (*v1.Pod, bool)
	GetMirrorPodByPod(*v1.Pod) (*v1.Pod, bool)
	TranslatePodUID(uid types.UID) kubetypes.ResolvedPodUID
	GetUIDTranslations() (podToMirror map[kubetypes.ResolvedPodUID]kubetypes.MirrorPodUID, mirrorToPod map[kubetypes.MirrorPodUID]kubetypes.ResolvedPodUID)
}

// PodStatusProvider knows how to provide status for a pod. It is intended to be used by other components
// that need to introspect the authoritative status of a pod.  The PodStatusProvider represents the actual
// status of a running pod as the kubelet sees it.
type PodStatusProvider interface {
	// GetPodStatus returns the cached status for the provided pod UID, as well as whether it
	// was a cache hit.
	GetPodStatus(uid types.UID) (v1.PodStatus, bool)
}

// PodDeletionSafetyProvider provides guarantees that a pod can be safely deleted.
type PodDeletionSafetyProvider interface {
	// PodCouldHaveRunningContainers returns true if the pod could have running containers.
	PodCouldHaveRunningContainers(pod *v1.Pod) bool
}

type PodStartupLatencyStateHelper interface {
	RecordStatusUpdated(logger klog.Logger, pod *v1.Pod)
	DeletePodStartupState(podUID types.UID)
}

// PodUpdateNotifier is an interface for receiving pod status updates.
type PodUpdateNotifier interface {
	// OnPodUpdated is called when a pod's status is updated.
	OnPodUpdated(logger klog.Logger, pod *v1.Pod, status v1.PodStatus, isAdded bool)
	// OnPodRemoved is called when a pod is terminated.
	OnPodRemoved(logger klog.Logger, pod *v1.Pod)
}

type podStatusNotification struct {
	pod           *v1.Pod
	status        v1.PodStatus
	isAdded       bool
	podIsFinished bool
}

// Manager is the Source of truth for kubelet pod status, and should be kept up-to-date with
// the latest v1.PodStatus. It also syncs updates back to the API server.
type Manager interface {
	PodStatusProvider

	// Start the API server status sync loop.
	Start(ctx context.Context)

	// AddPodUpdateNotifier adds a subscriber to receive pod status updates.
	AddPodUpdateNotifier(notifier PodUpdateNotifier)

	// SetPodStatus caches updates the cached status for the given pod, and triggers a status update.
	SetPodStatus(logger klog.Logger, pod *v1.Pod, status v1.PodStatus) (v1.PodStatus, bool)

	// SetContainerReadiness updates the cached container status with the given readiness, and
	// triggers a status update.
	SetContainerReadiness(logger klog.Logger, podUID types.UID, containerID kubecontainer.ContainerID, ready bool)

	// SetPodVolumeHealth updates the cached VolumeHealth entry for the given
	// volume name on the pod. conditions is the full desired condition set for
	// that volume (empty means healthy). Returns true if the cached status changed.
	SetPodVolumeHealth(logger klog.Logger, podUID types.UID, volumeName string, conditions []v1.VolumeHealthCondition) bool

	// SetContainerStartup updates the cached container status with the given startup, and
	// triggers a status update.
	SetContainerStartup(logger klog.Logger, podUID types.UID, containerID kubecontainer.ContainerID, started bool)

	// TerminatePod resets the container status for the provided pod to terminated and triggers
	// a status update.
	TerminatePod(logger klog.Logger, pod *v1.Pod)

	// RemoveOrphanedStatuses scans the status cache and removes any entries for pods not included in
	// the provided podUIDs.
	RemoveOrphanedStatuses(logger klog.Logger, podUIDs map[types.UID]bool)

	// GetPodResizeConditions returns cached PodStatus Resize conditions value
	GetPodResizeConditions(podUID types.UID) []*v1.PodCondition

	// SetPodResizePendingCondition caches the last PodResizePending condition for the pod.
	// It returns true if the condition reason or observedGeneration is modified as a result of this call.
	SetPodResizePendingCondition(podUID types.UID, reason, message string, observedGeneration int64) bool

	// SetPodResizeInProgressCondition caches the last PodResizeInProgress condition for the pod.
	// This function does not update observedGeneration if the condition already exists, nor does
	// it allow the reason or message to be cleared.
	// It returns true if the condition is modified as a result of this call, and it returns
	// the observedGeneration of the condition.
	SetPodResizeInProgressCondition(podUID types.UID, reason, message string, observedGeneration int64) (int64, bool)

	// ClearPodResizePendingCondition clears the PodResizePending condition for the pod from the cache.
	ClearPodResizePendingCondition(podUID types.UID, resolution metrics.DeferredResizeResolution)

	// ClearPodResizeInProgressCondition clears the PodResizeInProgress condition for the pod from the cache.
	// If the condition was cleared, it returns the observedGeneration of the cleared condition and true.
	// Otherwise it returns zero and false.
	ClearPodResizeInProgressCondition(podUID types.UID) (int64, bool)

	// IsPodResizeDeferred returns true if the pod resize is currently deferred.
	IsPodResizeDeferred(podUID types.UID) bool

	// IsPodResizeInfeasible returns true if the pod resize is infeasible.
	IsPodResizeInfeasible(podUID types.UID) bool

	// BackfillPodResizeConditions backfills the status manager's resize conditions by reading them from the
	// provided pods' statuses.
	BackfillPodResizeConditions(pods []*v1.Pod)
}

const syncPeriod = 10 * time.Second

// NewManager returns a functional Manager.
func NewManager(kubeClient clientset.Interface, podManager PodManager, podDeletionSafety PodDeletionSafetyProvider, podStartupLatencyHelper PodStartupLatencyStateHelper) Manager {
	return &manager{
		kubeClient:              kubeClient,
		podManager:              podManager,
		podStatuses:             make(map[types.UID]versionedPodStatus),
		podResizeConditions:     make(map[types.UID]podResizeConditions),
		podStatusChannel:        make(chan struct{}, 1),
		apiStatusVersions:       make(map[kubetypes.MirrorPodUID]uint64),
		podDeletionSafety:       podDeletionSafety,
		podStartupLatencyHelper: podStartupLatencyHelper,
	}
}

// isPodStatusByKubeletEqual returns true if the given pod statuses are equal, ignoring
// fields not managed by the kubelet (including non-kubelet-owned pod conditions,
// ResourceClaimStatuses, ExtendedResourceClaimStatus, and
// NodeAllocatableResourceClaimStatuses). Statuses are assumed to be
// normalized before calling this function.
func isPodStatusByKubeletEqual(oldStatus, status *v1.PodStatus) bool {
	if !kubeletConditionsEqual(oldStatus.Conditions, status.Conditions) {
		return false
	}
	return podStatusEqualExcludingIgnored(oldStatus, status, false, -1)
}

func isKubeletManagedCondition(condType v1.PodConditionType) bool {
	return kubetypes.PodConditionByKubelet(condType) || kubetypes.PodConditionSharedByKubelet(condType)
}

func findLastCondition(conditions []v1.PodCondition, condType v1.PodConditionType) *v1.PodCondition {
	for i := len(conditions) - 1; i >= 0; i-- {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

func kubeletConditionsEqual(oldConditions, newConditions []v1.PodCondition) bool {
	if len(oldConditions) == len(newConditions) {
		sameUniqueOrder := true
		for i := range newConditions {
			t := newConditions[i].Type
			if oldConditions[i].Type != t || findLastCondition(newConditions, t) != &newConditions[i] {
				sameUniqueOrder = false
				break
			}
		}
		if sameUniqueOrder {
			for i := range newConditions {
				nc := &newConditions[i]
				if isKubeletManagedCondition(nc.Type) {
					oc := &oldConditions[i]
					if oc.Status != nc.Status || oc.Message != nc.Message || oc.Reason != nc.Reason {
						return false
					}
				}
			}
			return true
		}
	}

	newCount := 0
	for i := range newConditions {
		nc := &newConditions[i]
		if !isKubeletManagedCondition(nc.Type) || findLastCondition(newConditions, nc.Type) != nc {
			continue
		}
		newCount++
		oc := findLastCondition(oldConditions, nc.Type)
		if oc == nil || oc.Status != nc.Status || oc.Message != nc.Message || oc.Reason != nc.Reason {
			return false
		}
	}

	oldCount := 0
	for i := range oldConditions {
		oc := &oldConditions[i]
		if isKubeletManagedCondition(oc.Type) && findLastCondition(oldConditions, oc.Type) == oc {
			oldCount++
		}
	}
	return newCount == oldCount
}

func podStatusEqualExcludingIgnored(oldStatus, status *v1.PodStatus, normalizeOld bool, bytesPerStatus int) bool {
	if oldStatus.Phase != status.Phase ||
		oldStatus.Message != status.Message ||
		oldStatus.Reason != status.Reason ||
		oldStatus.NominatedNodeName != status.NominatedNodeName ||
		oldStatus.HostIP != status.HostIP ||
		oldStatus.PodIP != status.PodIP ||
		oldStatus.QOSClass != status.QOSClass ||
		oldStatus.Resize != status.Resize ||
		oldStatus.ObservedGeneration != status.ObservedGeneration {
		return false
	}

	if (oldStatus.StartTime == nil) != (status.StartTime == nil) {
		return false
	}
	if oldStatus.StartTime != nil {
		if normalizeOld {
			if !normalizedTimeEqual(*oldStatus.StartTime, *status.StartTime) {
				return false
			}
		} else if !oldStatus.StartTime.Equal(status.StartTime) {
			return false
		}
	}

	if !slices.Equal(oldStatus.HostIPs, status.HostIPs) || !slices.Equal(oldStatus.PodIPs, status.PodIPs) {
		return false
	}

	if !containerStatusesEqual(oldStatus.ContainerStatuses, status.ContainerStatuses, normalizeOld, bytesPerStatus) ||
		!containerStatusesEqual(oldStatus.InitContainerStatuses, status.InitContainerStatuses, normalizeOld, bytesPerStatus) ||
		!containerStatusesEqual(oldStatus.EphemeralContainerStatuses, status.EphemeralContainerStatuses, normalizeOld, bytesPerStatus) {
		return false
	}

	if len(oldStatus.AllocatedResources) != len(status.AllocatedResources) ||
		(len(oldStatus.AllocatedResources) > 0 && !apiequality.Semantic.DeepEqual(oldStatus.AllocatedResources, status.AllocatedResources)) {
		return false
	}

	if (oldStatus.Resources == nil) != (status.Resources == nil) ||
		(oldStatus.Resources != nil && !apiequality.Semantic.DeepEqual(oldStatus.Resources, status.Resources)) {
		return false
	}

	if len(oldStatus.VolumeHealth) != len(status.VolumeHealth) ||
		(len(oldStatus.VolumeHealth) > 0 && !apiequality.Semantic.DeepEqual(oldStatus.VolumeHealth, status.VolumeHealth)) {
		return false
	}

	return true
}

func containerStatusesEqual(a, b []v1.ContainerStatus, normalizeA bool, bytesPerStatus int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := &a[i], &b[i]
		if ca.Name != cb.Name ||
			ca.Ready != cb.Ready ||
			ca.RestartCount != cb.RestartCount ||
			ca.Image != cb.Image ||
			ca.ImageID != cb.ImageID ||
			ca.ContainerID != cb.ContainerID {
			return false
		}
		if (ca.Started == nil) != (cb.Started == nil) || (ca.Started != nil && *ca.Started != *cb.Started) {
			return false
		}
		if (ca.StopSignal == nil) != (cb.StopSignal == nil) || (ca.StopSignal != nil && *ca.StopSignal != *cb.StopSignal) {
			return false
		}
		if !containerStateEqual(&ca.State, &cb.State, normalizeA, bytesPerStatus) ||
			!containerStateEqual(&ca.LastTerminationState, &cb.LastTerminationState, normalizeA, bytesPerStatus) {
			return false
		}
		if len(ca.AllocatedResources) != len(cb.AllocatedResources) ||
			(len(ca.AllocatedResources) > 0 && !apiequality.Semantic.DeepEqual(ca.AllocatedResources, cb.AllocatedResources)) {
			return false
		}
		if (ca.Resources == nil) != (cb.Resources == nil) ||
			(ca.Resources != nil && !apiequality.Semantic.DeepEqual(ca.Resources, cb.Resources)) {
			return false
		}
		if len(ca.VolumeMounts) != len(cb.VolumeMounts) ||
			(len(ca.VolumeMounts) > 0 && !apiequality.Semantic.DeepEqual(ca.VolumeMounts, cb.VolumeMounts)) {
			return false
		}
		if (ca.User == nil) != (cb.User == nil) ||
			(ca.User != nil && !apiequality.Semantic.DeepEqual(ca.User, cb.User)) {
			return false
		}
		if len(ca.AllocatedResourcesStatus) != len(cb.AllocatedResourcesStatus) ||
			(len(ca.AllocatedResourcesStatus) > 0 && !apiequality.Semantic.DeepEqual(ca.AllocatedResourcesStatus, cb.AllocatedResourcesStatus)) {
			return false
		}
	}
	return true
}

func containerStateEqual(a, b *v1.ContainerState, normalizeA bool, bytesPerStatus int) bool {
	if (a.Waiting == nil) != (b.Waiting == nil) || (a.Waiting != nil && *a.Waiting != *b.Waiting) {
		return false
	}
	if (a.Running == nil) != (b.Running == nil) {
		return false
	}
	if a.Running != nil {
		if normalizeA {
			if !normalizedTimeEqual(a.Running.StartedAt, b.Running.StartedAt) {
				return false
			}
		} else if !a.Running.StartedAt.Equal(&b.Running.StartedAt) {
			return false
		}
	}
	if (a.Terminated == nil) != (b.Terminated == nil) {
		return false
	}
	if a.Terminated != nil {
		ta, tb := a.Terminated, b.Terminated
		msgA := ta.Message
		if normalizeA && bytesPerStatus >= 0 && len(msgA) > bytesPerStatus {
			msgA = msgA[:bytesPerStatus]
		}
		if ta.ExitCode != tb.ExitCode ||
			ta.Signal != tb.Signal ||
			ta.Reason != tb.Reason ||
			msgA != tb.Message ||
			ta.ContainerID != tb.ContainerID {
			return false
		}
		if normalizeA {
			if !normalizedTimeEqual(ta.StartedAt, tb.StartedAt) || !normalizedTimeEqual(ta.FinishedAt, tb.FinishedAt) {
				return false
			}
		} else if !ta.StartedAt.Equal(&tb.StartedAt) || !ta.FinishedAt.Equal(&tb.FinishedAt) {
			return false
		}
	}
	return true
}

// Compile-time check ensuring podStatusEqualExcludingIgnored, containerStatusesEqual,
// and containerStateEqual stay exhaustive if new fields are added to core/v1 status types.
func _() {
	type podStatusFields struct {
		ObservedGeneration                   int64
		Phase                                v1.PodPhase
		Conditions                           []v1.PodCondition
		Message                              string
		Reason                               string
		NominatedNodeName                    string
		HostIP                               string
		HostIPs                              []v1.HostIP
		PodIP                                string
		PodIPs                               []v1.PodIP
		StartTime                            *metav1.Time
		InitContainerStatuses                []v1.ContainerStatus
		ContainerStatuses                    []v1.ContainerStatus
		QOSClass                             v1.PodQOSClass
		EphemeralContainerStatuses           []v1.ContainerStatus
		Resize                               v1.PodResizeStatus
		ResourceClaimStatuses                []v1.PodResourceClaimStatus
		ExtendedResourceClaimStatus          *v1.PodExtendedResourceClaimStatus
		AllocatedResources                   v1.ResourceList
		Resources                            *v1.ResourceRequirements
		NodeAllocatableResourceClaimStatuses []v1.NodeAllocatableResourceClaimStatus
		VolumeHealth                         []v1.PodVolumeHealth
	}
	type containerStatusFields struct {
		Name                     string
		State                    v1.ContainerState
		LastTerminationState     v1.ContainerState
		Ready                    bool
		RestartCount             int32
		Image                    string
		ImageID                  string
		ContainerID              string
		Started                  *bool
		AllocatedResources       v1.ResourceList
		Resources                *v1.ResourceRequirements
		VolumeMounts             []v1.VolumeMountStatus
		User                     *v1.ContainerUser
		AllocatedResourcesStatus []v1.ResourceStatus
		StopSignal               *v1.Signal
	}
	type containerStateFields struct {
		Waiting    *v1.ContainerStateWaiting
		Running    *v1.ContainerStateRunning
		Terminated *v1.ContainerStateTerminated
	}
	type containerStateTerminatedFields struct {
		ExitCode    int32
		Signal      int32
		Reason      string
		Message     string
		StartedAt   metav1.Time
		FinishedAt  metav1.Time
		ContainerID string
	}
	_ = podStatusFields(v1.PodStatus{})
	_ = containerStatusFields(v1.ContainerStatus{})
	_ = containerStateFields(v1.ContainerState{})
	_ = containerStateTerminatedFields(v1.ContainerStateTerminated{})
}

func normalizedMetav1Time(t metav1.Time) metav1.Time {
	if y := t.Year(); y < 0 || y > 9999 {
		return t.Rfc3339Copy()
	}
	return metav1.Time{Time: t.Time.UTC().Truncate(time.Second)}
}

func normalizedTimeEqual(a, b metav1.Time) bool {
	na := normalizedMetav1Time(a)
	nb := normalizedMetav1Time(b)
	return na.Equal(&nb)
}

func maxTerminationMessageBytesPerStatus(pod *v1.Pod) int {
	bytesPerStatus := kubecontainer.MaxPodTerminationMessageLogLength
	if containers := len(pod.Spec.Containers) + len(pod.Spec.InitContainers) + len(pod.Spec.EphemeralContainers); containers > 0 {
		bytesPerStatus = bytesPerStatus / containers
	}
	return bytesPerStatus
}

func isContainerStatusesSorted(statuses []v1.ContainerStatus) bool {
	for i := 1; i < len(statuses); i++ {
		if statuses[i].Name < statuses[i-1].Name {
			return false
		}
	}
	return true
}

func isInitContainerStatusesSorted(p *v1.Pod, statuses []v1.ContainerStatus) bool {
	if len(statuses) <= 1 {
		return true
	}
	current := 0
	for i := range p.Spec.InitContainers {
		name := p.Spec.InitContainers[i].Name
		for j := current; j < len(statuses); j++ {
			if name == statuses[j].Name {
				if j != current {
					return false
				}
				current++
				break
			}
		}
	}
	return true
}

func (m *manager) Start(ctx context.Context) {
	logger := klog.FromContext(ctx)
	// Don't start the status manager if we don't have a client. This will happen
	// on the master, where the kubelet is responsible for bootstrapping the pods
	// of the master components.
	if m.kubeClient == nil {
		logger.Info("Kubernetes client is nil, not starting status manager")
		return
	}

	logger.Info("Starting to sync pod status with apiserver")

	//nolint:staticcheck // SA1015 Ticker can leak since this is only called once and doesn't handle termination.
	syncTicker := time.NewTicker(syncPeriod).C

	// syncPod and syncBatch share the same go routine to avoid sync races.
	go wait.Forever(func() {
		for {
			select {
			case <-m.podStatusChannel:
				logger.V(4).Info("Syncing updated statuses")
				m.syncBatch(ctx, false)
			case <-syncTicker:
				logger.V(4).Info("Syncing all statuses")
				m.syncBatch(ctx, true)
			}
		}
	}, 0)
}

func (m *manager) AddPodUpdateNotifier(notifier PodUpdateNotifier) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()
	m.notifiers = append(m.notifiers, notifier)
}

// GetPodResizeConditions returns the last cached ResizeStatus value.
func (m *manager) GetPodResizeConditions(podUID types.UID) []*v1.PodCondition {
	m.podStatusesLock.RLock()
	defer m.podStatusesLock.RUnlock()
	return m.podResizeConditions[podUID].List()
}

// SetPodResizePendingCondition caches the last PodResizePending condition for the pod.
// It returns true if the condition reason or observedGeneration is modified as a result of this call.
func (m *manager) SetPodResizePendingCondition(podUID types.UID, reason, message string, observedGeneration int64) bool {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	previousCondition := m.podResizeConditions[podUID].PodResizePending

	if reason != v1.PodReasonInfeasible {
		// For all other "pending" reasons, we set the reason to "Deferred".
		reason = v1.PodReasonDeferred
	}

	m.podResizeConditions[podUID] = podResizeConditions{
		PodResizePending:    updatedPodResizeCondition(v1.PodResizePending, m.podResizeConditions[podUID].PodResizePending, reason, message, observedGeneration),
		PodResizeInProgress: m.podResizeConditions[podUID].PodResizeInProgress,
	}

	if previousCondition == nil {
		m.observePendingResizeCount()
		return true
	} else {
		return previousCondition.Reason != reason || previousCondition.ObservedGeneration != observedGeneration
	}
}

// This function does not update observedGeneration if the condition already exists, nor does
// it allow the reason or message to be cleared.
// It returns true if the condition is modified as a result of this call, and it returns
// the observedGeneration of the condition.
func (m *manager) SetPodResizeInProgressCondition(podUID types.UID, reason, message string, observedGeneration int64) (int64, bool) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	previousCondition := m.podResizeConditions[podUID].PodResizeInProgress
	if c := m.podResizeConditions[podUID].PodResizeInProgress; c != nil {
		// Preserve the old reason, message if they exist.
		if reason == "" && message == "" {
			reason = c.Reason
			message = c.Message
		}
		// The observedGeneration is always preserved. To update it, the caller must
		// clear the existing condition first.
		observedGeneration = c.ObservedGeneration
	}

	m.podResizeConditions[podUID] = podResizeConditions{
		PodResizeInProgress: updatedPodResizeCondition(v1.PodResizeInProgress, m.podResizeConditions[podUID].PodResizeInProgress, reason, message, observedGeneration),
		PodResizePending:    m.podResizeConditions[podUID].PodResizePending,
	}

	if previousCondition == nil {
		m.recordInProgressResizeCount()
	} else {
		// Ignore lastProbeTime when comparing conditions.
		previousCondition.LastProbeTime = m.podResizeConditions[podUID].PodResizeInProgress.LastProbeTime
		previousCondition.LastTransitionTime = m.podResizeConditions[podUID].PodResizeInProgress.LastTransitionTime
	}

	return observedGeneration, !reflect.DeepEqual(previousCondition, m.podResizeConditions[podUID].PodResizeInProgress)
}

func (m *manager) observeDeferredResizeDuration(podUID types.UID, cond *v1.PodCondition, resolution metrics.DeferredResizeResolution) {
	if cond == nil || cond.Reason != v1.PodReasonDeferred || cond.LastTransitionTime.IsZero() {
		return
	}
	// If the pod was already removed from the pod manager (e.g. during pod deletion cleanup),
	// GetPodByUID will return nil. GetPriorityBucketLabel(nil) will return PriorityBucketUnknown ("unknown").
	pod, _ := m.podManager.GetPodByUID(podUID)
	priorityBucket := metrics.GetPriorityBucketLabel(pod)
	duration := time.Since(cond.LastTransitionTime.Time).Seconds()
	metrics.PodDeferredResizeDurationSeconds.WithLabelValues(string(resolution), string(priorityBucket)).Observe(duration)
}

// ClearPodResizePendingCondition clears the PodResizePending condition for the pod from the cache.
func (m *manager) ClearPodResizePendingCondition(podUID types.UID, resolution metrics.DeferredResizeResolution) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	cond := m.podResizeConditions[podUID].PodResizePending
	if cond == nil {
		return
	}

	m.observeDeferredResizeDuration(podUID, cond, resolution)

	m.podResizeConditions[podUID] = podResizeConditions{
		PodResizeInProgress: m.podResizeConditions[podUID].PodResizeInProgress,
		PodResizePending:    nil,
	}

	m.observePendingResizeCount()
}

// ClearPodResizeInProgressCondition clears the PodResizeInProgress condition for the pod from the cache.
// If the condition was cleared, it returns the observedGeneration of the cleared condition and true.
// Otherwise it returns zero and false.
func (m *manager) ClearPodResizeInProgressCondition(podUID types.UID) (int64, bool) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	if m.podResizeConditions[podUID].PodResizeInProgress == nil {
		return 0, false
	}

	generation := m.podResizeConditions[podUID].PodResizeInProgress.ObservedGeneration
	m.podResizeConditions[podUID] = podResizeConditions{
		PodResizePending:    m.podResizeConditions[podUID].PodResizePending,
		PodResizeInProgress: nil,
	}

	m.recordInProgressResizeCount()
	return generation, true
}

func (m *manager) BackfillPodResizeConditions(pods []*v1.Pod) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	for _, pod := range pods {
		for _, c := range pod.Status.Conditions {
			switch c.Type {
			case v1.PodResizePending:
				newCondition := updatedPodResizeCondition(v1.PodResizePending, nil, c.Reason, c.Message, c.ObservedGeneration)
				oldConditions := m.podResizeConditions[pod.UID]

				m.podResizeConditions[pod.UID] = podResizeConditions{
					PodResizePending:    newCondition,
					PodResizeInProgress: oldConditions.PodResizeInProgress,
				}

			case v1.PodResizeInProgress:
				newCondition := updatedPodResizeCondition(v1.PodResizeInProgress, nil, c.Reason, c.Message, c.ObservedGeneration)
				oldConditions := m.podResizeConditions[pod.UID]

				m.podResizeConditions[pod.UID] = podResizeConditions{
					PodResizeInProgress: newCondition,
					PodResizePending:    oldConditions.PodResizePending,
				}
			}
		}
	}
	m.observePendingResizeCount()
	m.recordInProgressResizeCount()
}

// IsPodResizeDeferred returns true if the pod resize is currently deferred.
func (m *manager) IsPodResizeDeferred(podUID types.UID) bool {
	m.podStatusesLock.RLock()
	defer m.podStatusesLock.RUnlock()

	return m.podResizeConditions[podUID].PodResizePending != nil && m.podResizeConditions[podUID].PodResizePending.Reason == v1.PodReasonDeferred
}

// IsPodResizeInfeasible returns true if the pod resize is currently infeasible.
func (m *manager) IsPodResizeInfeasible(podUID types.UID) bool {
	m.podStatusesLock.RLock()
	defer m.podStatusesLock.RUnlock()

	return m.podResizeConditions[podUID].PodResizePending != nil && m.podResizeConditions[podUID].PodResizePending.Reason == v1.PodReasonInfeasible
}

func (m *manager) GetPodStatus(uid types.UID) (v1.PodStatus, bool) {
	m.podStatusesLock.RLock()
	defer m.podStatusesLock.RUnlock()
	status, ok := m.podStatuses[types.UID(m.podManager.TranslatePodUID(uid))]
	return status.status, ok
}

// isPodStatusUnchangedBeforeCopy checks whether a steady-state SetPodStatus call
// would normalize to the exact status already cached for pod.UID, avoiding an
// unnecessary PodStatus.DeepCopy() when nothing managed by kubelet has changed.
func isPodStatusUnchangedBeforeCopy(logger klog.Logger, pod *v1.Pod, cachedStatus *versionedPodStatus, status *v1.PodStatus) bool {
	if pod.DeletionTimestamp != nil ||
		(!kubetypes.IsStaticPod(pod) && cachedStatus.podIsFinished) ||
		cachedStatus.status.StartTime == nil || cachedStatus.status.StartTime.IsZero() ||
		logger.V(5).Enabled() ||
		!isContainerStatusesSorted(status.ContainerStatuses) ||
		!isInitContainerStatusesSorted(pod, status.InitContainerStatuses) ||
		!isContainerStatusesSorted(status.EphemeralContainerStatuses) {
		return false
	}
	candidate := *status
	candidate.ObservedGeneration = max(podutil.CalculatePodStatusObservedGeneration(pod), cachedStatus.status.ObservedGeneration)
	candidate.StartTime = cachedStatus.status.StartTime
	if len(cachedStatus.status.VolumeHealth) > 0 && len(candidate.VolumeHealth) == 0 {
		candidate.VolumeHealth = cachedStatus.status.VolumeHealth
	}
	return kubeletConditionsEqual(cachedStatus.status.Conditions, candidate.Conditions) &&
		podStatusEqualExcludingIgnored(&candidate, &cachedStatus.status, true, maxTerminationMessageBytesPerStatus(pod))
}

func (m *manager) SetPodStatus(logger klog.Logger, pod *v1.Pod, status v1.PodStatus) (v1.PodStatus, bool) {
	var notification *podStatusNotification
	defer func() {
		if notification != nil {
			m.sendNotification(logger, notification)
		}
	}()

	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	if cachedStatus, isCached := m.podStatuses[pod.UID]; isCached && isPodStatusUnchangedBeforeCopy(logger, pod, &cachedStatus, &status) {
		if loggerV := logger.V(3); loggerV.Enabled() {
			loggerV.Info("Ignoring same status for pod", "pod", klog.KObj(pod), "status", status)
		}
		return cachedStatus.status, false
	}

	// Make sure we're caching a deep copy.
	status = *status.DeepCopy()

	// Set the observedGeneration for this pod status.
	status.ObservedGeneration = podutil.CalculatePodStatusObservedGeneration(pod)

	// Force a status update if deletion timestamp is set. This is necessary
	// because if the pod is in the non-running state, the pod worker still
	// needs to be able to trigger an update and/or deletion.
	changed, notif := m.updateStatusInternal(logger, pod, status, pod.DeletionTimestamp != nil, false)
	notification = notif

	return m.podStatuses[pod.UID].status, changed
}

func (m *manager) SetContainerReadiness(logger klog.Logger, podUID types.UID, containerID kubecontainer.ContainerID, ready bool) {
	var notification *podStatusNotification
	defer func() {
		if notification != nil {
			m.sendNotification(logger, notification)
		}
	}()

	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	pod, ok := m.podManager.GetPodByUID(podUID)
	if !ok {
		if loggerV := logger.V(4); loggerV.Enabled() {
			loggerV.Info("Pod has been deleted, no need to update readiness", "podUID", podUID)
		}
		return
	}

	oldStatus, found := m.podStatuses[pod.UID]
	if !found {
		logger.Info("Container readiness changed before pod has synced",
			"pod", klog.KObj(pod),
			"containerID", containerID.String())
		return
	}

	// Find the container to update.
	containerStatus, idx, isInit, ok := findContainerStatusByID(&oldStatus.status, containerID)
	if !ok {
		logger.Info("Container readiness changed for unknown container",
			"pod", klog.KObj(pod),
			"containerID", containerID.String())
		return
	}

	if containerStatus.Ready == ready {
		if loggerV := logger.V(4); loggerV.Enabled() {
			loggerV.Info("Container readiness unchanged",
				"ready", ready,
				"pod", klog.KObj(pod),
				"containerID", containerID.String())
		}
		return
	}

	// Make sure we're not updating the cached version.
	status := *oldStatus.status.DeepCopy()
	if isInit {
		status.InitContainerStatuses[idx].Ready = ready
	} else {
		status.ContainerStatuses[idx].Ready = ready
	}

	// updateConditionFunc updates the corresponding type of condition
	updateConditionFunc := func(conditionType v1.PodConditionType, condition v1.PodCondition) {
		conditionIndex := -1
		for i, condition := range status.Conditions {
			if condition.Type == conditionType {
				conditionIndex = i
				break
			}
		}
		if conditionIndex != -1 {
			status.Conditions[conditionIndex] = condition
		} else {
			logger.Info("PodStatus missing condition type", "conditionType", conditionType, "status", status)
			status.Conditions = append(status.Conditions, condition)
		}
	}

	var allContainerStatuses []v1.ContainerStatus
	if len(status.InitContainerStatuses) == 0 || !kubetypes.HasRestartableInitContainer(pod) {
		allContainerStatuses = status.ContainerStatuses
	} else if len(status.ContainerStatuses) == 0 {
		allContainerStatuses = status.InitContainerStatuses
	} else {
		allContainerStatuses = make([]v1.ContainerStatus, 0, len(status.InitContainerStatuses)+len(status.ContainerStatuses))
		allContainerStatuses = append(allContainerStatuses, status.InitContainerStatuses...)
		allContainerStatuses = append(allContainerStatuses, status.ContainerStatuses...)
	}
	containersReady := GenerateContainersReadyCondition(pod, &oldStatus.status, allContainerStatuses, status.Phase)
	podReady := generatePodReadyConditionFromContainersReady(pod, &oldStatus.status, status.Conditions, containersReady)
	updateConditionFunc(v1.PodReady, podReady)
	updateConditionFunc(v1.ContainersReady, containersReady)
	_, notification = m.updateStatusInternal(logger, pod, status, false, false)
}

func (m *manager) SetPodVolumeHealth(logger klog.Logger, podUID types.UID, volumeName string, conditions []v1.VolumeHealthCondition) bool {
	var notification *podStatusNotification
	defer func() {
		if notification != nil {
			m.sendNotification(logger, notification)
		}
	}()

	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	pod, ok := m.podManager.GetPodByUID(podUID)
	if !ok {
		logger.V(4).Info("Pod has been deleted, no need to update volume health", "podUID", podUID)
		return false
	}

	oldStatus, found := m.podStatuses[pod.UID]
	if !found {
		logger.Info("Volume health changed before pod has synced",
			"pod", klog.KObj(pod),
			"volumeName", volumeName)
		return false
	}

	entryIndex := -1
	var existingConditions []v1.VolumeHealthCondition
	for i := range oldStatus.status.VolumeHealth {
		if oldStatus.status.VolumeHealth[i].Name == volumeName {
			entryIndex = i
			existingConditions = oldStatus.status.VolumeHealth[i].HealthConditions
			break
		}
	}

	// PATCH only when the (status, reason) set differs; message-only changes are suppressed.
	if volumeutil.VolumeHealthConditionSetsEqual(existingConditions, conditions) {
		logger.V(4).Info("Volume health unchanged",
			"pod", klog.KObj(pod),
			"volumeName", volumeName)
		return false
	}

	// Make sure we're not updating the cached version.
	status := *oldStatus.status.DeepCopy()
	newConditions := append([]v1.VolumeHealthCondition(nil), conditions...)
	now := metav1.Now()
	if entryIndex >= 0 {
		status.VolumeHealth[entryIndex].HealthConditions = newConditions
		status.VolumeHealth[entryIndex].LastTransitionTime = now
	} else {
		status.VolumeHealth = append(status.VolumeHealth, v1.PodVolumeHealth{
			Name:               volumeName,
			HealthConditions:   newConditions,
			LastTransitionTime: now,
		})
	}

	changed, notif := m.updateStatusInternal(logger, pod, status, false, false)
	notification = notif
	return changed
}

func (m *manager) SetContainerStartup(logger klog.Logger, podUID types.UID, containerID kubecontainer.ContainerID, started bool) {
	var notification *podStatusNotification
	defer func() {
		if notification != nil {
			m.sendNotification(logger, notification)
		}
	}()

	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	pod, ok := m.podManager.GetPodByUID(podUID)
	if !ok {
		logger.V(4).Info("Pod has been deleted, no need to update startup", "podUID", string(podUID))
		return
	}

	oldStatus, found := m.podStatuses[pod.UID]
	if !found {
		logger.Info("Container startup changed before pod has synced",
			"pod", klog.KObj(pod),
			"containerID", containerID.String())
		return
	}

	// Find the container to update.
	containerStatus, _, ok := findContainerStatus(&oldStatus.status, containerID.String())
	if !ok {
		logger.Info("Container startup changed for unknown container",
			"pod", klog.KObj(pod),
			"containerID", containerID.String())
		return
	}

	if containerStatus.Started != nil && *containerStatus.Started == started {
		logger.V(4).Info("Container startup unchanged",
			"pod", klog.KObj(pod),
			"containerID", containerID.String())
		return
	}

	// Make sure we're not updating the cached version.
	status := *oldStatus.status.DeepCopy()
	containerStatus, _, _ = findContainerStatus(&status, containerID.String())
	containerStatus.Started = &started

	_, notification = m.updateStatusInternal(logger, pod, status, false, false)
}

func findContainerStatus(status *v1.PodStatus, containerID string) (containerStatus *v1.ContainerStatus, init bool, ok bool) {
	// Find the container to update.
	for i := range status.ContainerStatuses {
		if status.ContainerStatuses[i].ContainerID == containerID {
			return &status.ContainerStatuses[i], false, true
		}
	}

	for i := range status.InitContainerStatuses {
		if status.InitContainerStatuses[i].ContainerID == containerID {
			return &status.InitContainerStatuses[i], true, true
		}
	}

	return nil, false, false
}

func findContainerStatusByID(status *v1.PodStatus, containerID kubecontainer.ContainerID) (containerStatus *v1.ContainerStatus, idx int, init bool, ok bool) {
	for i := range status.ContainerStatuses {
		if matchesContainerID(status.ContainerStatuses[i].ContainerID, containerID) {
			return &status.ContainerStatuses[i], i, false, true
		}
	}
	for i := range status.InitContainerStatuses {
		if matchesContainerID(status.InitContainerStatuses[i].ContainerID, containerID) {
			return &status.InitContainerStatuses[i], i, true, true
		}
	}
	return nil, -1, false, false
}

func matchesContainerID(raw string, id kubecontainer.ContainerID) bool {
	typeLen := len(id.Type)
	if len(raw) != typeLen+3+len(id.ID) {
		return false
	}
	return strings.HasPrefix(raw, id.Type) && raw[typeLen:typeLen+3] == "://" && strings.HasSuffix(raw, id.ID)
}

// TerminatePod ensures that the status of containers is properly defaulted at the end of the pod
// lifecycle. As the Kubelet must reconcile with the container runtime to observe container status
// there is always the possibility we are unable to retrieve one or more container statuses due to
// garbage collection, admin action, or loss of temporary data on a restart. This method ensures
// that any absent container status is treated as a failure so that we do not incorrectly describe
// the pod as successful. If we have not yet initialized the pod in the presence of init containers,
// the init container failure status is sufficient to describe the pod as failing, and we do not need
// to override waiting containers (unless there is evidence the pod previously started those containers).
// It also makes sure that pods are transitioned to a terminal phase (Failed or Succeeded) before
// their deletion.
func (m *manager) TerminatePod(logger klog.Logger, pod *v1.Pod) {
	var notification *podStatusNotification
	defer func() {
		if notification != nil {
			m.sendNotification(logger, notification)
		}
	}()

	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	// ensure that all containers have a terminated state - because we do not know whether the container
	// was successful, always report an error
	oldStatus := &pod.Status
	cachedStatus, isCached := m.podStatuses[pod.UID]
	if isCached {
		oldStatus = &cachedStatus.status
	}
	status := *oldStatus.DeepCopy()

	// once a pod has initialized, any missing status is treated as a failure
	if hasPodInitialized(logger, pod) {
		for i := range status.ContainerStatuses {
			if status.ContainerStatuses[i].State.Terminated != nil {
				continue
			}
			status.ContainerStatuses[i].State = v1.ContainerState{
				Terminated: &v1.ContainerStateTerminated{
					Reason:   kubecontainer.ContainerReasonStatusUnknown,
					Message:  "The container could not be located when the pod was terminated",
					ExitCode: 137,
				},
			}
		}
	}

	// all but the final suffix of init containers which have no evidence of a container start are
	// marked as failed containers
	for i := range initializedContainers(status.InitContainerStatuses) {
		if status.InitContainerStatuses[i].State.Terminated != nil {
			continue
		}
		status.InitContainerStatuses[i].State = v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{
				Reason:   kubecontainer.ContainerReasonStatusUnknown,
				Message:  "The container could not be located when the pod was terminated",
				ExitCode: 137,
			},
		}
	}

	// Make sure all pods are transitioned to a terminal phase.
	// TODO(#116484): Also assign terminal phase to static an pods.
	if !kubetypes.IsStaticPod(pod) {
		switch status.Phase {
		case v1.PodSucceeded, v1.PodFailed:
			// do nothing, already terminal
		case v1.PodPending, v1.PodRunning:
			if status.Phase == v1.PodRunning && isCached {
				logger.Info("Terminal running pod should have already been marked as failed, programmer error", "pod", klog.KObj(pod), "podUID", pod.UID)
			}
			logger.V(3).Info("Marking terminal pod as failed", "oldPhase", status.Phase, "pod", klog.KObj(pod), "podUID", pod.UID)
			status.Phase = v1.PodFailed
		default:
			logger.Error(fmt.Errorf("unknown phase: %v", status.Phase), "Unknown phase, programmer error", "pod", klog.KObj(pod), "podUID", pod.UID)
			status.Phase = v1.PodFailed
		}
	}

	logger.V(5).Info("TerminatePod calling updateStatusInternal", "pod", klog.KObj(pod), "podUID", pod.UID)
	_, notification = m.updateStatusInternal(logger, pod, status, true, true)
}

// hasPodInitialized returns true if the pod has no evidence of ever starting a regular container, which
// implies those containers should not be transitioned to terminated status.
func hasPodInitialized(logger klog.Logger, pod *v1.Pod) bool {
	// a pod without init containers is always initialized
	if len(pod.Spec.InitContainers) == 0 {
		return true
	}
	// if any container has ever moved out of waiting state, the pod has initialized
	for _, status := range pod.Status.ContainerStatuses {
		if status.LastTerminationState.Terminated != nil || status.State.Waiting == nil {
			return true
		}
	}
	// if the last init container has ever completed with a zero exit code, the pod is initialized
	if l := len(pod.Status.InitContainerStatuses); l > 0 {
		container, ok := kubeutil.GetContainerByIndex(pod.Spec.InitContainers, pod.Status.InitContainerStatuses, l-1)
		if !ok {
			logger.V(4).Info("Mismatch between pod spec and status, likely programmer error", "pod", klog.KObj(pod), "containerName", container.Name)
			return false
		}

		containerStatus := pod.Status.InitContainerStatuses[l-1]
		if podutil.IsRestartableInitContainer(&container) {
			if containerStatus.State.Running != nil &&
				containerStatus.Started != nil && *containerStatus.Started {
				return true
			}
		} else { // regular init container
			if state := containerStatus.LastTerminationState; state.Terminated != nil && state.Terminated.ExitCode == 0 {
				return true
			}
			if state := containerStatus.State; state.Terminated != nil && state.Terminated.ExitCode == 0 {
				return true
			}
		}
	}
	// otherwise the pod has no record of being initialized
	return false
}

// initializedContainers returns all status except for suffix of containers that are in Waiting
// state, which is the set of containers that have attempted to start at least once. If all containers
// are Waiting, the first container is always returned.
func initializedContainers(containers []v1.ContainerStatus) []v1.ContainerStatus {
	for i := len(containers) - 1; i >= 0; i-- {
		if containers[i].State.Waiting == nil || containers[i].LastTerminationState.Terminated != nil {
			return containers[0 : i+1]
		}
	}
	// always return at least one container
	if len(containers) > 0 {
		return containers[0:1]
	}
	return nil
}

// checkContainerStateTransition ensures that no container is trying to transition
// from a terminated to non-terminated state, which is illegal and indicates a
// logical error in the kubelet.
func checkContainerStateTransition(oldStatuses, newStatuses *v1.PodStatus, podSpec *v1.PodSpec) error {
	// If we should always restart, containers are allowed to leave the terminated state
	if podSpec.RestartPolicy == v1.RestartPolicyAlways {
		return nil
	}
	// If the pod can restart in-place, allow all containers to leave the terminated state.
	if utilfeature.DefaultFeatureGate.Enabled(features.RestartAllContainersOnContainerExits) {
		if podutil.AllContainersCouldRestart(podSpec) {
			return nil
		}
	}
	for _, oldStatus := range oldStatuses.ContainerStatuses {
		// Skip any container that wasn't terminated
		if oldStatus.State.Terminated == nil {
			continue
		}
		// Skip any container that failed but is allowed to restart
		if oldStatus.State.Terminated.ExitCode != 0 && podSpec.RestartPolicy == v1.RestartPolicyOnFailure {
			continue
		}
		// Skip any container that is allowed to restart by container restart policy
		if utilfeature.DefaultFeatureGate.Enabled(features.ContainerRestartRules) {
			restartable := false
			for _, container := range podSpec.Containers {
				if container.Name == oldStatus.Name && podutil.ContainerShouldRestart(container, *podSpec, oldStatus.State.Terminated.ExitCode) {
					restartable = true
				}
			}
			if restartable {
				continue
			}
		}
		for _, newStatus := range newStatuses.ContainerStatuses {
			if oldStatus.Name == newStatus.Name && newStatus.State.Terminated == nil {
				return fmt.Errorf("terminated container %v attempted illegal transition to non-terminated state", newStatus.Name)
			}
		}
	}

	for i, oldStatus := range oldStatuses.InitContainerStatuses {
		initContainer, ok := kubeutil.GetContainerByIndex(podSpec.InitContainers, oldStatuses.InitContainerStatuses, i)
		if !ok {
			return fmt.Errorf("found mismatch between pod spec and status, container: %v", oldStatus.Name)
		}
		// Skip any restartable init container as it always is allowed to restart
		if podutil.IsRestartableInitContainer(&initContainer) {
			continue
		}
		// Skip any container that wasn't terminated
		if oldStatus.State.Terminated == nil {
			continue
		}
		// Skip any container that failed but is allowed to restart
		if oldStatus.State.Terminated.ExitCode != 0 && podSpec.RestartPolicy == v1.RestartPolicyOnFailure {
			continue
		}
		// Skip any container that is allowed to restart by container restart policy
		if utilfeature.DefaultFeatureGate.Enabled(features.ContainerRestartRules) {
			restartable := false
			for _, container := range podSpec.InitContainers {
				if container.Name == oldStatus.Name && podutil.ContainerShouldRestart(container, *podSpec, oldStatus.State.Terminated.ExitCode) {
					restartable = true
				}
			}
			if restartable {
				continue
			}
		}
		for _, newStatus := range newStatuses.InitContainerStatuses {
			if oldStatus.Name == newStatus.Name && newStatus.State.Terminated == nil {
				return fmt.Errorf("terminated init container %v attempted illegal transition to non-terminated state", newStatus.Name)
			}
		}
	}
	return nil
}

// updateStatusInternal updates the internal status cache, and queues an update to the api server if
// necessary.
// This method IS NOT THREAD SAFE and must be called from a locked function.
func (m *manager) updateStatusInternal(logger klog.Logger, pod *v1.Pod, status v1.PodStatus, forceUpdate, podIsFinished bool) (bool, *podStatusNotification) {
	var oldStatus v1.PodStatus
	cachedStatus, isCached := m.podStatuses[pod.UID]
	if isCached {
		oldStatus = cachedStatus.status
		// TODO(#116484): Also assign terminal phase to static pods.
		if !kubetypes.IsStaticPod(pod) {
			if cachedStatus.podIsFinished && !podIsFinished {
				logger.Info("Got unexpected podIsFinished=false, while podIsFinished=true in status cache, programmer error", "pod", klog.KObj(pod))
				podIsFinished = true
			}
		}
	} else if mirrorPod, ok := m.podManager.GetMirrorPodByPod(pod); ok {
		oldStatus = mirrorPod.Status
	} else {
		oldStatus = pod.Status
	}

	// Check for illegal state transition in containers
	if err := checkContainerStateTransition(&oldStatus, &status, &pod.Spec); err != nil {
		logger.Error(err, "Status update on pod aborted", "pod", klog.KObj(pod))
		return false, nil
	}

	// Set ContainersReadyCondition.LastTransitionTime.
	updateLastTransitionTime(&status, &oldStatus, v1.ContainersReady)

	// Set ReadyCondition.LastTransitionTime.
	updateLastTransitionTime(&status, &oldStatus, v1.PodReady)

	// Set InitializedCondition.LastTransitionTime.
	updateLastTransitionTime(&status, &oldStatus, v1.PodInitialized)

	// Set PodReadyToStartContainersCondition.LastTransitionTime.
	updateLastTransitionTime(&status, &oldStatus, v1.PodReadyToStartContainers)

	// Set PodScheduledCondition.LastTransitionTime.
	updateLastTransitionTime(&status, &oldStatus, v1.PodScheduled)

	// Set DisruptionTarget.LastTransitionTime.
	updateLastTransitionTime(&status, &oldStatus, v1.DisruptionTarget)

	// Set AllContainersRestarting.LastTransitionTime
	updateLastTransitionTime(&status, &oldStatus, v1.AllContainersRestarting)

	// ensure that the start time does not change across updates.
	preserveOldStartTime := false
	if oldStatus.StartTime != nil && !oldStatus.StartTime.IsZero() {
		status.StartTime = oldStatus.StartTime
		preserveOldStartTime = true
	} else if status.StartTime.IsZero() {
		// if the status has no start time, we need to set an initial time
		now := metav1.Now()
		status.StartTime = &now
	}

	// Preserve volume health across pod status updates. VolumeHealth is set
	// independently by the volume health manager and is not part of the
	// status generated from the container runtime.
	if len(oldStatus.VolumeHealth) > 0 && len(status.VolumeHealth) == 0 {
		status.VolumeHealth = oldStatus.VolumeHealth
	}

	// prevent sending unnecessary patches
	if oldStatus.ObservedGeneration > status.ObservedGeneration {
		status.ObservedGeneration = oldStatus.ObservedGeneration
	}

	normalizeStatus(pod, &status)

	// Perform some more extensive logging of container termination state to assist in
	// debugging production races (generally not needed).
	if loggerV := logger.V(5); loggerV.Enabled() {
		var containers []string
		for _, s := range append(append([]v1.ContainerStatus(nil), status.InitContainerStatuses...), status.ContainerStatuses...) {
			var current, previous string
			switch {
			case s.State.Running != nil:
				current = "running"
			case s.State.Waiting != nil:
				current = "waiting"
			case s.State.Terminated != nil:
				current = fmt.Sprintf("terminated=%d", s.State.Terminated.ExitCode)
			default:
				current = "unknown"
			}
			switch {
			case s.LastTerminationState.Running != nil:
				previous = "running"
			case s.LastTerminationState.Waiting != nil:
				previous = "waiting"
			case s.LastTerminationState.Terminated != nil:
				previous = fmt.Sprintf("terminated=%d", s.LastTerminationState.Terminated.ExitCode)
			default:
				previous = "<none>"
			}
			containers = append(containers, fmt.Sprintf("(%s state=%s previous=%s)", s.Name, current, previous))
		}
		sort.Strings(containers)
		loggerV.Info("updateStatusInternal", "version", cachedStatus.version+1, "podIsFinished", podIsFinished, "pod", klog.KObj(pod), "podUID", pod.UID, "containers", strings.Join(containers, " "))
	}

	// Increment container termination metric for newly terminated containers.
	// We only want to count terminations once per container per pod.
	// By comparing the new status to the old status before we apply it to the cache,
	// we can detect exactly when a container in this pod transitions to the
	// Terminated state and record the metric.
	checkAndRecordTerminations := func(oldStatuses, newStatuses []v1.ContainerStatus, containerType string) {
		for _, newCStat := range newStatuses {
			if newCStat.State.Terminated != nil {
				// Find the old status for this container.
				var oldCStat *v1.ContainerStatus
				for i := range oldStatuses {
					if oldStatuses[i].Name == newCStat.Name {
						oldCStat = &oldStatuses[i]
						break
					}
				}
				// If it wasn't terminated before (or is brand new and already terminated), record it.
				if oldCStat == nil || oldCStat.State.Terminated == nil {
					metrics.TerminatedContainersTotal.WithLabelValues(containerType, strconv.Itoa(int(newCStat.State.Terminated.ExitCode)), newCStat.State.Terminated.Reason).Inc()
				}
			}
		}
	}
	checkAndRecordTerminations(oldStatus.InitContainerStatuses, status.InitContainerStatuses, metrics.InitContainer)
	checkAndRecordTerminations(oldStatus.ContainerStatuses, status.ContainerStatuses, metrics.Container)
	checkAndRecordTerminations(oldStatus.EphemeralContainerStatuses, status.EphemeralContainerStatuses, metrics.EphemeralContainer)

	// The intent here is to prevent concurrent updates to a pod's status from
	// clobbering each other so the phase of a pod progresses monotonically.
	if isCached && isPodStatusByKubeletEqual(&cachedStatus.status, &status) && !forceUpdate {
		if loggerV := logger.V(3); loggerV.Enabled() {
			loggerV.Info("Ignoring same status for pod", "pod", klog.KObj(pod), "status", status)
		}
		return false, nil
	}

	if preserveOldStartTime && oldStatus.StartTime != nil {
		startTimeCopy := *oldStatus.StartTime
		status.StartTime = &startTimeCopy
	}

	newStatus := versionedPodStatus{
		status:        status,
		version:       cachedStatus.version + 1,
		podName:       pod.Name,
		podNamespace:  pod.Namespace,
		podIsFinished: podIsFinished,
	}

	// Multiple status updates can be generated before we update the API server,
	// so we track the time from the first status update until we retire it to
	// the API.
	if cachedStatus.at.IsZero() {
		newStatus.at = time.Now()
	} else {
		newStatus.at = cachedStatus.at
	}

	m.podStatuses[pod.UID] = newStatus

	select {
	case m.podStatusChannel <- struct{}{}:
	default:
		// there's already a status update pending
	}

	var notification *podStatusNotification
	if len(m.notifiers) > 0 {
		podCopy := *pod
		podCopy.Status = status

		notification = &podStatusNotification{
			pod:           &podCopy,
			status:        status,
			isAdded:       !isCached && pod.DeletionTimestamp == nil,
			podIsFinished: podIsFinished,
		}
	}

	return true, notification
}

func (m *manager) sendNotification(logger klog.Logger, n *podStatusNotification) {
	for _, notifier := range m.notifiers {
		if !n.podIsFinished {
			notifier.OnPodUpdated(logger, n.pod, n.status, n.isAdded)
		} else {
			notifier.OnPodRemoved(logger, n.pod)
		}
	}
}

// updateLastTransitionTime updates the LastTransitionTime of a pod condition.
func updateLastTransitionTime(status, oldStatus *v1.PodStatus, conditionType v1.PodConditionType) {
	_, condition := podutil.GetPodCondition(status, conditionType)
	if condition == nil {
		return
	}
	_, oldCondition := podutil.GetPodCondition(oldStatus, conditionType)
	if oldCondition != nil && condition.Status == oldCondition.Status {
		condition.LastTransitionTime = oldCondition.LastTransitionTime
		return
	}
	condition.LastTransitionTime = metav1.Now()
}

// deletePodStatus simply removes the given pod from the status cache.
func (m *manager) deletePodStatus(uid types.UID) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()
	delete(m.podStatuses, uid)
	m.podStartupLatencyHelper.DeletePodStartupState(uid)
	if utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScaling) {
		if conds, exists := m.podResizeConditions[uid]; exists {
			m.observeDeferredResizeDuration(uid, conds.PodResizePending, metrics.DeferredResizeResolutionTerminated)
			delete(m.podResizeConditions, uid)
			m.recordInProgressResizeCount()
			m.observePendingResizeCount()
		}
	}
}

// TODO(filipg): It'd be cleaner if we can do this without signal from user.
func (m *manager) RemoveOrphanedStatuses(logger klog.Logger, podUIDs map[types.UID]bool) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()
	for key := range m.podStatuses {
		if _, ok := podUIDs[key]; !ok {
			logger.V(5).Info("Removing pod from status map", "podUID", key)
			delete(m.podStatuses, key)
			if utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScaling) {
				if conds, exists := m.podResizeConditions[key]; exists {
					m.observeDeferredResizeDuration(key, conds.PodResizePending, metrics.DeferredResizeResolutionTerminated)
					delete(m.podResizeConditions, key)
					m.recordInProgressResizeCount()
					m.observePendingResizeCount()
				}
			}
		}
	}
}

type podSync struct {
	podUID    types.UID
	statusUID kubetypes.MirrorPodUID
	status    versionedPodStatus
}

var podSyncSlicePool = sync.Pool{
	New: func() any {
		s := make([]podSync, 0, 16)
		return &s
	},
}

// syncBatch syncs pods statuses with the apiserver. Returns the number of syncs
// attempted for testing.
func (m *manager) syncBatch(ctx context.Context, all bool) int {
	logger := klog.FromContext(ctx)

	updatedStatusesPtr := podSyncSlicePool.Get().(*[]podSync)
	updatedStatuses := (*updatedStatusesPtr)[:0]
	defer func() {
		for i := range updatedStatuses {
			updatedStatuses[i] = podSync{}
		}
		*updatedStatusesPtr = updatedStatuses[:0]
		podSyncSlicePool.Put(updatedStatusesPtr)
	}()

	podToMirror, mirrorToPod := m.podManager.GetUIDTranslations()
	func() { // Critical section
		m.podStatusesLock.RLock()
		defer m.podStatusesLock.RUnlock()

		// Clean up orphaned versions.
		if all {
			for uid := range m.apiStatusVersions {
				_, hasPod := m.podStatuses[types.UID(uid)]
				_, hasMirror := mirrorToPod[uid]
				if !hasPod && !hasMirror {
					delete(m.apiStatusVersions, uid)
				}
			}
		}

		// Decide which pods need status updates.
		for uid, status := range m.podStatuses {
			// translate the pod UID (source) to the status UID (API pod) -
			// static pods are identified in source by pod UID but tracked in the
			// API via the uid of the mirror pod
			uidOfStatus := kubetypes.MirrorPodUID(uid)
			if mirrorUID, ok := podToMirror[kubetypes.ResolvedPodUID(uid)]; ok {
				if mirrorUID == "" {
					logger.V(5).Info("Static pod does not have a corresponding mirror pod; skipping",
						"podUID", uid,
						"pod", klog.KRef(status.podNamespace, status.podName))
					continue
				}
				uidOfStatus = mirrorUID
			}

			// if a new status update has been delivered, trigger an update, otherwise the
			// pod can wait for the next bulk check (which performs reconciliation as well)
			if !all {
				if m.apiStatusVersions[uidOfStatus] >= status.version {
					continue
				}
				updatedStatuses = append(updatedStatuses, podSync{uid, uidOfStatus, status})
				continue
			}

			// Ensure that any new status, or mismatched status, or pod that is ready for
			// deletion gets updated. If a status update fails we retry the next time any
			// other pod is updated.
			if m.needsUpdatePtr(logger, types.UID(uidOfStatus), &status) {
				updatedStatuses = append(updatedStatuses, podSync{uid, uidOfStatus, status})
			} else if m.needsReconcilePtr(logger, uid, &status.status) {
				// Delete the apiStatusVersions here to force an update on the pod status
				// In most cases the deleted apiStatusVersions here should be filled
				// soon after the following syncPod() [If the syncPod() sync an update
				// successfully].
				delete(m.apiStatusVersions, uidOfStatus)
				updatedStatuses = append(updatedStatuses, podSync{uid, uidOfStatus, status})
			}
		}
	}()

	for i := range updatedStatuses {
		update := &updatedStatuses[i]
		logger.V(5).Info("Sync pod status", "podUID", update.podUID, "statusUID", update.statusUID, "version", update.status.version)
		m.syncPod(ctx, update.podUID, update.status)
	}

	return len(updatedStatuses)
}

// syncPod syncs the given status with the API server. The caller must not hold the status lock.
func (m *manager) syncPod(ctx context.Context, uid types.UID, status versionedPodStatus) {
	logger := klog.FromContext(ctx)
	// TODO: make me easier to express from client code
	pod, err := m.kubeClient.CoreV1().Pods(status.podNamespace).Get(ctx, status.podName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		logger.V(3).Info("Pod does not exist on the server",
			"podUID", uid,
			"pod", klog.KRef(status.podNamespace, status.podName))
		// If the Pod is deleted the status will be cleared in
		// RemoveOrphanedStatuses, so we just ignore the update here.
		return
	}
	if err != nil {
		logger.Error(err, "Failed to get status for pod",
			"podUID", uid,
			"pod", klog.KRef(status.podNamespace, status.podName))
		return
	}

	translatedUID := m.podManager.TranslatePodUID(pod.UID)
	// Type convert original uid just for the purpose of comparison.
	if len(translatedUID) > 0 && translatedUID != kubetypes.ResolvedPodUID(uid) {
		logger.V(2).Info("Pod was deleted and then recreated, skipping status update",
			"pod", klog.KObj(pod),
			"oldPodUID", uid,
			"podUID", translatedUID)
		m.deletePodStatus(uid)
		return
	}

	mergedStatus := mergePodStatus(pod, pod.Status, status.status, m.podDeletionSafety.PodCouldHaveRunningContainers(pod))

	newPod, patchBytes, unchanged, err := statusutil.PatchPodStatus(ctx, m.kubeClient, pod.Namespace, pod.Name, pod.UID, pod.Status, mergedStatus)
	if loggerV := logger.V(3); loggerV.Enabled() {
		loggerV.Info("Patch status for pod", "pod", klog.KObj(pod), "podUID", uid, "patch", string(patchBytes))
	}

	if err != nil {
		logger.Error(err, "Failed to update status for pod", "pod", klog.KObj(pod))
		return
	}
	if unchanged {
		if loggerV := logger.V(3); loggerV.Enabled() {
			loggerV.Info("Status for pod is up-to-date", "pod", klog.KObj(pod), "statusVersion", status.version)
		}
	} else {
		if loggerV := logger.V(3); loggerV.Enabled() {
			loggerV.Info("Status for pod updated successfully", "pod", klog.KObj(pod), "statusVersion", status.version, "status", mergedStatus)
		}
		pod = newPod
		// We pass a new object (result of API call which contains updated ResourceVersion)
		m.podStartupLatencyHelper.RecordStatusUpdated(logger, pod)
	}

	// measure how long the status update took to propagate from generation to update on the server
	if status.at.IsZero() {
		if loggerV := logger.V(3); loggerV.Enabled() {
			loggerV.Info("Pod had no status time set", "pod", klog.KObj(pod), "podUID", uid, "version", status.version)
		}
	} else {
		duration := time.Since(status.at).Truncate(time.Millisecond)
		metrics.PodStatusSyncDuration.Observe(duration.Seconds())
	}

	m.apiStatusVersions[kubetypes.MirrorPodUID(pod.UID)] = status.version

	// We don't handle graceful deletion of mirror pods.
	if m.canBeDeletedWithPhase(logger, pod, status.status.Phase, status.podIsFinished) {
		deleteOptions := metav1.DeleteOptions{
			GracePeriodSeconds: new(int64),
			// Use the pod UID as the precondition for deletion to prevent deleting a
			// newly created pod with the same name and namespace.
			Preconditions: metav1.NewUIDPreconditions(string(pod.UID)),
		}
		err = m.kubeClient.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, deleteOptions)
		if err != nil {
			logger.Info("Failed to delete status for pod", "pod", klog.KObj(pod), "err", err)
			return
		}
		if loggerV := logger.V(3); loggerV.Enabled() {
			loggerV.Info("Pod fully terminated and removed from etcd", "pod", klog.KObj(pod))
		}
		m.deletePodStatus(uid)
	}
}

// needsUpdate returns whether the status is stale for the given pod UID.
// This method is not thread safe, and must only be accessed by the sync thread.
func (m *manager) needsUpdate(logger klog.Logger, uid types.UID, status versionedPodStatus) bool {
	return m.needsUpdatePtr(logger, uid, &status)
}

func (m *manager) needsUpdatePtr(logger klog.Logger, uid types.UID, status *versionedPodStatus) bool {
	latest, ok := m.apiStatusVersions[kubetypes.MirrorPodUID(uid)]
	if !ok || latest < status.version {
		return true
	}
	pod, ok := m.podManager.GetPodByUID(uid)
	if !ok {
		return false
	}
	return m.canBeDeletedWithPhase(logger, pod, status.status.Phase, status.podIsFinished)
}

func (m *manager) canBeDeleted(logger klog.Logger, pod *v1.Pod, status v1.PodStatus, podIsFinished bool) bool {
	return m.canBeDeletedWithPhase(logger, pod, status.Phase, podIsFinished)
}

func (m *manager) canBeDeletedWithPhase(logger klog.Logger, pod *v1.Pod, localPhase v1.PodPhase, podIsFinished bool) bool {
	if pod.DeletionTimestamp == nil || kubetypes.IsMirrorPod(pod) {
		return false
	}
	// Delay deletion of pods until the phase is terminal, based on pod.Status
	// which comes from pod manager.
	if !podutil.IsPodPhaseTerminal(pod.Status.Phase) {
		// For debugging purposes we also log the kubelet's local phase, when the deletion is delayed.
		if loggerV := logger.V(3); loggerV.Enabled() {
			loggerV.Info("Delaying pod deletion as the phase is non-terminal", "phase", pod.Status.Phase, "localPhase", localPhase, "pod", klog.KObj(pod), "podUID", pod.UID)
		}
		return false
	}
	// If this is an update completing pod termination then we know the pod termination is finished.
	if podIsFinished {
		if loggerV := logger.V(3); loggerV.Enabled() {
			loggerV.Info("The pod termination is finished as SyncTerminatedPod completes its execution", "phase", pod.Status.Phase, "localPhase", localPhase, "pod", klog.KObj(pod), "podUID", pod.UID)
		}
		return true
	}
	return false
}

// needsReconcile compares the given status with the status in the pod manager (which
// in fact comes from apiserver), returns whether the status needs to be reconciled with
// the apiserver. Now when pod status is inconsistent between apiserver and kubelet,
// kubelet should forcibly send an update to reconcile the inconsistence, because kubelet
// should be the source of truth of pod status.
// NOTE(random-liu): It's simpler to pass in mirror pod uid and get mirror pod by uid, but
// now the pod manager only supports getting mirror pod by static pod, so we have to pass
// static pod uid here.
// TODO(random-liu): Simplify the logic when mirror pod manager is added.
func (m *manager) needsReconcile(logger klog.Logger, uid types.UID, status v1.PodStatus) bool {
	return m.needsReconcilePtr(logger, uid, &status)
}

func (m *manager) needsReconcilePtr(logger klog.Logger, uid types.UID, status *v1.PodStatus) bool {
	// The pod could be a static pod, so we should translate first.
	pod, ok := m.podManager.GetPodByUID(uid)
	if !ok {
		if loggerV := logger.V(4); loggerV.Enabled() {
			loggerV.Info("Pod has been deleted, no need to reconcile", "podUID", uid)
		}
		return false
	}
	// If the pod is a static pod, we should check its mirror pod, because only status in mirror pod is meaningful to us.
	if kubetypes.IsStaticPod(pod) {
		mirrorPod, ok := m.podManager.GetMirrorPodByPod(pod)
		if !ok {
			if loggerV := logger.V(4); loggerV.Enabled() {
				loggerV.Info("Static pod has no corresponding mirror pod, no need to reconcile", "pod", klog.KObj(pod))
			}
			return false
		}
		pod = mirrorPod
	}

	if isContainerStatusesSorted(pod.Status.ContainerStatuses) &&
		isInitContainerStatusesSorted(pod, pod.Status.InitContainerStatuses) &&
		isContainerStatusesSorted(pod.Status.EphemeralContainerStatuses) {
		if kubeletConditionsEqual(pod.Status.Conditions, status.Conditions) &&
			podStatusEqualExcludingIgnored(&pod.Status, status, true, maxTerminationMessageBytesPerStatus(pod)) {
			return false
		}
		if loggerV := logger.V(3); loggerV.Enabled() {
			podStatus := pod.Status.DeepCopy()
			normalizeStatus(pod, podStatus)
			statusCopy := *status
			loggerV.Info("Pod status is inconsistent with cached status for pod, a reconciliation should be triggered",
				"pod", klog.KObj(pod),
				"statusDiff", diff.Diff(podStatus, &statusCopy))
		}
		return true
	}

	podStatus := pod.Status.DeepCopy()
	normalizeStatus(pod, podStatus)

	if isPodStatusByKubeletEqual(podStatus, status) {
		// If the status from the source is the same with the cached status,
		// reconcile is not needed. Just return.
		return false
	}
	if loggerV := logger.V(3); loggerV.Enabled() {
		statusCopy := *status
		loggerV.Info("Pod status is inconsistent with cached status for pod, a reconciliation should be triggered",
			"pod", klog.KObj(pod),
			"statusDiff", diff.Diff(podStatus, &statusCopy))
	}

	return true
}

// normalizeStatus normalizes nanosecond precision timestamps in podStatus
// down to second precision (*RFC339NANO* -> *RFC3339*). This must be done
// before comparing podStatus to the status returned by apiserver because
// apiserver does not support RFC339NANO.
// Related issue #15262/PR #15263 to move apiserver to RFC339NANO is closed.
func normalizeStatus(pod *v1.Pod, status *v1.PodStatus) *v1.PodStatus {
	bytesPerStatus := maxTerminationMessageBytesPerStatus(pod)
	normalizeTimeStamp := func(t *metav1.Time) {
		*t = normalizedMetav1Time(*t)
	}
	normalizeContainerState := func(c *v1.ContainerState) {
		if c.Running != nil {
			normalizeTimeStamp(&c.Running.StartedAt)
		}
		if c.Terminated != nil {
			normalizeTimeStamp(&c.Terminated.StartedAt)
			normalizeTimeStamp(&c.Terminated.FinishedAt)
			if len(c.Terminated.Message) > bytesPerStatus {
				c.Terminated.Message = c.Terminated.Message[:bytesPerStatus]
			}
		}
	}

	if status.StartTime != nil {
		normalizeTimeStamp(status.StartTime)
	}
	for i := range status.Conditions {
		condition := &status.Conditions[i]
		normalizeTimeStamp(&condition.LastProbeTime)
		normalizeTimeStamp(&condition.LastTransitionTime)
	}

	normalizeContainerStatuses := func(containerStatuses []v1.ContainerStatus) {
		for i := range containerStatuses {
			cstatus := &containerStatuses[i]
			normalizeContainerState(&cstatus.State)
			normalizeContainerState(&cstatus.LastTerminationState)
		}
	}

	normalizeContainerStatuses(status.ContainerStatuses)
	if !isContainerStatusesSorted(status.ContainerStatuses) {
		sort.Sort(kubetypes.SortedContainerStatuses(status.ContainerStatuses))
	}

	normalizeContainerStatuses(status.InitContainerStatuses)
	if !isInitContainerStatusesSorted(pod, status.InitContainerStatuses) {
		kubetypes.SortInitContainerStatuses(pod, status.InitContainerStatuses)
	}

	normalizeContainerStatuses(status.EphemeralContainerStatuses)
	if !isContainerStatusesSorted(status.EphemeralContainerStatuses) {
		sort.Sort(kubetypes.SortedContainerStatuses(status.EphemeralContainerStatuses))
	}

	return status
}

// mergePodStatus merges oldPodStatus and newPodStatus to preserve where pod conditions
// not owned by kubelet and to ensure terminal phase transition only happens after all
// running containers have terminated. This method does not modify the old status.
func mergePodStatus(pod *v1.Pod, oldPodStatus, newPodStatus v1.PodStatus, couldHaveRunningContainers bool) v1.PodStatus {
	transitioningToTerminalPhase := !podutil.IsPodPhaseTerminal(oldPodStatus.Phase) && podutil.IsPodPhaseTerminal(newPodStatus.Phase)

	nonKubeletOldCount := 0
	for i := range oldPodStatus.Conditions {
		if !kubetypes.PodConditionByKubelet(oldPodStatus.Conditions[i].Type) {
			nonKubeletOldCount++
		}
	}

	needsConditionMerge := nonKubeletOldCount > 0 || podutil.IsPodPhaseTerminal(newPodStatus.Phase)
	if !needsConditionMerge {
		for i := range newPodStatus.Conditions {
			if !kubetypes.PodConditionByKubelet(newPodStatus.Conditions[i].Type) {
				needsConditionMerge = true
				break
			}
		}
	}

	if needsConditionMerge {
		podConditions := make([]v1.PodCondition, 0, nonKubeletOldCount+len(newPodStatus.Conditions))

		for _, c := range oldPodStatus.Conditions {
			if !kubetypes.PodConditionByKubelet(c.Type) {
				podConditions = append(podConditions, c)
			}
		}

		for _, c := range newPodStatus.Conditions {
			if kubetypes.PodConditionByKubelet(c.Type) {
				podConditions = append(podConditions, c)
			} else if kubetypes.PodConditionSharedByKubelet(c.Type) {
				// we replace or append all the "shared by kubelet" conditions
				if c.Type == v1.DisruptionTarget {
					// guard the update of the DisruptionTarget condition with a check to ensure
					// it will only be sent once all containers have terminated and the phase
					// is terminal. This avoids sending an unnecessary patch request to add
					// the condition if the actual status phase transition is delayed.
					if transitioningToTerminalPhase && !couldHaveRunningContainers {
						// update the LastTransitionTime again here because the older transition
						// time set in updateStatusInternal is likely stale as sending of
						// the condition was delayed until all pod's containers have terminated.
						if _, newCond := podutil.GetPodConditionFromList(newPodStatus.Conditions, c.Type); newCond != nil {
							cond := *newCond
							if _, oldCond := podutil.GetPodConditionFromList(oldPodStatus.Conditions, c.Type); oldCond != nil && cond.Status == oldCond.Status {
								cond.LastTransitionTime = oldCond.LastTransitionTime
							} else {
								cond.LastTransitionTime = metav1.Now()
							}
							// for shared conditions we update or append in podConditions
							podConditions = statusutil.ReplaceOrAppendPodCondition(podConditions, &cond)
						}
					}
				}
			}
		}
		newPodStatus.Conditions = podConditions
	}

	// ResourceClaimStatuses is not owned and not modified by kubelet.
	newPodStatus.ResourceClaimStatuses = oldPodStatus.ResourceClaimStatuses
	// ExtendedResourceClaimStatus is not owned and not modified by kubelet.
	newPodStatus.ExtendedResourceClaimStatus = oldPodStatus.ExtendedResourceClaimStatus
	// NodeAllocatableResourceClaimStatuses is not owned and not modified by kubelet.
	newPodStatus.NodeAllocatableResourceClaimStatuses = oldPodStatus.NodeAllocatableResourceClaimStatuses

	// Delay transitioning a pod to a terminal status unless the pod is actually terminal.
	// The Kubelet should never transition a pod to terminal status that could have running
	// containers and thus actively be leveraging exclusive resources. Note that resources
	// like volumes are reconciled by a subsystem in the Kubelet and will converge if a new
	// pod reuses an exclusive resource (unmount -> free -> mount), which means we do not
	// need wait for those resources to be detached by the Kubelet. In general, resources
	// the Kubelet exclusively owns must be released prior to a pod being reported terminal,
	// while resources that have participanting components above the API use the pod's
	// transition to a terminal phase (or full deletion) to release those resources.
	if transitioningToTerminalPhase {
		if couldHaveRunningContainers {
			newPodStatus.Phase = oldPodStatus.Phase
			newPodStatus.Reason = oldPodStatus.Reason
			newPodStatus.Message = oldPodStatus.Message
		}
	}

	// If the new phase is terminal, explicitly set the ready condition to false for v1.PodReady and v1.ContainersReady.
	// It may take some time for kubelet to reconcile the ready condition, so explicitly set ready conditions to false if the phase is terminal.
	// This is done to ensure kubelet does not report a status update with terminal pod phase and ready=true.
	// See https://issues.k8s.io/108594 for more details.
	if podutil.IsPodPhaseTerminal(newPodStatus.Phase) {
		if podutil.IsPodReadyConditionTrue(newPodStatus) || podutil.IsContainersReadyConditionTrue(newPodStatus) {
			oldStatusCopy := oldPodStatus
			newStatusCopy := newPodStatus
			containersReadyCondition := generateContainersReadyConditionForTerminalPhase(pod, &oldStatusCopy, newStatusCopy.Phase)
			podutil.UpdatePodCondition(&newStatusCopy, &containersReadyCondition)

			podReadyCondition := generatePodReadyConditionForTerminalPhase(pod, &oldStatusCopy, newStatusCopy.Phase)
			podutil.UpdatePodCondition(&newStatusCopy, &podReadyCondition)
			newPodStatus = newStatusCopy
		}
	}

	return newPodStatus
}

// NeedToReconcilePodReadiness returns if the pod "Ready" condition need to be reconcile
func NeedToReconcilePodReadiness(pod *v1.Pod) bool {
	if len(pod.Spec.ReadinessGates) == 0 {
		return false
	}
	podReadyCondition := GeneratePodReadyCondition(pod, &pod.Status, pod.Status.Conditions, pod.Status.ContainerStatuses, pod.Status.Phase)
	i, curCondition := podutil.GetPodConditionFromList(pod.Status.Conditions, v1.PodReady)
	// Only reconcile if "Ready" condition is present and Status or Message is not expected
	if i >= 0 && (curCondition.Status != podReadyCondition.Status || curCondition.Message != podReadyCondition.Message) {
		return true
	}
	return false
}

func updatedPodResizeCondition(conditionType v1.PodConditionType, oldCondition *v1.PodCondition, reason, message string, observedGeneration int64) *v1.PodCondition {
	now := metav1.NewTime(time.Now())
	var lastTransitionTime metav1.Time
	if oldCondition == nil || oldCondition.Reason != reason {
		lastTransitionTime = now
	} else {
		lastTransitionTime = oldCondition.LastTransitionTime
	}

	return &v1.PodCondition{
		Type:               conditionType,
		Status:             v1.ConditionTrue,
		LastProbeTime:      now,
		LastTransitionTime: lastTransitionTime,
		ObservedGeneration: observedGeneration,
		Reason:             reason,
		Message:            message,
	}
}

// observePendingResizeCount sets the pending resize metric.
func (m *manager) observePendingResizeCount() {
	type pendingKey struct {
		reason         string
		priorityBucket metrics.PriorityBucket
	}
	pendingResizeCount := make(map[pendingKey]int)
	for uid, conditions := range m.podResizeConditions {
		if conditions.PodResizePending != nil {
			pod, ok := m.podManager.GetPodByUID(uid)
			if !ok {
				continue
			}
			reason := strings.ToLower(conditions.PodResizePending.Reason)
			priorityBucket := metrics.GetPriorityBucketLabel(pod)
			pendingResizeCount[pendingKey{reason: reason, priorityBucket: priorityBucket}]++
		}
	}

	metrics.PodPendingResizes.Reset()
	for k, count := range pendingResizeCount {
		metrics.PodPendingResizes.WithLabelValues(k.reason, string(k.priorityBucket)).Set(float64(count))
	}
}

// recordInProgressResize sets the in-progress resize metric.
func (m *manager) recordInProgressResizeCount() {
	inProgressResizeCount := 0
	for _, conditions := range m.podResizeConditions {
		if conditions.PodResizeInProgress != nil {
			inProgressResizeCount++
		}
	}
	metrics.PodInProgressResizes.Set(float64(inProgressResizeCount))
}
