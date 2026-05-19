package client

import "github.com/prometheus/client_golang/prometheus"

// MetricStorage is the interface that metric backends must implement to
// receive Kubernetes client metrics. It mirrors the Storage interface in
// internal/metrics so that callers outside this module can implement it
// without importing the internal package.
//
// Implementations are typically provided by flant/shell-operator's
// metrics-storage package or any Prometheus-backed equivalent.
type MetricStorage interface {
	RegisterCounter(metric string, labels map[string]string) *prometheus.CounterVec
	CounterAdd(metric string, value float64, labels map[string]string)
	RegisterHistogram(metric string, labels map[string]string, buckets []float64) *prometheus.HistogramVec
	HistogramObserve(metric string, value float64, labels map[string]string, buckets []float64)
}
