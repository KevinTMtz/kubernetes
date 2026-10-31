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

package cm

import (
	v1 "k8s.io/api/core/v1"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/memorymanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
)

// DefaultNodeAllocatableCgroupNameForTest exposes defaultNodeAllocatableCgroupName to cm_test benchmarks.
const DefaultNodeAllocatableCgroupNameForTest = defaultNodeAllocatableCgroupName

// NewPodContainerManagerForTest constructs a real podContainerManagerImpl backed by a containerManagerImpl
// wired to cpuManager (so PodHasExclusiveCPUs and ContainerHasExclusiveCPUs run real production code)
// while delegating cgroup writes to the provided CgroupManager.
func NewPodContainerManagerForTest(
	qosContainersInfo QOSContainersInfo,
	cgroupManager CgroupManager,
	cpuManager cpumanager.Manager,
	enforceCPULimits bool,
	memoryReservationPolicy kubeletconfig.MemoryReservationPolicy,
	throttlingFactor *float64,
) PodContainerManager {
	cmImpl := &containerManagerImpl{
		cpuManager: cpuManager,
	}
	return &podContainerManagerImpl{
		qosContainersInfo:       qosContainersInfo,
		cgroupManager:           cgroupManager,
		podPidsLimit:            4096,
		enforceCPULimits:        enforceCPULimits,
		cpuCFSQuotaPeriod:       QuotaPeriod,
		podContainerManager:     cmImpl,
		memoryReservationPolicy: memoryReservationPolicy,
		memoryThrottlingFactor:  throttlingFactor,
	}
}

// NewQOSContainerManagerForTest constructs a real qosContainerManagerImpl for deterministic UpdateCgroups calls.
func NewQOSContainerManagerForTest(
	qosContainersInfo QOSContainersInfo,
	cgroupRoot CgroupName,
	cgroupManager CgroupManager,
	activePods ActivePodsFunc,
	getNodeAllocatable func() v1.ResourceList,
	qosReserved map[v1.ResourceName]int64,
	memoryReservationPolicy kubeletconfig.MemoryReservationPolicy,
) QOSContainerManager {
	return &qosContainerManagerImpl{
		qosContainersInfo:       qosContainersInfo,
		cgroupManager:           cgroupManager,
		activePods:              activePods,
		getNodeAllocatable:      getNodeAllocatable,
		cgroupRoot:              cgroupRoot,
		qosReserved:             qosReserved,
		memoryReservationPolicy: memoryReservationPolicy,
	}
}

// NewInternalContainerLifecycleForTest constructs a real internalContainerLifecycleImpl.
func NewInternalContainerLifecycleForTest(
	cpuManager cpumanager.Manager,
	memoryManager memorymanager.Manager,
	topologyManager topologymanager.Manager,
) InternalContainerLifecycle {
	return &internalContainerLifecycleImpl{
		cpuManager:      cpuManager,
		memoryManager:   memoryManager,
		topologyManager: topologyManager,
	}
}
