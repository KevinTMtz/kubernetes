/*
Copyright 2018 The Kubernetes Authors.

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

package collectors

import (
	"context"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/component-base/metrics"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	kubeletmetrics "k8s.io/kubernetes/pkg/kubelet/metrics"
	serverstats "k8s.io/kubernetes/pkg/kubelet/server/stats"
)

var (
	volumeStatsCapacityBytesDesc = metrics.NewDesc(
		metrics.BuildFQName("", kubeletmetrics.KubeletSubsystem, kubeletmetrics.VolumeStatsCapacityBytesKey),
		"Capacity in bytes of the volume",
		[]string{"namespace", "persistentvolumeclaim"}, nil,
		metrics.ALPHA, "",
	)
	volumeStatsAvailableBytesDesc = metrics.NewDesc(
		metrics.BuildFQName("", kubeletmetrics.KubeletSubsystem, kubeletmetrics.VolumeStatsAvailableBytesKey),
		"Number of available bytes in the volume",
		[]string{"namespace", "persistentvolumeclaim"}, nil,
		metrics.ALPHA, "",
	)
	volumeStatsUsedBytesDesc = metrics.NewDesc(
		metrics.BuildFQName("", kubeletmetrics.KubeletSubsystem, kubeletmetrics.VolumeStatsUsedBytesKey),
		"Number of used bytes in the volume",
		[]string{"namespace", "persistentvolumeclaim"}, nil,
		metrics.ALPHA, "",
	)
	volumeStatsInodesDesc = metrics.NewDesc(
		metrics.BuildFQName("", kubeletmetrics.KubeletSubsystem, kubeletmetrics.VolumeStatsInodesKey),
		"Maximum number of inodes in the volume",
		[]string{"namespace", "persistentvolumeclaim"}, nil,
		metrics.ALPHA, "",
	)
	volumeStatsInodesFreeDesc = metrics.NewDesc(
		metrics.BuildFQName("", kubeletmetrics.KubeletSubsystem, kubeletmetrics.VolumeStatsInodesFreeKey),
		"Number of free inodes in the volume",
		[]string{"namespace", "persistentvolumeclaim"}, nil,
		metrics.ALPHA, "",
	)
	volumeStatsInodesUsedDesc = metrics.NewDesc(
		metrics.BuildFQName("", kubeletmetrics.KubeletSubsystem, kubeletmetrics.VolumeStatsInodesUsedKey),
		"Number of used inodes in the volume",
		[]string{"namespace", "persistentvolumeclaim"}, nil,
		metrics.ALPHA, "",
	)
)

type volumeStatsCollector struct {
	metrics.BaseStableCollector

	statsProvider serverstats.Provider
}

// Check if volumeStatsCollector implements necessary interface
var _ metrics.StableCollector = &volumeStatsCollector{}

// NewVolumeStatsCollector creates a volume stats metrics.StableCollector.
func NewVolumeStatsCollector(statsProvider serverstats.Provider) metrics.StableCollector {
	return &volumeStatsCollector{statsProvider: statsProvider}
}

// DescribeWithStability implements the metrics.StableCollector interface.
func (collector *volumeStatsCollector) DescribeWithStability(ch chan<- *metrics.Desc) {
	ch <- volumeStatsCapacityBytesDesc
	ch <- volumeStatsAvailableBytesDesc
	ch <- volumeStatsUsedBytesDesc
	ch <- volumeStatsInodesDesc
	ch <- volumeStatsInodesFreeDesc
	ch <- volumeStatsInodesUsedDesc
}

// CollectWithStability implements the metrics.StableCollector interface.
func (collector *volumeStatsCollector) CollectWithStability(ch chan<- metrics.Metric) {
	// Use context.TODO() because we currently do not have a proper context to pass in.
	// Replace this with an appropriate context when refactoring this function to accept a context parameter.
	ctx := context.TODO()
	podStats, err := collector.statsProvider.ListPodStats(ctx)
	if err != nil {
		return
	}
	pvcLabels := make([]string, 2)
	addGauge := func(desc *metrics.Desc, v float64) {
		ch <- metrics.NewLazyConstMetric(desc, metrics.GaugeValue, v, pvcLabels...)
	}
	allPVCs := make(sets.Set[stats.PVCReference], len(podStats)*2)
	for i := range podStats {
		podStat := &podStats[i]
		if podStat.VolumeStats == nil {
			continue
		}
		for j := range podStat.VolumeStats {
			volumeStat := &podStat.VolumeStats[j]
			pvcRef := volumeStat.PVCRef
			if pvcRef == nil {
				// ignore if no PVC reference
				continue
			}
			if allPVCs.Has(*pvcRef) {
				// ignore if already collected
				continue
			}
			pvcLabels[0] = pvcRef.Namespace
			pvcLabels[1] = pvcRef.Name
			addGauge(volumeStatsCapacityBytesDesc, float64(*volumeStat.CapacityBytes))
			addGauge(volumeStatsAvailableBytesDesc, float64(*volumeStat.AvailableBytes))
			addGauge(volumeStatsUsedBytesDesc, float64(*volumeStat.UsedBytes))
			addGauge(volumeStatsInodesDesc, float64(*volumeStat.Inodes))
			addGauge(volumeStatsInodesFreeDesc, float64(*volumeStat.InodesFree))
			addGauge(volumeStatsInodesUsedDesc, float64(*volumeStat.InodesUsed))
			allPVCs.Insert(*pvcRef)
		}
	}
}
