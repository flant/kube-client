package client

import (
	"testing"

	"k8s.io/client-go/rest"
)

// Two clients in one process must not fight over prometheus.DefaultRegisterer.
// Each Init creates its own defaultMetricStorage, so the per-instance dedup map
// always misses on the second client; the registry must absorb the duplicate.
func TestInitTwiceDoesNotPanic(t *testing.T) {
	t.Setenv("FLANT_KUBE_CLIENT_IN_MEMORY_DISCOVERY_CACHE", "1")

	for i := range 2 {
		c := New()
		c.WithRestConfig(&rest.Config{Host: "https://127.0.0.1:6443"})

		if err := c.Init(); err != nil {
			t.Fatalf("client #%d: Init: %v", i+1, err)
		}
	}
}

// A duplicate registration must yield the collector that is already in the
// registry, so samples from every client land in the same series instead of
// being dropped.
func TestDefaultMetricStorageReusesExistingCollector(t *testing.T) {
	labels := map[string]string{"verb": "", "url": ""}
	buckets := []float64{0.1, 1}

	first := newDefaultMetricStorage()
	second := newDefaultMetricStorage()

	hv := first.RegisterHistogram("test_reuse_histogram_seconds", labels, buckets)
	if hv == nil {
		t.Fatal("first RegisterHistogram returned nil")
	}

	if got := second.RegisterHistogram("test_reuse_histogram_seconds", labels, buckets); got != hv {
		t.Errorf("histogram: got %p, want the already registered %p", got, hv)
	}

	cv := first.RegisterCounter("test_reuse_counter_total", labels)
	if cv == nil {
		t.Fatal("first RegisterCounter returned nil")
	}

	if got := second.RegisterCounter("test_reuse_counter_total", labels); got != cv {
		t.Errorf("counter: got %p, want the already registered %p", got, cv)
	}
}
