package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/features"
	"k8s.io/client-go/rest"

	internalcbor "github.com/flant/kube-client/internal/cbor"
	"github.com/flant/kube-client/internal/cbor/cbortest"
)

func initClient(t *testing.T, srv *cbortest.Server, opts ...func(*Client)) *Client {
	t.Helper()
	t.Setenv("FLANT_KUBE_CLIENT_IN_MEMORY_DISCOVERY_CACHE", "1")

	c := New()
	c.WithRestConfig(&rest.Config{Host: srv.URL})

	for _, opt := range opts {
		opt(c)
	}

	require.NoError(t, c.Init())

	return c
}

func listWidgets(t *testing.T, c *Client) {
	t.Helper()

	_, err := c.Dynamic().Resource(cbortest.Widgets).Namespace("default").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
}

func TestInitSwitchesToCBORWhenTheAPIServerSupportsIt(t *testing.T) {
	srv := cbortest.NewServer(t)
	srv.SetCBOR(true)

	listWidgets(t, initClient(t, srv))

	assert.Equal(t, runtime.ContentTypeCBOR, srv.LastRequest().Response)
	// Other clients of the process must stay as they are.
	assert.False(t, features.FeatureGates().Enabled(features.ClientsAllowCBOR))
	assert.False(t, features.FeatureGates().Enabled(features.ClientsPreferCBOR))
}

func TestInitAsksAgainWhenTheAPIServerWasUnavailable(t *testing.T) {
	t.Cleanup(internalcbor.SetRetryIntervals(time.Nanosecond, time.Nanosecond))

	srv := cbortest.NewServer(t)
	srv.SetCBOR(true)
	srv.SetUnavailable(true)

	c := initClient(t, srv)
	widgets := c.Dynamic().Resource(cbortest.Widgets).Namespace("default")

	srv.SetUnavailable(false)

	assert.Eventually(t, func() bool {
		_, err := widgets.List(context.Background(), metav1.ListOptions{})
		last := srv.LastRequest()

		return err == nil && last.Path == cbortest.WidgetsPath && last.Response == runtime.ContentTypeCBOR
	}, 5*time.Second, 10*time.Millisecond, "the dynamic client must switch to CBOR")
}

func TestInitKeepsJSONWhenTheAPIServerHasNoCBOR(t *testing.T) {
	srv := cbortest.NewServer(t)

	listWidgets(t, initClient(t, srv))

	assert.Equal(t, runtime.ContentTypeJSON, srv.LastRequest().Accept)
}

func TestWithCBORFalseAsksNothing(t *testing.T) {
	srv := cbortest.NewServer(t)
	srv.SetCBOR(true)

	c := initClient(t, srv, func(c *Client) { c.WithCBOR(false) })

	assert.Empty(t, srv.Requests())

	listWidgets(t, c)
	assert.Equal(t, runtime.ContentTypeJSON, srv.LastRequest().Response)
}

func TestAcceptOnlyJSONTurnsCBOROff(t *testing.T) {
	srv := cbortest.NewServer(t)
	srv.SetCBOR(true)

	initClient(t, srv, func(c *Client) { c.WithAcceptOnlyJSONContentType(true) })

	assert.Empty(t, srv.Requests())
}
