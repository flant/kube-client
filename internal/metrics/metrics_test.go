package metrics

import (
	"context"
	"net/url"
	"sort"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
)

// recordingStorage remembers registered label keys and observed samples.
type recordingStorage struct {
	histograms map[string][]string
	observed   map[string][]string
}

func (s *recordingStorage) RegisterCounter(string, map[string]string) *prometheus.CounterVec {
	return nil
}

func (s *recordingStorage) CounterAdd(string, float64, map[string]string) {}

func (s *recordingStorage) RegisterHistogram(metric string, labels map[string]string, _ []float64) *prometheus.HistogramVec {
	s.histograms[metric] = keys(labels)
	return nil
}

func (s *recordingStorage) HistogramObserve(metric string, _ float64, labels map[string]string, _ []float64) {
	s.observed[metric] = keys(labels)
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// Each latency backend must write to a registered histogram with exactly the
// registered label set, otherwise the sample is dropped or the vector panics.
func TestLatencyBackendsMatchRegisteredHistograms(t *testing.T) {
	s := &recordingStorage{histograms: map[string][]string{}, observed: map[string][]string{}}
	extra := map[string]string{"operator": "test"}

	RegisterKubernetesClientMetrics(s, extra, "p_")

	u := url.URL{Scheme: "https", Host: "api", Path: "/api/v1/pods"}
	NewRequestLatency(s, extra, "p_").Observe(context.Background(), "GET", u, time.Second)
	NewRateLimiterLatency(s, extra, "p_").Observe(context.Background(), "GET", u, time.Second)

	for _, name := range []string{
		"p_kubernetes_client_request_latency_seconds",
		"p_kubernetes_client_rate_limiter_latency_seconds",
	} {
		assert.Contains(t, s.histograms, name)
		assert.Equal(t, s.histograms[name], s.observed[name], name)
	}
}
