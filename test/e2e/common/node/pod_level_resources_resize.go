/*
Copyright 2024 The Kubernetes Authors.

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

package node

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/test/e2e/common/node/framework/cgroups"
	"k8s.io/kubernetes/test/e2e/common/node/framework/podresize"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"

	"github.com/onsi/ginkgo/v2"
)

func doGuaranteedPodLevelResizeTests(f *framework.Framework) {
	ginkgo.DescribeTableSubtree("guaranteed qos - 1 container with resize policy", func(cpuPolicy, memPolicy v1.ResourceResizeRestartPolicy, resizeInitCtrs bool) {
		ginkgo.DescribeTable("resizing", func(ctx context.Context, desiredCtrCPU, desiredCtrMem, desiredPodCPU, desiredPodMem string) {

			// The tests for guaranteed pods include extended resources.
			nodes, err := e2enode.GetReadySchedulableNodes(context.Background(), f.ClientSet)
			framework.ExpectNoError(err)
			for _, node := range nodes.Items {
				e2enode.AddExtendedResource(ctx, f.ClientSet, node.Name, fakeExtendedResource, resource.MustParse("123"))
			}
			defer func() {
				for _, node := range nodes.Items {
					e2enode.RemoveExtendedResource(ctx, f.ClientSet, node.Name, fakeExtendedResource)
				}
			}()

			originalContainers := makeGuaranteedContainers(1, cpuPolicy, memPolicy, true, true, originalCPU, originalMem)
			expectedContainers := makeGuaranteedContainers(1, cpuPolicy, memPolicy, true, true, desiredCtrCPU, desiredCtrMem)
			for i, c := range expectedContainers {
				// If the pod has init containers, but we are not resizing them, keep the original resources.
				if c.InitCtr && !resizeInitCtrs {
					c.Resources = originalContainers[i].Resources
					expectedContainers[i] = c
					continue
				}
				// For containers where the resize policy is "restart", we expect a restart.
				expectRestart := int32(0)
				if cpuPolicy == v1.RestartContainer && desiredCtrCPU != originalCPU {
					expectRestart = 1
				}
				if memPolicy == v1.RestartContainer && desiredCtrMem != originalMem {
					expectRestart = 1
				}
				c.RestartCount = expectRestart
				expectedContainers[i] = c
			}

			var originalPodResources, desiredPodResources *v1.ResourceRequirements
			if desiredPodCPU != "" || desiredPodMem != "" {
				originalPodResources = makePodResources(offsetCPU(15, originalCPU), offsetCPU(15, originalCPU), offsetMemory(15, originalMem), offsetMemory(15, originalMem))
				desiredPodResources = makePodResources(offsetCPU(15, desiredPodCPU), offsetCPU(15, desiredPodCPU), offsetMemory(15, desiredPodMem), offsetMemory(15, desiredPodMem))
			}

			doPatchAndRollback(ctx, f, originalContainers, expectedContainers, originalPodResources, desiredPodResources, true)
		},
			// All tests will perform the requested resize, and once completed, will roll back the change.
			// This results in the coverage of both increase and decrease of resources.
			ginkgo.Entry("cpu", increasedCPU, originalMem, "", ""),
			ginkgo.Entry("mem", originalCPU, increasedMem, "", ""),
			ginkgo.Entry("cpu & mem in the same direction", increasedCPU, increasedMem, "", ""),
			ginkgo.Entry("cpu & mem in opposite directions", increasedCPU, reducedMem, "", ""),
			ginkgo.Entry("pod-level cpu", originalCPU, originalMem, increasedCPU, originalMem),
			ginkgo.Entry("pod-level mem", originalCPU, originalMem, originalCPU, offsetMemory(10, increasedMem)),
			ginkgo.Entry("pod-level cpu & mem in the same direction", originalCPU, originalMem, increasedCPU, increasedMem),
			ginkgo.Entry("pod-level cpu & mem in opposite directions", originalCPU, originalMem, increasedCPU, reducedMem),
		)
	},
		ginkgo.Entry("no restart", v1.NotRequired, v1.NotRequired, false),
		ginkgo.Entry("no restart + resize initContainers", v1.NotRequired, v1.NotRequired, true),
		ginkgo.Entry("mem restart", v1.NotRequired, v1.RestartContainer, false),
		ginkgo.Entry("cpu restart", v1.RestartContainer, v1.NotRequired, false),
		ginkgo.Entry("cpu & mem restart", v1.RestartContainer, v1.RestartContainer, false),
		ginkgo.Entry("cpu & mem restart + resize initContainers", v1.RestartContainer, v1.RestartContainer, true),
	)

	// All tests will perform the requested resize, and once completed, will roll back the change.
	// This results in coverage of both the operation as described, and its reverse.
	ginkgo.Describe("pod-level guaranteed pods with multiple containers", func() {
		/*
			Release: v1.35
			Testname: In-place Pod Resize, guaranteed pods with multiple containers, net increase
			Description: Issuing an in-place Pod Resize request via the Pod Resize subresource patch endpoint to modify CPU and memory requests and limits for a guaranteed pod with 3 containers with a net increase MUST result in the Pod resources being updated as expected.
		*/
		framework.It("3 containers - increase cpu & mem on c1, c2, decrease cpu & mem on c3 - net increase [MinimumKubeletVersion:1.34]", func(ctx context.Context) {
			originalContainers := makeGuaranteedContainers(3, v1.NotRequired, v1.NotRequired, false, false, originalCPU, originalMem)
			for i := range originalContainers {
				originalContainers[i].CPUPolicy = nil
				originalContainers[i].MemPolicy = nil
			}

			expectedContainers := []podresize.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &cgroups.ContainerResources{CPUReq: offsetCPU(0, increasedCPU), CPULim: offsetCPU(0, increasedCPU), MemReq: offsetMemory(0, increasedMem), MemLim: offsetMemory(0, increasedMem)},
				},
				{
					Name:      "c2",
					Resources: &cgroups.ContainerResources{CPUReq: offsetCPU(1, increasedCPU), CPULim: offsetCPU(1, increasedCPU), MemReq: offsetMemory(1, increasedMem), MemLim: offsetMemory(1, increasedMem)},
				},
				{
					Name:      "c3",
					Resources: &cgroups.ContainerResources{CPUReq: offsetCPU(2, reducedCPU), CPULim: offsetCPU(2, reducedCPU), MemReq: offsetMemory(2, reducedMem), MemLim: offsetMemory(2, reducedMem)},
				},
			}

			doPatchAndRollback(ctx, f, originalContainers, expectedContainers, nil, nil, true)
		})

		/*
			Release: v1.35
			Testname: In-place Pod Resize, guaranteed pods with multiple containers, net decrease
			Description: Issuing an in-place Pod Resize request via the Pod Resize subresource patch endpoint to modify CPU and memory requests and limits for a pod with 3 containers with a net decrease MUST result in the Pod resources being updated as expected.
		*/
		framework.It("3 containers - increase cpu & mem on c1, decrease cpu & mem on c2, c3 - net decrease [MinimumKubeletVersion:1.34]", func(ctx context.Context) {
			originalContainers := makeGuaranteedContainers(3, v1.NotRequired, v1.NotRequired, false, false, originalCPU, originalMem)
			for i := range originalContainers {
				originalContainers[i].CPUPolicy = nil
				originalContainers[i].MemPolicy = nil
			}

			expectedContainers := []podresize.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &cgroups.ContainerResources{CPUReq: offsetCPU(0, increasedCPU), CPULim: offsetCPU(0, increasedCPU), MemReq: offsetMemory(0, increasedMem), MemLim: offsetMemory(0, increasedMem)},
				},
				{
					Name:      "c2",
					Resources: &cgroups.ContainerResources{CPUReq: offsetCPU(1, reducedCPU), CPULim: offsetCPU(1, reducedCPU), MemReq: offsetMemory(1, reducedMem), MemLim: offsetMemory(1, reducedMem)},
				},
				{
					Name:      "c3",
					Resources: &cgroups.ContainerResources{CPUReq: offsetCPU(2, reducedCPU), CPULim: offsetCPU(2, reducedCPU), MemReq: offsetMemory(2, reducedMem), MemLim: offsetMemory(2, reducedMem)},
				},
			}

			doPatchAndRollback(ctx, f, originalContainers, expectedContainers, nil, nil, true)
		})

		/*
			Release: v1.35
			Testname: In-place Pod Resize, guaranteed pods with multiple containers, various operations
			Description: Issuing an in-place Pod Resize request via the Pod Resize subresource patch endpoint to modify CPU and memory requests and limits for a pod with 3 containers with various operations MUST result in the Pod resources being updated as expected.
		*/
		framework.It("3 containers - increase: CPU (c1,c3), memory (c2, c3) ; decrease: CPU (c2) [MinimumKubeletVersion:1.34]", func(ctx context.Context) {
			originalContainers := makeGuaranteedContainers(3, v1.NotRequired, v1.NotRequired, false, false, originalCPU, originalMem)
			for i := range originalContainers {
				originalContainers[i].CPUPolicy = nil
				originalContainers[i].MemPolicy = nil
			}

			expectedContainers := []podresize.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &cgroups.ContainerResources{CPUReq: offsetCPU(0, increasedCPU), CPULim: offsetCPU(0, increasedCPU), MemReq: offsetMemory(0, originalMem), MemLim: offsetMemory(0, originalMem)},
				},
				{
					Name:      "c2",
					Resources: &cgroups.ContainerResources{CPUReq: offsetCPU(1, reducedCPU), CPULim: offsetCPU(1, reducedCPU), MemReq: offsetMemory(1, increasedMem), MemLim: offsetMemory(1, increasedMem)},
				},
				{
					Name:      "c3",
					Resources: &cgroups.ContainerResources{CPUReq: offsetCPU(2, increasedCPU), CPULim: offsetCPU(2, increasedCPU), MemReq: offsetMemory(2, increasedMem), MemLim: offsetMemory(2, increasedMem)},
				},
			}

			doPatchAndRollback(ctx, f, originalContainers, expectedContainers, nil, nil, true)
		})
	})
}

