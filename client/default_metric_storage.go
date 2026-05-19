package client

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// defaultMetricStorage is a MetricStorage backed by prometheus.DefaultRegisterer.
// It is used when no explicit storage is provided via WithMetricStorage.
type defaultMetricStorage struct {
	mu         sync.Mutex
	counters   map[string]*prometheus.CounterVec
	histograms map[string]*prometheus.HistogramVec
}

func newDefaultMetricStorage() *defaultMetricStorage {
	return &defaultMetricStorage{
		counters:   make(map[string]*prometheus.CounterVec),
		histograms: make(map[string]*prometheus.HistogramVec),
	}
}

func (s *defaultMetricStorage) RegisterCounter(metric string, labels map[string]string) *prometheus.CounterVec {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cv, ok := s.counters[metric]; ok {
		return cv
	}

	cv := prometheus.NewCounterVec(prometheus.CounterOpts{Name: metric}, labelKeys(labels))
	prometheus.MustRegister(cv)
	s.counters[metric] = cv

	return cv
}

func (s *defaultMetricStorage) CounterAdd(metric string, value float64, labels map[string]string) {
	s.mu.Lock()
	cv := s.counters[metric]
	s.mu.Unlock()

	if cv == nil {
		return
	}

	cv.With(prometheus.Labels(labels)).Add(value)
}

func (s *defaultMetricStorage) RegisterHistogram(metric string, labels map[string]string, buckets []float64) *prometheus.HistogramVec {
	s.mu.Lock()
	defer s.mu.Unlock()

	if hv, ok := s.histograms[metric]; ok {
		return hv
	}

	hv := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    metric,
		Buckets: buckets,
	}, labelKeys(labels))
	prometheus.MustRegister(hv)
	s.histograms[metric] = hv

	return hv
}

func (s *defaultMetricStorage) HistogramObserve(metric string, value float64, labels map[string]string, _ []float64) {
	s.mu.Lock()
	hv := s.histograms[metric]
	s.mu.Unlock()

	if hv == nil {
		return
	}

	hv.With(prometheus.Labels(labels)).Observe(value)
}

// labelKeys returns the sorted label key slice from a label map.
func labelKeys(labels map[string]string) []string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}

	return keys
}
