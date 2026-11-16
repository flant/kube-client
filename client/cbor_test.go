package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/features"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestCBORSupportedBy(t *testing.T) {
	tests := map[string]bool{
		"":                                   false,
		"(devel)":                            false,
		"v0.35.8":                            false,
		"v0.36.0":                            true,
		"v0.37.1":                            true,
		"v1.0.0":                             true,
		"v0.0.0-20250101000000-abcdefabcdef": false,
	}

	for v, want := range tests {
		assert.Equal(t, want, cborSupportedBy(v), v)
	}
}

func TestCheckServerDecodesLargeCBOR(t *testing.T) {
	tests := map[string]bool{
		"35":  false,
		"36":  true,
		"36+": true, // managed clusters append a suffix to the minor version
		"":    false,
	}

	for minor, want := range tests {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"major":"1","minor":%q,"gitVersion":"v1.%s.0"}`, minor, minor)
		}))

		iface, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
		require.NoError(t, err)

		c := New()
		c.Interface = iface

		err = c.checkServerDecodesLargeCBOR(context.Background())
		assert.Equal(t, want, err == nil, "minor %q: %v", minor, err)

		srv.Close()
	}
}

// Without a known-good apimachinery the option must leave the gate alone:
// a decoder limited to 1024 elements breaks large lists.
func TestWithCBORKeepsGateOffOnUnknownApimachinery(t *testing.T) {
	t.Setenv("FLANT_KUBE_CLIENT_IN_MEMORY_DISCOVERY_CACHE", "1")

	require.False(t, cborSupportedBy(apimachineryVersion()), "test binary is expected to have no usable build info")

	c := New()
	c.WithRestConfig(&rest.Config{Host: "https://127.0.0.1:6443"})
	c.WithCBOR(true)
	c.WithCBORRequests(true)

	require.NoError(t, c.Init())
	assert.False(t, features.FeatureGates().Enabled(features.ClientsAllowCBOR))
	assert.False(t, features.FeatureGates().Enabled(features.ClientsPreferCBOR))
}
