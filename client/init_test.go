package client

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/cached/memory"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

func TestInitDoesNotChangeCallerRestConfig(t *testing.T) {
	t.Setenv("FLANT_KUBE_CLIENT_IN_MEMORY_DISCOVERY_CACHE", "1")

	cfg := &rest.Config{Host: "https://127.0.0.1:6443"}

	c := New()
	c.WithRestConfig(cfg)
	c.WithRateLimiterSettings(5, 10)
	c.WithAcceptOnlyJSONContentType(true)

	require.NoError(t, c.Init())

	assert.Zero(t, cfg.QPS)
	assert.Zero(t, cfg.Burst)
	assert.Empty(t, cfg.AcceptContentTypes)
	assert.Equal(t, float32(5), c.RestConfig().QPS)
}

func TestInitFailsOnMissingExplicitKubeconfig(t *testing.T) {
	c := New()
	c.WithConfigPath(t.TempDir() + "/missing-kubeconfig")

	require.Error(t, c.Init())
}

func TestReloadDynamicKeepsRealClient(t *testing.T) {
	realClient, err := dynamic.NewForConfig(&rest.Config{Host: "https://127.0.0.1:6443"})
	require.NoError(t, err)

	c := New()
	c.dynamicClient = realClient

	c.ReloadDynamic(map[schema.GroupVersionResource]string{})

	assert.Same(t, realClient, c.Dynamic())
}

func TestReloadDynamicReplacesFakeClient(t *testing.T) {
	c := NewFake(map[schema.GroupVersionResource]string{})
	before := c.Dynamic()

	c.ReloadDynamic(map[schema.GroupVersionResource]string{})

	assert.NotSame(t, before, c.Dynamic())
}

func resourceList(gv string, kinds ...string) *metav1.APIResourceList {
	list := &metav1.APIResourceList{GroupVersion: gv}
	for _, k := range kinds {
		list.APIResources = append(list.APIResources, metav1.APIResource{
			Name: k + "s", Kind: k, Verbs: metav1.Verbs{"get", "list"},
		})
	}

	return list
}

// newCachedFake returns a client whose discovery is an in-memory cache over a
// fake server, so tests can change the server after the cache is filled.
func newCachedFake(t *testing.T, server ...*metav1.APIResourceList) (*Client, *fakediscovery.FakeDiscovery) {
	t.Helper()

	c := NewFake(nil)

	fd, ok := c.Discovery().(*fakediscovery.FakeDiscovery)
	require.True(t, ok)

	fd.Resources = server
	c.cachedDiscovery = memory.NewMemCacheClient(fd)

	return c, fd
}

// A CRD installed after the cache was filled, for example by a parallel hook,
// must be found by the next lookup.
func TestAPIResourceFindsKindAddedAfterCacheFill(t *testing.T) {
	c, fd := newCachedFake(t, resourceList("a.io/v1", "Foo"))

	_, err := c.APIResource("a.io/v1", "Foo")
	require.NoError(t, err)

	fd.Resources = []*metav1.APIResourceList{
		resourceList("a.io/v1", "Foo", "Bar"),
		resourceList("b.io/v1", "Baz"),
	}

	res, err := c.APIResource("a.io/v1", "Bar")
	require.NoError(t, err)
	assert.Equal(t, "a.io", res.Group)

	res, err = c.APIResource("b.io/v1", "Baz")
	require.NoError(t, err)
	assert.Equal(t, "b.io", res.Group)
}

// Waiting for a kind that is not installed yet must not drop the whole cache
// on every call: the miss costs one request for its group version.
func TestAPIResourceMissKeepsCache(t *testing.T) {
	c, fd := newCachedFake(t,
		resourceList("a.io/v1", "Foo"),
		resourceList("b.io/v1", "Baz"),
		resourceList("c.io/v1", "Qux"),
	)

	_, err := c.APIResource("a.io/v1", "Foo")
	require.NoError(t, err)

	fd.ClearActions()

	_, err = c.APIResource("a.io/v1", "Missing")
	require.Error(t, err)
	assert.True(t, apiErrors.IsNotFound(err))
	assert.Len(t, fd.Actions(), 1, "a miss must cost one group version request, got %v", fd.Actions())
}
