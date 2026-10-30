/*
Copyright 2019 The Kubernetes Authors.

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
	"fmt"
	"slices"
	"sync"
	"unicode/utf8"

	"github.com/blang/semver/v4"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"k8s.io/klog/v2"
)

const maxDescLabelCacheEntries = 4096

type descLabelCache struct {
	initErr   error
	zeroPairs []*dto.LabelPair
	mu        sync.RWMutex
	cache1    map[string][]*dto.LabelPair
	cache2    map[[2]string][]*dto.LabelPair
	cache3    map[[3]string][]*dto.LabelPair
	cache4    map[[4]string][]*dto.LabelPair
}

// Desc is a prometheus.Desc extension.
//
// Use NewDesc to create new Desc instances.
type Desc struct {
	// fqName has been built from Namespace, Subsystem, and Name.
	fqName string
	// help provides some helpful information about this metric.
	help string
	// constLabels is the label names. Their label values are variable.
	constLabels Labels
	// variableLabels contains names of labels for which the metric
	// maintains variable values.
	variableLabels []string

	// promDesc is the descriptor used by every Prometheus Metric.
	promDesc      *prometheus.Desc
	annotatedHelp string
	labelCache    *descLabelCache

	// stabilityLevel represents the API guarantees for a given defined metric.
	stabilityLevel StabilityLevel
	// deprecatedVersion represents in which version this metric be deprecated.
	deprecatedVersion string

	isDeprecated        bool
	isHidden            bool
	isCreated           bool
	createLock          sync.RWMutex
	markDeprecationOnce sync.Once
	createOnce          sync.Once
	deprecateOnce       sync.Once
	hideOnce            sync.Once
	annotateOnce        sync.Once
}

// NewDesc extends prometheus.NewDesc with stability support.
//
// The stabilityLevel should be valid stability label, such as "metrics.ALPHA"
// and "metrics.STABLE"(Maybe "metrics.BETA" in future). Default value "metrics.ALPHA"
// will be used in case of empty or invalid stability label.
//
// The deprecatedVersion represents in which version this Metric be deprecated.
// The deprecation policy outlined by the control plane metrics stability KEP.
func NewDesc(fqName string, help string, variableLabels []string, constLabels Labels,
	stabilityLevel StabilityLevel, deprecatedVersion string) *Desc {
	d := &Desc{
		fqName:            fqName,
		help:              help,
		annotatedHelp:     help,
		variableLabels:    variableLabels,
		constLabels:       constLabels,
		stabilityLevel:    stabilityLevel,
		deprecatedVersion: deprecatedVersion,
	}
	d.stabilityLevel.setDefaults()

	return d
}

// String formats the Desc as a string.
// The stability metadata maybe annotated in 'HELP' section if called after registry,
// otherwise not.
// e.g. "Desc{fqName: "normal_stable_descriptor", help: "[STABLE] this is a stable descriptor", constLabels: {}, variableLabels: []}"
func (d *Desc) String() string {
	if d.isCreated {
		return d.promDesc.String()
	}

	return prometheus.NewDesc(d.fqName, d.help, d.variableLabels, prometheus.Labels(d.constLabels)).String()
}

// toPrometheusDesc transform self to prometheus.Desc
func (d *Desc) toPrometheusDesc() *prometheus.Desc {
	return d.promDesc
}

// DeprecatedVersion returns a pointer to the Version or nil
func (d *Desc) DeprecatedVersion() *semver.Version {
	return parseSemver(d.deprecatedVersion)

}

func (d *Desc) determineDeprecationStatus(currentVersion semver.Version) {
	deprecatedVersion := d.DeprecatedVersion()
	if deprecatedVersion == nil {
		return
	}
	d.markDeprecationOnce.Do(func() {
		d.isDeprecated = isDeprecated(currentVersion, *deprecatedVersion)
		if shouldHide(d.stabilityLevel, &currentVersion, deprecatedVersion) {
			if ShouldShowHidden() {
				klog.Warningf("Hidden metrics(%s) have been manually overridden, showing this very deprecated metric.", d.fqName)
				return
			}
			// TODO(RainbowMango): Remove this log temporarily. https://github.com/kubernetes/kubernetes/issues/85369
			// klog.Warningf("This metric(%s) has been deprecated for more than one release, hiding.", d.fqName)
			d.isHidden = true
		}
	})
}

// IsHidden returns if metric will be hidden
func (d *Desc) IsHidden() bool {
	return d.isHidden
}

// IsDeprecated returns if metric has been deprecated
func (d *Desc) IsDeprecated() bool {
	return d.isDeprecated
}

// IsCreated returns if metric has been created.
func (d *Desc) IsCreated() bool {
	d.createLock.RLock()
	defer d.createLock.RUnlock()

	return d.isCreated
}

// create forces the initialization of Desc which has been deferred until
// the point at which this method is invoked. This method will determine whether
// the Desc is deprecated or hidden, no-opting if the Desc should be considered
// hidden. Furthermore, this function no-opts and returns true if Desc is already
// created.
func (d *Desc) create(version *semver.Version) bool {
	if version != nil {
		d.determineDeprecationStatus(*version)
	}

	// let's not create if this metric is slated to be hidden
	if d.IsHidden() {
		return false
	}
	d.createOnce.Do(func() {
		d.createLock.Lock()
		defer d.createLock.Unlock()

		d.isCreated = true
		if d.IsDeprecated() {
			d.initializeDeprecatedDesc()
		} else {
			d.initialize()
		}
	})
	return d.IsCreated()
}

// ClearState will clear all the states marked by Create.
// It intends to be used for re-register a hidden metric.
func (d *Desc) ClearState() {
	d.isDeprecated = false
	d.isHidden = false
	d.isCreated = false

	d.markDeprecationOnce = *new(sync.Once)
	d.createOnce = *new(sync.Once)
	d.deprecateOnce = *new(sync.Once)
	d.hideOnce = *new(sync.Once)
	d.annotateOnce = *new(sync.Once)

	d.annotatedHelp = d.help
	d.promDesc = nil
	d.labelCache = nil
}

func (d *Desc) markDeprecated() {
	d.deprecateOnce.Do(func() {
		d.annotatedHelp = fmt.Sprintf("(Deprecated since %s) %s", d.deprecatedVersion, d.annotatedHelp)
	})
}

func (d *Desc) annotateStabilityLevel() {
	d.annotateOnce.Do(func() {
		d.annotatedHelp = fmt.Sprintf("[%v] %v", d.stabilityLevel, d.annotatedHelp)
	})
}

func (d *Desc) initialize() {
	d.annotateStabilityLevel()

	// this actually creates the underlying prometheus desc.
	d.promDesc = prometheus.NewDesc(d.fqName, d.annotatedHelp, d.variableLabels, prometheus.Labels(d.constLabels))
	d.initLabelCache()
}

func (d *Desc) initLabelCache() {
	_, err := prometheus.NewConstMetric(d.promDesc, prometheus.GaugeValue, 0, make([]string, len(d.variableLabels))...)
	if err != nil {
		d.labelCache = &descLabelCache{initErr: err}
		return
	}
	lc := &descLabelCache{}
	if len(d.variableLabels) == 0 {
		lc.zeroPairs = prometheus.MakeLabelPairs(d.promDesc, nil)
	}
	d.labelCache = lc
}

func (d *Desc) getOrBuildLabelPairs(labelValues []string) ([]*dto.LabelPair, bool) {
	lc := d.labelCache
	if lc == nil || lc.initErr != nil || len(labelValues) != len(d.variableLabels) {
		return nil, false
	}
	switch len(d.variableLabels) {
	case 0:
		return lc.zeroPairs, true
	case 1:
		k := labelValues[0]
		lc.mu.RLock()
		pairs, ok := lc.cache1[k]
		lc.mu.RUnlock()
		if ok {
			return pairs, true
		}
		if !utf8.ValidString(k) {
			return nil, false
		}
		pairs = prometheus.MakeLabelPairs(d.promDesc, []string{k})
		lc.mu.Lock()
		if lc.cache1 == nil {
			lc.cache1 = make(map[string][]*dto.LabelPair, 16)
		} else if len(lc.cache1) >= maxDescLabelCacheEntries {
			clear(lc.cache1)
		}
		lc.cache1[k] = pairs
		lc.mu.Unlock()
		return pairs, true
	case 2:
		k := [2]string{labelValues[0], labelValues[1]}
		lc.mu.RLock()
		pairs, ok := lc.cache2[k]
		lc.mu.RUnlock()
		if ok {
			return pairs, true
		}
		if !utf8.ValidString(k[0]) || !utf8.ValidString(k[1]) {
			return nil, false
		}
		pairs = prometheus.MakeLabelPairs(d.promDesc, []string{k[0], k[1]})
		lc.mu.Lock()
		if lc.cache2 == nil {
			lc.cache2 = make(map[[2]string][]*dto.LabelPair, 64)
		} else if len(lc.cache2) >= maxDescLabelCacheEntries {
			clear(lc.cache2)
		}
		lc.cache2[k] = pairs
		lc.mu.Unlock()
		return pairs, true
	case 3:
		k := [3]string{labelValues[0], labelValues[1], labelValues[2]}
		lc.mu.RLock()
		pairs, ok := lc.cache3[k]
		lc.mu.RUnlock()
		if ok {
			return pairs, true
		}
		if !utf8.ValidString(k[0]) || !utf8.ValidString(k[1]) || !utf8.ValidString(k[2]) {
			return nil, false
		}
		pairs = prometheus.MakeLabelPairs(d.promDesc, []string{k[0], k[1], k[2]})
		lc.mu.Lock()
		if lc.cache3 == nil {
			lc.cache3 = make(map[[3]string][]*dto.LabelPair, 64)
		} else if len(lc.cache3) >= maxDescLabelCacheEntries {
			clear(lc.cache3)
		}
		lc.cache3[k] = pairs
		lc.mu.Unlock()
		return pairs, true
	case 4:
		k := [4]string{labelValues[0], labelValues[1], labelValues[2], labelValues[3]}
		lc.mu.RLock()
		pairs, ok := lc.cache4[k]
		lc.mu.RUnlock()
		if ok {
			return pairs, true
		}
		if !utf8.ValidString(k[0]) || !utf8.ValidString(k[1]) || !utf8.ValidString(k[2]) || !utf8.ValidString(k[3]) {
			return nil, false
		}
		pairs = prometheus.MakeLabelPairs(d.promDesc, []string{k[0], k[1], k[2], k[3]})
		lc.mu.Lock()
		if lc.cache4 == nil {
			lc.cache4 = make(map[[4]string][]*dto.LabelPair, 64)
		} else if len(lc.cache4) >= maxDescLabelCacheEntries {
			clear(lc.cache4)
		}
		lc.cache4[k] = pairs
		lc.mu.Unlock()
		return pairs, true
	default:
		for _, lv := range labelValues {
			if !utf8.ValidString(lv) {
				return nil, false
			}
		}
		return prometheus.MakeLabelPairs(d.promDesc, slices.Clone(labelValues)), true
	}
}

func (d *Desc) initializeDeprecatedDesc() {
	d.markDeprecated()
	d.initialize()
}

// GetRawDesc will returns a new *Desc with original parameters provided to NewDesc().
//
// It will be useful in testing scenario that the same Desc be registered to different registry.
//  1. Desc `D` is registered to registry 'A' in TestA (Note: `D` maybe created)
//  2. Desc `D` is registered to registry 'B' in TestB (Note: since 'D' has been created once, thus will be ignored by registry 'B')
func (d *Desc) GetRawDesc() *Desc {
	return NewDesc(d.fqName, d.help, d.variableLabels, d.constLabels, d.stabilityLevel, d.deprecatedVersion)
}
