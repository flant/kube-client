package client

import (
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// cachedFakeDiscovery wraps FakeDiscovery to satisfy CachedDiscoveryInterface
// (adds Fresh and Invalidate methods needed by Client.discovery()).
type cachedFakeDiscovery struct {
	*fakediscovery.FakeDiscovery
	fresh bool
}

func (d *cachedFakeDiscovery) Fresh() bool { return d.fresh }
func (d *cachedFakeDiscovery) Invalidate() { d.fresh = false }

// newClientWithFakeDiscovery creates a Client wired up with a fake discovery
// that contains some API resources, suitable for testing concurrent access.
func newClientWithFakeDiscovery() *Client {
	k8sClient := fake.NewSimpleClientset()
	fd := &cachedFakeDiscovery{
		FakeDiscovery: k8sClient.Discovery().(*fakediscovery.FakeDiscovery),
		fresh:         true,
	}

	// Populate discovery with a few resources so APIResource/APIResourceList
	// have something to find.
	fd.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{
					Name:       "pods",
					Kind:       "Pod",
					Verbs:      []string{"get", "list", "watch"},
					Namespaced: true,
				},
				{
					Name:       "namespaces",
					Kind:       "Namespace",
					Verbs:      []string{"get", "list", "watch"},
					Namespaced: false,
				},
			},
		},
		{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{
					Name:       "deployments",
					Kind:       "Deployment",
					Verbs:      []string{"get", "list", "watch", "create", "delete"},
					Namespaced: true,
				},
				{
					Name:       "replicasets",
					Kind:       "ReplicaSet",
					Verbs:      []string{"get", "list", "watch"},
					Namespaced: true,
				},
			},
		},
		{
			GroupVersion: "batch/v1",
			APIResources: []metav1.APIResource{
				{
					Name:       "jobs",
					Kind:       "Job",
					Verbs:      []string{"get", "list", "watch", "create", "delete"},
					Namespaced: true,
				},
				{
					Name:       "cronjobs",
					Kind:       "CronJob",
					Verbs:      []string{"get", "list", "watch"},
					Namespaced: true,
				},
			},
		},
	}

	c := New()
	c.Interface = k8sClient
	c.cachedDiscovery = fd

	return c
}

// TestConcurrentAPIResourceNoRace verifies that concurrent calls to
// APIResource, APIResourceList and InvalidateDiscoveryCache do not trigger
// a data race.
//
// Run with: go test ./client/... -race -run TestConcurrent -count=100
func TestConcurrentAPIResourceNoRace(_ *testing.T) {
	c := newClientWithFakeDiscovery()

	const (
		goroutines = 50
		iterations = 20
	)

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Goroutines calling APIResource.
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()

			for j := 0; j < iterations; j++ {
				_, _ = c.APIResource("apps/v1", "Deployment")
			}
		}()
	}

	// Goroutines calling InvalidateDiscoveryCache — the most dangerous
	// scenario because it mutates shared cached state.
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()

			for j := 0; j < iterations; j++ {
				c.InvalidateDiscoveryCache()
			}
		}()
	}

	wg.Wait()
}

// TestConcurrentAPIResourceListNoRace exercises APIResourceList with
// concurrent invalidation.
func TestConcurrentAPIResourceListNoRace(_ *testing.T) {
	c := newClientWithFakeDiscovery()

	const (
		goroutines = 50
		iterations = 30
	)

	var wg sync.WaitGroup
	wg.Add(goroutines*2 + 5)

	apiVersions := []string{"apps/v1", "batch/v1", "v1", ""}

	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()

			for j := 0; j < iterations; j++ {
				_, _ = c.APIResourceList(apiVersions[i%len(apiVersions)])
			}
		}(i)
	}

	// APIResource calls mixed in.
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()

			for j := 0; j < iterations; j++ {
				_, _ = c.APIResource(apiVersions[i%len(apiVersions)], "Deployment")
			}
		}(i)
	}

	// Invalidate from separate goroutines.
	for i := 0; i < 5; i++ {
		go func() {
			defer wg.Done()

			for j := 0; j < iterations; j++ {
				c.InvalidateDiscoveryCache()
			}
		}()
	}

	wg.Wait()
}

// TestConcurrentGroupVersionResourceNoRace exercises the GroupVersionResource
// wrapper (which internally calls APIResource) from multiple goroutines.
func TestConcurrentGroupVersionResourceNoRace(_ *testing.T) {
	c := newClientWithFakeDiscovery()

	const (
		goroutines = 50
		iterations = 30
	)

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()

			for j := 0; j < iterations; j++ {
				_, _ = c.GroupVersionResource("apps/v1", "Deployment")
			}
		}()
	}

	wg.Wait()
}

// TestConcurrentMixedOpsNoRace is the most comprehensive test: all discovery
// methods are called concurrently with cache invalidation.
func TestConcurrentMixedOpsNoRace(_ *testing.T) {
	c := newClientWithFakeDiscovery()

	const (
		goroutines = 60
		iterations = 40
	)

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()

			for j := 0; j < iterations; j++ {
				switch i % 5 {
				case 0:
					_, _ = c.APIResource("apps/v1", "Deployment")
				case 1:
					_, _ = c.APIResourceList("apps/v1")
				case 2:
					_, _ = c.APIResourceList("") // empty triggers ServerPreferredResources
				case 3:
					_, _ = c.GroupVersionResource("apps/v1", "Deployment")
				case 4:
					c.InvalidateDiscoveryCache()
				}
			}
		}(i)
	}

	wg.Wait()
}