func doBurstablePodLevelResizeTests(f *framework.Framework) {
	ginkgo.DescribeTableSubtree("pod-level burstable pods - 1 container with all requests & limits set and resize policy", func(cpuPolicy, memPolicy v1.ResourceResizeRestartPolicy) {
		ginkgo.DescribeTable("resizing", func(ctx context.Context, desiredContainerResources, desiredPodLevelResources resourceRequestsLimits) {
			originalContainers := makeBurstableContainers(1, cpuPolicy, memPolicy, originalCPU, originalCPULimit, originalMem, originalMemLimit)
			expectedContainers := makeBurstableContainers(1, cpuPolicy, memPolicy, desiredContainerResources.cpuReq, desiredContainerResources.cpuLim, desiredContainerResources.memReq, desiredContainerResources.memLim)
			for i, c := range expectedContainers {
				// For containers where the resize policy is "restart", we expect a restart.
				expectRestart := int32(0)
				if cpuPolicy == v1.RestartContainer && (desiredContainerResources.cpuReq != originalCPU || desiredContainerResources.cpuLim != originalCPULimit) {
					expectRestart = 1
				}
				if memPolicy == v1.RestartContainer && (desiredContainerResources.memReq != originalMem || desiredContainerResources.memLim != originalMemLimit) {
					expectRestart = 1
				}
				c.RestartCount = expectRestart
				expectedContainers[i] = c
			}

			var originalPodResources, desiredPodResources *v1.ResourceRequirements
			if desiredPodLevelResources != (resourceRequestsLimits{}) {
				originalPodResources = makePodResources(offsetCPU(30, originalCPU), offsetCPU(30, originalCPULimit), offsetMemory(30, originalMem), offsetMemory(30, originalMemLimit))
				desiredPodResources = makePodResources(offsetCPU(30, desiredPodLevelResources.cpuReq), offsetCPU(30, desiredPodLevelResources.cpuLim), offsetMemory(30, desiredPodLevelResources.memReq), offsetMemory(30, desiredPodLevelResources.memLim))
			}
			doPatchAndRollback(ctx, f, originalContainers, expectedContainers, originalPodResources, desiredPodResources, true)
		},
			// All tests will perform the requested resize, and once completed, will roll back the change.
			// This results in the coverage of both increase and decrease of resources.
			ginkgo.Entry("cpu requests", resourceRequestsLimits{increasedCPU, originalCPULimit, originalMem, originalMemLimit}, resourceRequestsLimits{}),
			ginkgo.Entry("cpu limits", resourceRequestsLimits{originalCPU, increasedCPULimit, originalMem, originalMemLimit}, resourceRequestsLimits{}),
			ginkgo.Entry("mem requests", resourceRequestsLimits{originalCPU, originalCPULimit, increasedMem, originalMemLimit}, resourceRequestsLimits{}),
			ginkgo.Entry("mem limits", resourceRequestsLimits{originalCPU, originalCPULimit, originalMem, increasedMemLimit}, resourceRequestsLimits{}),
			ginkgo.Entry("all resources in the same direction", resourceRequestsLimits{increasedCPU, increasedCPULimit, increasedMem, increasedMemLimit}, resourceRequestsLimits{}),
			ginkgo.Entry("cpu & mem in opposite directions", resourceRequestsLimits{increasedCPU, increasedCPULimit, reducedMem, reducedMemLimit}, resourceRequestsLimits{}),
			ginkgo.Entry("requests & limits in opposite directions", resourceRequestsLimits{reducedCPU, increasedCPULimit, increasedMem, reducedMemLimit}, resourceRequestsLimits{}),
			ginkgo.Entry("pod-level cpu requests", resourceRequestsLimits{originalCPU, originalCPULimit, originalMem, originalMemLimit}, resourceRequestsLimits{increasedCPU, originalCPULimit, originalMem, originalMemLimit}),
			ginkgo.Entry("pod-level cpu limits", resourceRequestsLimits{originalCPU, originalCPULimit, originalMem, originalMemLimit}, resourceRequestsLimits{originalCPU, increasedCPULimit, originalMem, originalMemLimit}),
			ginkgo.Entry("pod-level mem requests", resourceRequestsLimits{originalCPU, originalCPULimit, originalMem, originalMemLimit}, resourceRequestsLimits{originalCPU, originalCPULimit, increasedMem, originalMemLimit}),
			ginkgo.Entry("pod-level mem limits", resourceRequestsLimits{originalCPU, originalCPULimit, originalMem, originalMemLimit}, resourceRequestsLimits{originalCPU, originalCPULimit, originalMem, increasedMemLimit}),
			ginkgo.Entry("pod-level all resources in the same direction", resourceRequestsLimits{originalCPU, originalCPULimit, originalMem, originalMemLimit}, resourceRequestsLimits{increasedCPU, increasedCPULimit, increasedMem, increasedMemLimit}),
			ginkgo.Entry("pod-level cpu & mem in opposite directions", resourceRequestsLimits{originalCPU, originalCPULimit, originalMem, originalMemLimit}, resourceRequestsLimits{increasedCPU, increasedCPULimit, reducedMem, reducedMemLimit}),
			ginkgo.Entry("pod-level requests & limits in opposite directions", resourceRequestsLimits{originalCPU, originalCPULimit, originalMem, originalMemLimit}, resourceRequestsLimits{reducedCPU, increasedCPULimit, increasedMem, reducedMemLimit}),
		)
	},
		ginkgo.Entry("no restart", v1.NotRequired, v1.NotRequired),
		ginkgo.Entry("cpu restart", v1.RestartContainer, v1.NotRequired),
		ginkgo.Entry("mem restart", v1.NotRequired, v1.RestartContainer),
		ginkgo.Entry("cpu & mem restart", v1.RestartContainer, v1.RestartContainer),
	)

	// The following tests cover the remaining burstable pod scenarios:
	// - multiple containers
	// - adding limits where only requests were previously set
	// - adding requests where none were previously set
	// - resizing with equivalents (e.g. 2m -> 1m)
	ginkgo.Describe("burstable pods - extended", func() {
		/*
			Release: v1.35
			Testname: In-place Pod Resize, burstable pod with multiple containers and various operations
			Description: Issuing a Pod Resize request via the Pod Resize subresource patch endpoint to modify CPU and memory requests and limits on a 6-container pod with various operations MUST result in the Pod resources being updated as expected.
		*/
		framework.It("6 containers - various operations performed (including adding limits and requests) [MinimumKubeletVersion:1.34]", func(ctx context.Context) {
			originalContainers := []podresize.ResizableContainerInfo{
				{
					// c1 starts with CPU requests only; increase CPU requests + add CPU limits
					Name:      "c1",
					Resources: &cgroups.ContainerResources{CPUReq: originalCPU},
				},
				{
					// c2 starts with memory requests only; increase memory requests + add memory limits
					Name:      "c2",
					Resources: &cgroups.ContainerResources{MemReq: originalMem},
				},
				{
					// c3 starts with CPU and memory requests; decrease memory requests only
					Name:      "c3",
					Resources: &cgroups.ContainerResources{CPUReq: originalCPU, MemReq: originalMem},
				},
				{
					// c4 starts with CPU requests only; decrease CPU requests + add memory requests
					Name:      "c4",
					Resources: &cgroups.ContainerResources{CPUReq: originalCPU},
				},
				{
					// c5 starts with memory requests only; increase memory requests + add CPU requests
					Name:      "c5",
					Resources: &cgroups.ContainerResources{MemReq: originalMem},
				},
			}
			expectedContainers := []podresize.ResizableContainerInfo{
				{
					// c1 starts with CPU requests only; increase CPU requests + add CPU limits
					Name:      "c1",
					Resources: &cgroups.ContainerResources{CPUReq: increasedCPU, CPULim: increasedCPULimit},
				},
				{
					// c2 starts with memory requests only; decrease memory requests + add memory limits
					Name:      "c2",
					Resources: &cgroups.ContainerResources{MemReq: reducedMem, MemLim: increasedMemLimit},
				},
				{
					// c3 starts with CPU and memory requests; decrease memory requests onloy
					Name:      "c3",
					Resources: &cgroups.ContainerResources{CPUReq: originalCPU, MemReq: reducedMem},
				},
				{
					// c4 starts with CPU requests only; decrease CPU requests + add memory requests
					Name:      "c4",
					Resources: &cgroups.ContainerResources{CPUReq: reducedCPU, MemReq: originalMem},
				},
				{
					// c5 starts with memory requests only; increase memory requests + add CPU requests
					Name:      "c5",
					Resources: &cgroups.ContainerResources{CPUReq: originalCPU, MemReq: increasedMem},
				},
			}
			doPatchAndRollback(ctx, f, originalContainers, expectedContainers, nil, nil, false)
		})

		/*
			Release: v1.35
			Testname: In-place Pod Resize, burstable pod resized with equivalents
			Description: Issuing an in-place Pod Resize request via the Pod Resize subresource patch endpoint to modify CPU requests and limits using equivalent values (e.g. 2m -> 1m) MUST result in the updated Pod resources displayed correctly in the status.
		*/
		framework.It("resize with equivalents [MinimumKubeletVersion:1.34]", func(ctx context.Context) {
			originalContainers := []podresize.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &cgroups.ContainerResources{CPUReq: "2m", CPULim: "10m"},
				},
			}
			expectedContainers := []podresize.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &cgroups.ContainerResources{CPUReq: "1m", CPULim: "5m"},
				},
			}
			doPatchAndRollback(ctx, f, originalContainers, expectedContainers, nil, nil, true)
		})
	})

	ginkgo.DescribeTable("burstable pods - pod-level resources", func(ctx context.Context, originalContainers, expectedContainers []podresize.ResizableContainerInfo, originalPodLevelResources, expectedPodLevelResources resourceRequestsLimits, doRollback bool) {

		originalPodResources := makePodResources(originalPodLevelResources.cpuReq, originalPodLevelResources.cpuLim, originalPodLevelResources.memReq, originalPodLevelResources.memLim)
		desiredPodResources := makePodResources(expectedPodLevelResources.cpuReq, expectedPodLevelResources.cpuLim, expectedPodLevelResources.memReq, expectedPodLevelResources.memLim)

		doPatchAndRollback(ctx, f, originalContainers, expectedContainers, originalPodResources, desiredPodResources, doRollback)
	},
		ginkgo.Entry("pod-level resize with equivalents",
			[]podresize.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &cgroups.ContainerResources{CPUReq: "2m", CPULim: "10m"},
				},
			},
			[]podresize.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &cgroups.ContainerResources{CPUReq: "2m", CPULim: "10m"},
				},
			},
			resourceRequestsLimits{cpuReq: "4m", cpuLim: "20m"},
			resourceRequestsLimits{cpuReq: "5m", cpuLim: "25m"},
			true,
		),
		ginkgo.Entry("pod-level resize with limits add",
			[]podresize.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &cgroups.ContainerResources{CPUReq: "2m", CPULim: "10m"},
				},
			},
			[]podresize.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &cgroups.ContainerResources{CPUReq: "2m", CPULim: "10m"},
				},
			},
			resourceRequestsLimits{cpuReq: "4m"},
			resourceRequestsLimits{cpuReq: "5m", cpuLim: "25m"},
			false,
		),
		ginkgo.Entry("pod-level resize with requests and limits add",
			[]podresize.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &cgroups.ContainerResources{CPUReq: "2m", CPULim: "10m"},
				},
			},
			[]podresize.ResizableContainerInfo{
				{
					Name:      "c1",
					Resources: &cgroups.ContainerResources{CPUReq: "2m", CPULim: "10m"},
				},
			},
			resourceRequestsLimits{},
			resourceRequestsLimits{cpuReq: "5m", cpuLim: "25m"},
			false,
		),
		ginkgo.Entry("pod-level resize with no container requests and limits",
			[]podresize.ResizableContainerInfo{
				{
					Name: "c1",
				},
			},
			[]podresize.ResizableContainerInfo{
				{
					Name: "c1",
				},
			},
			resourceRequestsLimits{cpuReq: "4m", memReq: "10m"},
			resourceRequestsLimits{cpuReq: "5m", memReq: "25m"},
			false,
		),
	)
}

