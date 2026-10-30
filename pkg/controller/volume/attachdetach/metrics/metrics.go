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

package metrics

import (
	"errors"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/controller/volume/attachdetach/cache"
	"k8s.io/kubernetes/pkg/controller/volume/attachdetach/util"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/csimigration"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
)

const pluginNameNotAvailable = "N/A"

const (
	// Force detach reason is timeout
	ForceDetachReasonTimeout = "timeout"
	// Force detach reason is the node has an out-of-service taint
	ForceDetachReasonOutOfService = "out-of-service"
	attachDetachController        = "attach_detach_controller"
)

var (
	inUseVolumeMetricDesc = metrics.NewDesc(
		metrics.BuildFQName("", "storage_count", "attachable_volumes_in_use"),
		"Measure number of volumes in use",
		[]string{"node", "volume_plugin"}, nil,
		metrics.ALPHA, "")

	totalVolumesMetricDesc = metrics.NewDesc(
		metrics.BuildFQName("", "attachdetach_controller", "total_volumes"),
		"Number of volumes in A/D Controller",
		[]string{"plugin_name", "state"}, nil,
		metrics.ALPHA, "")

	ForceDetachMetricCounter = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      attachDetachController,
			Name:           "attachdetach_controller_forced_detaches",
			Help:           "Number of times the A/D Controller performed a forced detach",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"reason"},
	)
)
var registerMetrics sync.Once

// Register registers metrics in A/D Controller.
func Register(pvcLister corelisters.PersistentVolumeClaimLister,
	pvLister corelisters.PersistentVolumeLister,
	podLister corelisters.PodLister,
	asw cache.ActualStateOfWorld,
	dsw cache.DesiredStateOfWorld,
	pluginMgr *volume.VolumePluginMgr,
	csiMigratedPluginManager csimigration.PluginManager,
	intreeToCSITranslator csimigration.InTreeToCSITranslator) {
	registerMetrics.Do(func() {
		legacyregistry.CustomMustRegister(newAttachDetachStateCollector(pvcLister,
			podLister,
			pvLister,
			asw,
			dsw,
			pluginMgr,
			csiMigratedPluginManager,
			intreeToCSITranslator))
		legacyregistry.MustRegister(ForceDetachMetricCounter)
	})
}

type attachDetachStateCollector struct {
	metrics.BaseStableCollector

	pvcLister                corelisters.PersistentVolumeClaimLister
	podLister                corelisters.PodLister
	pvLister                 corelisters.PersistentVolumeLister
	asw                      cache.ActualStateOfWorld
	dsw                      cache.DesiredStateOfWorld
	volumePluginMgr          *volume.VolumePluginMgr
	csiMigratedPluginManager csimigration.PluginManager
	intreeToCSITranslator    csimigration.InTreeToCSITranslator
}

// volumeCount is a map of maps used as a counter, e.g.:
//
//	node 172.168.1.100.ec2.internal has 10 EBS and 3 glusterfs PVC in use:
//	{"172.168.1.100.ec2.internal": {"aws-ebs": 10, "glusterfs": 3}}
//	state actual_state_of_world contains a total of 10 EBS volumes:
//	{"actual_state_of_world": {"aws-ebs": 10}}
type volumeCount map[string]map[string]int64

func (v volumeCount) add(typeKey, counterKey string) {
	count, ok := v[typeKey]
	if !ok {
		count = make(map[string]int64, 4)
		v[typeKey] = count
	}
	count[counterKey]++
}

var adSpecPool = sync.Pool{
	New: func() any {
		return &volume.Spec{}
	},
}

func newAttachDetachStateCollector(
	pvcLister corelisters.PersistentVolumeClaimLister,
	podLister corelisters.PodLister,
	pvLister corelisters.PersistentVolumeLister,
	asw cache.ActualStateOfWorld,
	dsw cache.DesiredStateOfWorld,
	pluginMgr *volume.VolumePluginMgr,
	csiMigratedPluginManager csimigration.PluginManager,
	intreeToCSITranslator csimigration.InTreeToCSITranslator) *attachDetachStateCollector {
	return &attachDetachStateCollector{pvcLister: pvcLister, podLister: podLister, pvLister: pvLister, asw: asw, dsw: dsw, volumePluginMgr: pluginMgr, csiMigratedPluginManager: csiMigratedPluginManager, intreeToCSITranslator: intreeToCSITranslator}
}

// Check if our collector implements necessary collector interface
var _ metrics.StableCollector = &attachDetachStateCollector{}

func (collector *attachDetachStateCollector) DescribeWithStability(ch chan<- *metrics.Desc) {
	ch <- inUseVolumeMetricDesc
	ch <- totalVolumesMetricDesc
}

func (collector *attachDetachStateCollector) CollectWithStability(ch chan<- metrics.Metric) {
	labelBuf := make([]string, 2)
	nodeVolumeMap := collector.getVolumeInUseCount(klog.TODO())
	for nodeName, pluginCount := range nodeVolumeMap {
		labelBuf[0] = string(nodeName)
		for pluginName, count := range pluginCount {
			labelBuf[1] = pluginName
			ch <- metrics.NewLazyConstMetric(inUseVolumeMetricDesc,
				metrics.GaugeValue,
				float64(count),
				labelBuf...)
		}
	}

	stateVolumeMap := collector.getTotalVolumesCount()
	for stateName, pluginCount := range stateVolumeMap {
		labelBuf[1] = string(stateName)
		for pluginName, count := range pluginCount {
			labelBuf[0] = pluginName
			ch <- metrics.NewLazyConstMetric(totalVolumesMetricDesc,
				metrics.GaugeValue,
				float64(count),
				labelBuf...)
		}
	}
}

