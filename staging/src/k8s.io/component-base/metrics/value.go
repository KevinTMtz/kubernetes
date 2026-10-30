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
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// ValueType is an enumeration of metric types that represent a simple value.
type ValueType int

// Possible values for the ValueType enum.
const (
	_ ValueType = iota
	CounterValue
	GaugeValue
	UntypedValue
)

func (vt *ValueType) toPromValueType() prometheus.ValueType {
	return prometheus.ValueType(*vt)
}

type gaugeConstMetric struct {
	desc       *prometheus.Desc
	labelPairs []*dto.LabelPair
	val        float64
	tsMillis   int64
	hasTs      bool
	gauge      dto.Gauge
}

func (m *gaugeConstMetric) Desc() *prometheus.Desc {
	return m.desc
}

func (m *gaugeConstMetric) Write(out *dto.Metric) error {
	out.Label = m.labelPairs
	out.Gauge = &m.gauge
	if m.hasTs {
		out.TimestampMs = &m.tsMillis
	}
	return nil
}

type counterConstMetric struct {
	desc       *prometheus.Desc
	labelPairs []*dto.LabelPair
	val        float64
	tsMillis   int64
	hasTs      bool
	counter    dto.Counter
}

func (m *counterConstMetric) Desc() *prometheus.Desc {
	return m.desc
}

func (m *counterConstMetric) Write(out *dto.Metric) error {
	out.Label = m.labelPairs
	out.Counter = &m.counter
	if m.hasTs {
		out.TimestampMs = &m.tsMillis
	}
	return nil
}

type untypedConstMetric struct {
	desc       *prometheus.Desc
	labelPairs []*dto.LabelPair
	val        float64
	tsMillis   int64
	hasTs      bool
	untyped    dto.Untyped
}

func (m *untypedConstMetric) Desc() *prometheus.Desc {
	return m.desc
}

func (m *untypedConstMetric) Write(out *dto.Metric) error {
	out.Label = m.labelPairs
	out.Untyped = &m.untyped
	if m.hasTs {
		out.TimestampMs = &m.tsMillis
	}
	return nil
}

func newFastConstMetric(desc *prometheus.Desc, pairs []*dto.LabelPair, valueType ValueType, value float64) (Metric, bool) {
	switch valueType {
	case GaugeValue:
		m := &gaugeConstMetric{
			desc:       desc,
			labelPairs: pairs,
			val:        value,
		}
		m.gauge.Value = &m.val
		return m, true
	case CounterValue:
		m := &counterConstMetric{
			desc:       desc,
			labelPairs: pairs,
			val:        value,
		}
		m.counter.Value = &m.val
		return m, true
	case UntypedValue:
		m := &untypedConstMetric{
			desc:       desc,
			labelPairs: pairs,
			val:        value,
		}
		m.untyped.Value = &m.val
		return m, true
	default:
		return nil, false
	}
}

// NewLazyConstMetric is a helper of MustNewConstMetric.
//
// Note: If the metrics described by the desc is hidden, the metrics will not be created.
func NewLazyConstMetric(desc *Desc, valueType ValueType, value float64, labelValues ...string) Metric {
	if desc.IsHidden() {
		return nil
	}
	if pairs, ok := desc.getOrBuildLabelPairs(labelValues); ok {
		if m, validType := newFastConstMetric(desc.promDesc, pairs, valueType, value); validType {
			return m
		}
	}
	return prometheus.MustNewConstMetric(desc.toPrometheusDesc(), valueType.toPromValueType(), value, slices.Clone(labelValues)...)
}

// NewConstMetric is a helper of NewConstMetric.
//
// Note: If the metrics described by the desc is hidden, the metrics will not be created.
func NewConstMetric(desc *Desc, valueType ValueType, value float64, labelValues ...string) (Metric, error) {
	if desc.IsHidden() {
		return nil, nil
	}
	if pairs, ok := desc.getOrBuildLabelPairs(labelValues); ok {
		if m, validType := newFastConstMetric(desc.promDesc, pairs, valueType, value); validType {
			return m, nil
		}
	}
	return prometheus.NewConstMetric(desc.toPrometheusDesc(), valueType.toPromValueType(), value, slices.Clone(labelValues)...)
}

// NewLazyMetricWithTimestamp is a helper of NewMetricWithTimestamp.
//
// Warning: the Metric 'm' must be the one created by NewLazyConstMetric(),
// otherwise, no stability guarantees would be offered.
func NewLazyMetricWithTimestamp(t time.Time, m Metric) Metric {
	if m == nil {
		return nil
	}
	switch cm := m.(type) {
	case *gaugeConstMetric:
		if !cm.hasTs {
			cm.tsMillis = t.UnixNano() / int64(time.Millisecond)
			cm.hasTs = true
			return cm
		}
	case *counterConstMetric:
		if !cm.hasTs {
			cm.tsMillis = t.UnixNano() / int64(time.Millisecond)
			cm.hasTs = true
			return cm
		}
	case *untypedConstMetric:
		if !cm.hasTs {
			cm.tsMillis = t.UnixNano() / int64(time.Millisecond)
			cm.hasTs = true
			return cm
		}
	}

	return prometheus.NewMetricWithTimestamp(t, m)
}
