// Package metrics provides Prometheus-style metrics for the Kubernetes
// client.
//
// There are two steps to set up exporting:
//  1. Register metrics with a storage via RegisterKubernetesClientMetrics.
//  2. Register the produced backends with client-go via the
//     k8s.io/client-go/tools/metrics package.
//
// Backends implement the interfaces from
// https://github.com/kubernetes/client-go/blob/master/tools/metrics/metrics.go.
//
// The naming pattern is modelled after
// https://github.com/flant/shell-operator/blob/main/pkg/metrics/metrics.go:
// metric name constants contain a {PREFIX} placeholder. Call ReplacePrefix to
// obtain the concrete name for a given prefix. Prefix resolution is
// per-instance, but client-go accepts its metric backends only once per
// process (k8s.io/client-go/tools/metrics.Register), so only the first
// initialized Client's prefix, labels and storage receive samples.
package metrics

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	clientgometrics "k8s.io/client-go/tools/metrics"
)

// Metric name templates. The {PREFIX} placeholder is replaced at construction
// time by passing a prefix to the constructors or to ReplacePrefix.
// These constants are intentionally read-only; use ReplacePrefix to build the
// concrete name for a specific client instance.
const (
	// ============================================================================
	// Kubernetes Client Metrics
	// ============================================================================
	// KubernetesClientRequestLatencySeconds measures Kubernetes API request
	// latency per verb and URL.
	KubernetesClientRequestLatencySeconds = "{PREFIX}kubernetes_client_request_latency_seconds"
	// KubernetesClientRequestResultTotal counts Kubernetes API request results
	// per code, method and host.
	KubernetesClientRequestResultTotal = "{PREFIX}kubernetes_client_request_result_total"
	// KubernetesClientRateLimiterLatencySeconds measures client-go rate
	// limiter latency per verb and URL.
	KubernetesClientRateLimiterLatencySeconds = "{PREFIX}kubernetes_client_rate_limiter_latency_seconds"
)

// ReplacePrefix replaces the {PREFIX} placeholder in a metric name template
// with the provided prefix. Useful in tests or when constructing metric names
// for external use.
func ReplacePrefix(metricName, prefix string) string {
	return strings.ReplaceAll(metricName, "{PREFIX}", prefix)
}

// Storage models the subset of flant/shell-operator's metric storage that
// this package depends on.
type Storage interface {
	RegisterCounter(metric string, labels map[string]string) *prometheus.CounterVec
	CounterAdd(metric string, value float64, labels map[string]string)
	RegisterHistogram(metric string, labels map[string]string, buckets []float64) *prometheus.HistogramVec
	HistogramObserve(metric string, value float64, labels map[string]string, buckets []float64)
}

// kubernetesClientRequestBuckets is the histogram bucket set used for
// Kubernetes client request timing (milliseconds to tens of seconds).
var kubernetesClientRequestBuckets = []float64{
	0.0,
	0.001, 0.002, 0.005, // 1,2,5 milliseconds
	0.01, 0.02, 0.05, // 10,20,50 milliseconds
	0.1, 0.2, 0.5, // 100,200,500 milliseconds
	1, 2, 5, // 1,2,5 seconds
	10, 20, 50, // 10,20,50 seconds
}

// RegisterKubernetesClientMetrics registers all Kubernetes client metrics with
// the provided storage. prefix is substituted for {PREFIX} in the metric name
// templates. extraLabels are merged into the label set of each metric and let
// callers attach custom dimensions (for example "operator").
func RegisterKubernetesClientMetrics(storage Storage, extraLabels map[string]string, prefix string) {
	for _, name := range []string{KubernetesClientRequestLatencySeconds, KubernetesClientRateLimiterLatencySeconds} {
		storage.RegisterHistogram(
			ReplacePrefix(name, prefix),
			mergeLabels(extraLabels, LabelVerb, LabelURL),
			kubernetesClientRequestBuckets,
		)
	}

	storage.RegisterCounter(
		ReplacePrefix(KubernetesClientRequestResultTotal, prefix),
		mergeLabels(extraLabels, LabelCode, LabelMethod, LabelHost),
	)
}

// NewRequestLatency returns a client-go metrics.LatencyMetric that observes
// the duration of Kubernetes API requests.
func NewRequestLatency(storage Storage, extraLabels map[string]string, prefix string) clientgometrics.LatencyMetric {
	return Latency{
		storage:     storage,
		extraLabels: extraLabels,
		metricName:  ReplacePrefix(KubernetesClientRequestLatencySeconds, prefix),
	}
}

// NewRateLimiterLatency returns a client-go metrics.LatencyMetric that
// observes how long requests wait in the client-go rate limiter.
func NewRateLimiterLatency(storage Storage, extraLabels map[string]string, prefix string) clientgometrics.LatencyMetric {
	return Latency{
		storage:     storage,
		extraLabels: extraLabels,
		metricName:  ReplacePrefix(KubernetesClientRateLimiterLatencySeconds, prefix),
	}
}

// Latency observes a client-go latency into one histogram. The label set
// matches the one registered by RegisterKubernetesClientMetrics.
type Latency struct {
	storage     Storage
	extraLabels map[string]string
	metricName  string
}

func (l Latency) Observe(_ context.Context, verb string, u url.URL, latency time.Duration) {
	labels := make(map[string]string, len(l.extraLabels)+2)
	for k, v := range l.extraLabels {
		labels[k] = v
	}

	labels[LabelVerb] = verb
	labels[LabelURL] = u.String()

	l.storage.HistogramObserve(l.metricName, latency.Seconds(), labels, kubernetesClientRequestBuckets)
}

// NewRequestResult returns a client-go metrics.ResultMetric backed by the
// given storage. The metric name is resolved from the template using prefix at
// construction time. extraLabels are added to each emitted counter sample.
func NewRequestResult(storage Storage, extraLabels map[string]string, prefix string) clientgometrics.ResultMetric {
	return RequestResult{
		storage:     storage,
		extraLabels: extraLabels,
		metricName:  ReplacePrefix(KubernetesClientRequestResultTotal, prefix),
	}
}

// RequestResult counts client-go request results by code, method and host.
type RequestResult struct {
	storage     Storage
	extraLabels map[string]string
	metricName  string
}

func (r RequestResult) Increment(_ context.Context, code, method, host string) {
	labels := make(map[string]string, len(r.extraLabels)+3)
	for k, v := range r.extraLabels {
		labels[k] = v
	}

	labels[LabelCode] = code
	labels[LabelMethod] = method
	labels[LabelHost] = host

	r.storage.CounterAdd(r.metricName, 1.0, labels)
}

// mergeLabels returns a new label map containing zero-valued entries for the
// given keys plus zero-valued entries for any keys in extra.
func mergeLabels(extra map[string]string, keys ...string) map[string]string {
	out := make(map[string]string, len(extra)+len(keys))
	for k := range extra {
		out[k] = ""
	}

	for _, k := range keys {
		out[k] = ""
	}

	return out
}
