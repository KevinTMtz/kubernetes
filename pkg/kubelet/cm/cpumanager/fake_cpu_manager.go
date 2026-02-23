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

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/cm/containermap"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	cmqos "k8s.io/kubernetes/pkg/kubelet/cm/qos"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/utils/cpuset"
)

type fakeManager struct {
	logger logr.Logger
	state  state.State
}

func (m *fakeManager) Start(ctx context.Context, activePods ActivePodsFunc, sourcesReady config.SourcesReady, podStatusProvider status.PodStatusProvider, containerRuntime runtimeService, initialContainers containermap.ContainerMap) error {
	logger := klog.FromContext(ctx)
	logger.Info("Start()")
	return nil
}

func (m *fakeManager) Policy() Policy {
	m.logger.Info("Policy()")
	pol, _ := NewNonePolicy(nil)
	return pol
}

func (m *fakeManager) Allocate(pod *v1.Pod, container *v1.Container) error {
	logger := klog.TODO()
	logger.Info("Allocate", "pod", klog.KObj(pod), "containerName", container.Name)
	return nil
}

func (m *fakeManager) AddContainer(logger logr.Logger, pod *v1.Pod, container *v1.Container, containerID string) {
	logger.Info("AddContainer", "pod", klog.KObj(pod), "containerName", container.Name, "containerID", containerID)
}

func (m *fakeManager) RemoveContainer(logger logr.Logger, containerID string) error {
	logger.Info("RemoveContainer", "containerID", containerID)
	return nil
}

func (m *fakeManager) GetTopologyHints(pod *v1.Pod, container *v1.Container) map[string][]topologymanager.TopologyHint {
	logger := klog.TODO()
	logger.Info("Get container topology hints")
	return map[string][]topologymanager.TopologyHint{}
}

func (m *fakeManager) GetPodTopologyHints(pod *v1.Pod) map[string][]topologymanager.TopologyHint {
	logger := klog.TODO()
	logger.Info("Get pod topology hints")
	return map[string][]topologymanager.TopologyHint{}
}

func (m *fakeManager) AllocatePod(pod *v1.Pod) error {
	logger := klog.TODO()
	logger.Info("AllocatePod", "pod", klog.KObj(pod))
	return nil
}

func (m *fakeManager) State() state.Reader {
	return m.state
}

func (m *fakeManager) GetExclusiveCPUs(podUID, containerName string) cpuset.CPUSet {
	m.logger.Info("GetExclusiveCPUs", "podUID", podUID, "containerName", containerName)
	return cpuset.CPUSet{}
}

func (m *fakeManager) GetAllocatableCPUs() cpuset.CPUSet {
	m.logger.Info("Get Allocatable CPUs")
	return cpuset.CPUSet{}
}

func (m *fakeManager) GetCPUAffinity(podUID, containerName string) cpuset.CPUSet {
	m.logger.Info("GetCPUAffinity", "podUID", podUID, "containerName", containerName)
	// Try to get specific container assignment first.
	// If none exists, fall back to the pod-level pool (pod assignment).
	// This fallback is critical for the pod-level isolation model, ensuring unassigned
	// containers (like ephemeral containers) stay within the pod's hardware boundary.
	if cpuSet, ok := m.state.GetPodCPUSet(podUID); ok {
		if cset, ok := m.state.GetCPUAssignments()[podUID][containerName]; ok {
			return cset
		}
		return cpuSet
	}
	return m.state.GetCPUSetOrDefault(podUID, containerName)
}

func (m *fakeManager) GetAllCPUs() cpuset.CPUSet {
	m.logger.Info("GetAllCPUs")
	return m.state.GetDefaultCPUSet()
}

func (m *fakeManager) GetResourceIsolationLevel(pod *v1.Pod, container *v1.Container) cmqos.ResourceIsolationLevel {
	// If the container has no specific affinity assigned, it defaults to the pod-level isolation.
	if _, ok := m.state.GetCPUSet(string(pod.UID), container.Name); !ok {
		return cmqos.ResourceIsolationHost
	}
	return cmqos.ResourceIsolationContainer
}

// GetPodCPUSet returns the CPUSet allocated to the pod as a whole (the pod-level pool).
func (m *fakeManager) GetPodCPUSet(podUID string) cpuset.CPUSet {
	return cpuset.CPUSet{}
}

// NewFakeManager creates empty/fake cpu manager
func NewFakeManager(logger logr.Logger) Manager {
	logger = klog.LoggerWithName(logger, "cpu.fake")
	return &fakeManager{
		logger: logger,
		state:  state.NewMemoryState(logger),
	}
}