var _ = SIGDescribe("Pod InPlace Resize", feature.InPlacePodLevelResourcesVerticalScaling, framework.WithFeatureGate(features.InPlacePodLevelResourcesVerticalScaling), func() {
	f := framework.NewDefaultFramework("pod-level-resources-resize-tests")

	ginkgo.BeforeEach(func(ctx context.Context) {
		_, err := e2enode.GetRandomReadySchedulableNode(ctx, f.ClientSet)
		framework.ExpectNoError(err)
		if framework.NodeOSDistroIs("windows") {
			e2eskipper.Skipf("runtime does not support InPlacePodVerticalScaling -- skipping")
		}
	})

	doGuaranteedPodLevelResizeTests(f)
	doBurstablePodLevelResizeTests(f)
})

func makePodResources(cpuReq, cpuLim, memReq, memLim string) *v1.ResourceRequirements {
	if cpuReq == "" && memReq == "" && cpuLim == "" && memLim == "" {
		return nil
	}

	resources := &v1.ResourceRequirements{}

	if cpuLim != "" || memLim != "" {
		resources.Limits = v1.ResourceList{}
	}

	if cpuLim != "" {
		resources.Limits[v1.ResourceCPU] = resource.MustParse(cpuLim)
	}

	if memLim != "" {
		resources.Limits[v1.ResourceMemory] = resource.MustParse(memLim)

	}

	if cpuReq != "" || memReq != "" {
		resources.Requests = v1.ResourceList{}
	}

	if cpuReq != "" {
		resources.Requests[v1.ResourceCPU] = resource.MustParse(cpuReq)
	}

	if memReq != "" {
		resources.Requests[v1.ResourceMemory] = resource.MustParse(memReq)

	}
	return resources
}
