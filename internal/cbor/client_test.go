package cbor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/flant/kube-client/internal/cbor/cbortest"
)

func newClient(t *testing.T, url string) *Client {
	t.Helper()

	c, err := NewClient(&rest.Config{Host: url})
	require.NoError(t, err)

	return c
}

func widgets(c *Client) dynamic.ResourceInterface {
	return c.Resource(cbortest.Widgets).Namespace("default")
}

func TestProbe(t *testing.T) {
	tests := map[string]struct {
		cbor        bool
		smallBodies bool
		want        Support
	}{
		"JSON only":         {want: None},
		"CBOR before v1.36": {cbor: true, smallBodies: true, want: Responses},
		"CBOR":              {cbor: true, want: ResponsesAndRequests},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			srv := cbortest.NewServer(t)
			srv.SetCBOR(tt.cbor)
			srv.SetSmallBodies(tt.smallBodies)

			support, err := newClient(t, srv.URL).Probe(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tt.want, support)
		})
	}
}

// An API server that cannot be asked is an error, not a server without CBOR.
func TestProbeReportsServerThatCannotBeAsked(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	support, err := newClient(t, srv.URL).Probe(context.Background())
	require.Error(t, err)
	assert.Equal(t, None, support)
}

// apimachinery before v0.36 cannot decode a list of more than 1024 objects.
func TestListLongerThanOldDecoderLimit(t *testing.T) {
	srv := cbortest.NewServer(t)
	srv.SetCBOR(true)
	srv.SetWidgets(2000)

	list, err := widgets(newClient(t, srv.URL)).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)

	assert.Len(t, list.Items, 2000)
	assert.Equal(t, acceptHeader, srv.LastRequest().Accept)
	assert.Equal(t, runtime.ContentTypeCBOR, srv.LastRequest().Response)
}

// Switching to CBOR must not change the objects callers get.
func TestCBORDecodesAsJSON(t *testing.T) {
	ctx := context.Background()

	cborServer := cbortest.NewServer(t)
	cborServer.SetCBOR(true)

	jsonServer := cbortest.NewServer(t)

	cborClient := newClient(t, cborServer.URL)
	jsonClient := newClient(t, jsonServer.URL)

	fromCBOR, err := widgets(cborClient).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, runtime.ContentTypeCBOR, cborServer.LastRequest().Response)

	fromJSON, err := widgets(jsonClient).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, runtime.ContentTypeJSON, jsonServer.LastRequest().Response)

	assert.Equal(t, fromJSON, fromCBOR)
	assert.Equal(t, "Widget", fromCBOR.Items[0].GetKind(), "items of typed lists get the kind of the list")

	oneFromCBOR, err := widgets(cborClient).Get(ctx, "widget-1", metav1.GetOptions{})
	require.NoError(t, err)

	oneFromJSON, err := widgets(jsonClient).Get(ctx, "widget-1", metav1.GetOptions{})
	require.NoError(t, err)

	assert.Equal(t, oneFromJSON, oneFromCBOR)
}

// A CBOR Status must become the same API error as a JSON one.
func TestErrorStatus(t *testing.T) {
	srv := cbortest.NewServer(t)
	srv.SetCBOR(true)

	_, err := widgets(newClient(t, srv.URL)).Get(context.Background(), "missing", metav1.GetOptions{})

	assert.True(t, apierrors.IsNotFound(err), err)
	assert.Equal(t, runtime.ContentTypeCBOR, srv.LastRequest().Response)
}

func TestWatch(t *testing.T) {
	srv := cbortest.NewServer(t)
	srv.SetCBOR(true)

	w, err := widgets(newClient(t, srv.URL)).Watch(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)

	defer w.Stop()

	var names []string

	for event := range w.ResultChan() {
		require.Equal(t, watch.Added, event.Type, event.Object)

		obj, ok := event.Object.(*unstructured.Unstructured)
		require.True(t, ok, event.Object)

		names = append(names, obj.GetName())
	}

	assert.Equal(t, []string{"widget-0", "widget-1", "widget-2"}, names)
	assert.Equal(t, runtime.ContentTypeCBORSequence, srv.LastRequest().Response)
}

func newWidget(name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "Widget",
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	}}
}

// probedClient returns a client that sends CBOR bodies.
func probedClient(t *testing.T, srv *cbortest.Server) *Client {
	t.Helper()

	c := newClient(t, srv.URL)

	support, err := c.Probe(context.Background())
	require.NoError(t, err)
	require.Equal(t, ResponsesAndRequests, support)

	return c
}