func (collector *attachDetachStateCollector) resolveFallbackPluginName(logger klog.Logger, podVolume *v1.Volume, pod *v1.Pod) (string, bool) {
	volumeSpec, err := util.CreateVolumeSpecWithNodeMigration(logger, *podVolume, pod, types.NodeName(pod.Spec.NodeName), collector.volumePluginMgr, collector.pvcLister, collector.pvLister, collector.csiMigratedPluginManager, collector.intreeToCSITranslator)
	if err != nil {
		return "", false
	}
	volumePlugin, err := collector.volumePluginMgr.FindPluginBySpec(volumeSpec)
	if err != nil || volumePlugin == nil {
		return "", false
	}
	return volumeutil.GetFullQualifiedPluginNameForVolume(volumePlugin.GetPluginName(), volumeSpec), true
}

func (collector *attachDetachStateCollector) resolveSpecPluginName(logger klog.Logger, spec *volume.Spec, podVolume *v1.Volume, pod *v1.Pod) (string, bool) {
	if collector.csiMigratedPluginManager.PluginNameMapper != nil {
		migratable, err := collector.csiMigratedPluginManager.IsMigratable(spec)
		if err != nil {
			return "", false
		}
		if migratable {
			return collector.resolveFallbackPluginName(logger, podVolume, pod)
		}
	}
	volumePlugin, err := collector.volumePluginMgr.FindPluginBySpec(spec)
	if err != nil || volumePlugin == nil {
		return "", false
	}
	return volumeutil.GetFullQualifiedPluginNameForVolume(volumePlugin.GetPluginName(), spec), true
}

func (collector *attachDetachStateCollector) resolvePodVolumePluginName(logger klog.Logger, podVolume *v1.Volume, pod *v1.Pod) (string, bool) {
	if collector.volumePluginMgr == nil {
		return "", false
	}
	if podVolume.Ephemeral != nil {
		return collector.resolveFallbackPluginName(logger, podVolume, pod)
	}

	if pvcSource := podVolume.PersistentVolumeClaim; pvcSource != nil && pvcSource.ClaimName != "" {
		if collector.pvcLister == nil || collector.pvLister == nil {
			return "", false
		}
		claim, err := collector.pvcLister.PersistentVolumeClaims(pod.Namespace).Get(pvcSource.ClaimName)
		if err != nil || claim == nil || claim.Status.Phase != v1.ClaimBound || claim.Spec.VolumeName == "" {
			return "", false
		}
		pv, err := collector.pvLister.Get(claim.Spec.VolumeName)
		if err != nil || pv == nil || pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != claim.UID {
			return "", false
		}

		spec := adSpecPool.Get().(*volume.Spec)
		*spec = volume.Spec{PersistentVolume: pv, ReadOnly: pvcSource.ReadOnly}
		pluginName, ok := collector.resolveSpecPluginName(logger, spec, podVolume, pod)
		*spec = volume.Spec{}
		adSpecPool.Put(spec)
		return pluginName, ok
	}

	spec := adSpecPool.Get().(*volume.Spec)
	*spec = volume.Spec{Volume: podVolume}
	pluginName, ok := collector.resolveSpecPluginName(logger, spec, podVolume, pod)
	*spec = volume.Spec{}
	adSpecPool.Put(spec)
	return pluginName, ok
}

func (collector *attachDetachStateCollector) getVolumeInUseCount(logger klog.Logger) volumeCount {
	pods, err := collector.podLister.List(labels.Everything())
	if err != nil {
		logger.Error(errors.New("Error getting pod list"), "Get pod list failed")
		return nil
	}

	nodeVolumeMap := make(volumeCount, 64)
	for _, pod := range pods {
		if len(pod.Spec.Volumes) <= 0 {
			continue
		}

		if pod.Spec.NodeName == "" {
			continue
		}
		for i := range pod.Spec.Volumes {
			pluginName, ok := collector.resolvePodVolumePluginName(logger, &pod.Spec.Volumes[i], pod)
			if !ok {
				continue
			}
			nodeVolumeMap.add(pod.Spec.NodeName, pluginName)
		}
	}
	return nodeVolumeMap
}

func (collector *attachDetachStateCollector) getTotalVolumesCount() volumeCount {
	stateVolumeMap := make(volumeCount, 2)
	for _, v := range collector.dsw.GetVolumesToAttach() {
		if plugin, err := collector.volumePluginMgr.FindPluginBySpec(v.VolumeSpec); err == nil {
			pluginName := pluginNameNotAvailable
			if plugin != nil {
				pluginName = volumeutil.GetFullQualifiedPluginNameForVolume(plugin.GetPluginName(), v.VolumeSpec)
			}
			stateVolumeMap.add("desired_state_of_world", pluginName)
		}
	}
	for _, v := range collector.asw.GetAttachedVolumes() {
		if plugin, err := collector.volumePluginMgr.FindPluginBySpec(v.VolumeSpec); err == nil {
			pluginName := pluginNameNotAvailable
			if plugin != nil {
				pluginName = volumeutil.GetFullQualifiedPluginNameForVolume(plugin.GetPluginName(), v.VolumeSpec)
			}
			stateVolumeMap.add("actual_state_of_world", pluginName)
		}
	}
	return stateVolumeMap
}

// RecordForcedDetachMetric register a forced detach metric.
func RecordForcedDetachMetric(forceDetachReason string) {
	ForceDetachMetricCounter.WithLabelValues(forceDetachReason).Inc()
}
