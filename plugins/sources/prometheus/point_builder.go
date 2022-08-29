// Copyright 2021 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package prometheus

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/wavefronthq/wavefront-collector-for-kubernetes/internal/experimental"
	"github.com/wavefronthq/wavefront-collector-for-kubernetes/internal/filter"

	"github.com/wavefronthq/wavefront-collector-for-kubernetes/internal/wf"

	prom "github.com/prometheus/client_model/go"

	gometrics "github.com/rcrowley/go-metrics"
	"github.com/wavefronthq/wavefront-collector-for-kubernetes/internal/util"
)

type pointBuilder struct {
	filters          filter.Filter
	filtered         gometrics.Counter
	source           string
	prefix           string
	omitBucketSuffix bool
	tags             map[string]string
	interner         util.StringInterner
}

func NewPointBuilder(src *prometheusMetricsSource, filtered gometrics.Counter) *pointBuilder {
	return &pointBuilder{
		source:           src.source,
		prefix:           src.prefix,
		omitBucketSuffix: src.omitBucketSuffix,
		tags:             src.tags,
		filters:          src.filters,
		filtered:         filtered,
		interner:         util.NewStringInterner(),
	}

}

// build converts a map of prometheus metric families by metric name to a collection of wavefront points
// build actually never returns an error
func (builder *pointBuilder) build(metricFamilies map[string]*prom.MetricFamily) ([]wf.Metric, error) {
	now := time.Now().Unix()
	var result []wf.Metric
	for metricName, mf := range metricFamilies {
		for _, m := range mf.Metric {
			// Prometheus metric family -> wavefront metric points
			if mf.GetType() == prom.MetricType_SUMMARY {
				result = append(result, builder.buildSummaryPoints(metricName, m, now, builder.buildTags(m))...)
			} else if mf.GetType() == prom.MetricType_HISTOGRAM {
				if experimental.IsEnabled(experimental.HistogramConversion) {
					point := builder.buildWFHistogram(metricName, m, now, builder.buildTags(m))
					result = wf.FilterAppend(builder.filters, builder.filtered, result, point)
				}
				result = append(result, builder.buildHistogramPoints(metricName, m, now, builder.buildTags(m))...)
			} else {
				result = append(result, builder.buildPoints(metricName, m, now)...)
			}
		}
	}
	return result, nil
}

func (builder *pointBuilder) point(name string, value float64, ts int64, source string, tags map[string]string) wf.Metric {
	point := wf.NewPoint(
		builder.name(name),
		value,
		ts,
		source,
		nil,
	)

	point.SetLabelPairs(builder.deduplicate(tags)) //store tags as LabelPairs for memory optimization
	return point
}

func (builder *pointBuilder) name(name string) string {
	return builder.prefix + strings.Replace(name, "_", ".", -1)
}

// Get name and value from metric
func (builder *pointBuilder) buildPoints(name string, m *prom.Metric, now int64) []wf.Metric {
	var result []wf.Metric
	if m.Gauge != nil {
		if !math.IsNaN(m.GetGauge().GetValue()) {
			point := builder.point(name+".gauge", m.GetGauge().GetValue(), now, builder.source, builder.buildTags(m))
			result = wf.FilterAppend(builder.filters, builder.filtered, result, point)
		}
	} else if m.Counter != nil {
		if !math.IsNaN(m.GetCounter().GetValue()) {
			point := builder.point(name+".counter", m.GetCounter().GetValue(), now, builder.source, builder.buildTags(m))
			result = wf.FilterAppend(builder.filters, builder.filtered, result, point)
		}
	} else if m.Untyped != nil {
		if !math.IsNaN(m.GetUntyped().GetValue()) {
			point := builder.point(name+".value", m.GetUntyped().GetValue(), now, builder.source, builder.buildTags(m))
			result = wf.FilterAppend(builder.filters, builder.filtered, result, point)
		}
	}
	return result
}

// Get Quantiles from summary metric
func (builder *pointBuilder) buildSummaryPoints(name string, m *prom.Metric, now int64, tags map[string]string) []wf.Metric {
	var result []wf.Metric
	for _, q := range m.GetSummary().Quantile {
		if !math.IsNaN(q.GetValue()) {
			newTags := copyOf(tags)
			newTags["quantile"] = fmt.Sprintf("%v", q.GetQuantile())
			point := builder.point(name, q.GetValue(), now, builder.source, newTags)
			result = wf.FilterAppend(builder.filters, builder.filtered, result, point)
		}
	}
	point := builder.point(name+".count", float64(m.GetSummary().GetSampleCount()), now, builder.source, tags)
	result = wf.FilterAppend(builder.filters, builder.filtered, result, point)
	point = builder.point(name+".sum", m.GetSummary().GetSampleSum(), now, builder.source, tags)
	result = wf.FilterAppend(builder.filters, builder.filtered, result, point)

	return result
}

func (builder *pointBuilder) buildWFHistogram(name string, m *prom.Metric, now int64, tags map[string]string) wf.Metric {
	buckets := m.GetHistogram().Bucket
	centroids := make([]wf.Centroid, 0, len(buckets))
	for i, bucket := range buckets {
		centroids = append(centroids, wf.Centroid{
			Value: bucket.GetUpperBound(),
			Count: float64(buckets[i].GetCumulativeCount()),
		})
	}
	return wf.NewCumulativeDistribution(builder.name(name), builder.source, tags, centroids, time.Unix(now, 0))
}

// Get Buckets from histogram metric
func (builder *pointBuilder) buildHistogramPoints(name string, m *prom.Metric, now int64, tags map[string]string) []wf.Metric {
	var result []wf.Metric
	histName := builder.histogramName(name)
	for _, b := range m.GetHistogram().Bucket {
		newTags := copyOf(tags)
		newTags["le"] = fmt.Sprintf("%v", b.GetUpperBound())
		point := builder.point(histName, float64(b.GetCumulativeCount()), now, builder.source, newTags)
		result = wf.FilterAppend(builder.filters, builder.filtered, result, point)
	}
	point := builder.point(name+".count", float64(m.GetHistogram().GetSampleCount()), now, builder.source, tags)
	result = wf.FilterAppend(builder.filters, builder.filtered, result, point)
	point = builder.point(name+".sum", m.GetHistogram().GetSampleSum(), now, builder.source, tags)
	result = wf.FilterAppend(builder.filters, builder.filtered, result, point)
	return result
}

// Get labels from metric
func (builder *pointBuilder) buildTags(m *prom.Metric) map[string]string {
	tags := make(map[string]string, len(builder.tags)+len(m.Label))
	for k, v := range builder.tags {
		if len(v) > 0 {
			tags[k] = v
		}
	}
	if len(m.Label) >= 0 {
		for _, label := range m.Label {
			if len(label.GetName()) > 0 && len(label.GetValue()) > 0 {
				tags[label.GetName()] = label.GetValue()
			}
		}
	}
	return tags
}

func (builder *pointBuilder) histogramName(name string) string {
	if builder.omitBucketSuffix {
		return name
	}
	return name + ".bucket"
}

func (builder *pointBuilder) deduplicate(tags map[string]string) []wf.LabelPair {
	result := make([]wf.LabelPair, 0)
	for k, v := range tags {
		result = append(result, wf.LabelPair{
			Name:  builder.interner.Intern(k),
			Value: builder.interner.Intern(v),
		})
	}
	return result
}

func copyOf(tags map[string]string) map[string]string {
	newTags := make(map[string]string, len(tags)+1)
	for k, v := range tags {
		newTags[k] = v
	}
	return newTags
}