// lastTwo returns the last two requests the server received.
func lastTwo(srv *cbortest.Server) (cbortest.Request, cbortest.Request) {
	requests := srv.Requests()

	return requests[len(requests)-2], requests[len(requests)-1]
}

func TestCreateSendsCBORAfterProbe(t *testing.T) {
	ctx := context.Background()

	srv := cbortest.NewServer(t)
	srv.SetCBOR(true)

	c := newClient(t, srv.URL)
	obj := newWidget("new", map[string]any{"replicas": int64(3), "ratio": 0.5, "tags": []any{"a"}})

	_, err := widgets(c).Create(ctx, obj, metav1.CreateOptions{})
	require.NoError(t, err)
	assert.Equal(t, runtime.ContentTypeJSON, srv.LastRequest().ContentType, "before Probe bodies are JSON")

	support, err := c.Probe(ctx)
	require.NoError(t, err)
	require.Equal(t, ResponsesAndRequests, support)

	created, err := widgets(c).Create(ctx, obj, metav1.CreateOptions{})
	require.NoError(t, err)
	assert.Equal(t, runtime.ContentTypeCBOR, srv.LastRequest().ContentType)
	assert.Equal(t, obj.Object["spec"], created.Object["spec"])
}

// An API server that stops reading CBOR answers 415: the request is sent
// again in JSON, and later bodies go in JSON.
func TestCreateFallsBackToJSONOnUnsupportedMediaType(t *testing.T) {
	ctx := context.Background()

	srv := cbortest.NewServer(t)
	srv.SetCBOR(true)

	c := probedClient(t, srv)
	obj := newWidget("new", map[string]any{"replicas": int64(3)})

	srv.SetCBOR(false)

	_, err := widgets(c).Create(ctx, obj, metav1.CreateOptions{})
	require.NoError(t, err)

	rejected, repeated := lastTwo(srv)
	assert.Equal(t, runtime.ContentTypeCBOR, rejected.ContentType)
	assert.Equal(t, http.StatusUnsupportedMediaType, rejected.Status)
	assert.Equal(t, runtime.ContentTypeJSON, repeated.ContentType)
	assert.Equal(t, http.StatusCreated, repeated.Status)

	_, err = widgets(c).Create(ctx, obj, metav1.CreateOptions{})
	require.NoError(t, err)
	assert.Equal(t, runtime.ContentTypeJSON, srv.LastRequest().ContentType)
}

// During an upgrade Probe may reach an API server of v1.36 and a later request
// an older one, which rejects a large CBOR body with 400. The request is sent
// again in JSON, and later bodies go in JSON.
func TestCreateFallsBackToJSONOnOlderAPIServer(t *testing.T) {
	ctx := context.Background()

	srv := cbortest.NewServer(t)
	srv.SetCBOR(true)

	c := probedClient(t, srv)

	srv.SetSmallBodies(true)

	labels := make(map[string]any, largeMap)
	for i := range largeMap {
		labels["label-"+strconv.Itoa(i)] = "x"
	}

	_, err := widgets(c).Create(ctx, newWidget("large", map[string]any{"labels": labels}), metav1.CreateOptions{})
	require.NoError(t, err)

	rejected, repeated := lastTwo(srv)
	assert.Equal(t, runtime.ContentTypeCBOR, rejected.ContentType)
	assert.Equal(t, http.StatusBadRequest, rejected.Status)
	assert.Equal(t, runtime.ContentTypeJSON, repeated.ContentType)
	assert.Equal(t, http.StatusCreated, repeated.Status)

	_, err = widgets(c).Create(ctx, newWidget("small", nil), metav1.CreateOptions{})
	require.NoError(t, err)
	assert.Equal(t, runtime.ContentTypeJSON, srv.LastRequest().ContentType)
}

// A request that is wrong in any format must not turn CBOR bodies off.
func TestBadRequestKeepsCBORBodies(t *testing.T) {
	ctx := context.Background()

	srv := cbortest.NewServer(t)
	srv.SetCBOR(true)

	c := probedClient(t, srv)

	_, err := widgets(c).Create(ctx, newWidget(cbortest.BadRequestName, nil), metav1.CreateOptions{})
	assert.True(t, apierrors.IsBadRequest(err), err)

	rejected, repeated := lastTwo(srv)
	assert.Equal(t, runtime.ContentTypeCBOR, rejected.ContentType)
	assert.Equal(t, runtime.ContentTypeJSON, repeated.ContentType)
	assert.Equal(t, http.StatusBadRequest, repeated.Status)

	_, err = widgets(c).Create(ctx, newWidget("good", nil), metav1.CreateOptions{})
	require.NoError(t, err)
	assert.Equal(t, runtime.ContentTypeCBOR, srv.LastRequest().ContentType)
}
