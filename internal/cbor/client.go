// Package cbor lets the dynamic client of client-go talk CBOR to API servers
// that support it, with client-go v0.32 and later.
//
// client-go itself reads and writes CBOR only while its process-wide feature
// gates ClientsAllowCBOR and ClientsPreferCBOR are on, which changes every
// client in the process, and before v0.36 its decoder rejects arrays and maps
// with more than 1024 elements, so it cannot read a list of more than 1024
// objects. This package leaves the gates alone: it asks for CBOR in each
// request, decodes it without that limit and converts request bodies to CBOR
// in the transport.
package cbor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"sync/atomic"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	apicbor "k8s.io/apimachinery/pkg/runtime/serializer/cbor"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// Support is what an API server can do with CBOR.
type Support int

const (
	// None means the API server talks only JSON.
	None Support = iota
	// Responses means the API server sends CBOR but rejects CBOR bodies with
	// more than 1024 elements in an array or map, as servers before v1.36 do.
	Responses
	// ResponsesAndRequests means the API server sends and reads CBOR.
	ResponsesAndRequests
)

// acceptHeader prefers CBOR; an API server without it answers in JSON.
const acceptHeader = "application/cbor;q=1,application/json;q=0.9"

// selfSubjectReviewPath is asked about CBOR: every authenticated user may
// create a SelfSubjectReview, and nothing is stored.
const selfSubjectReviewPath = "/apis/authentication.k8s.io/v1/selfsubjectreviews"

// largeMap is one entry more than API servers before v1.36 accept in a CBOR map.
const largeMap = 1025

// selfDescribedCBOR starts everything the CBOR encoder of apimachinery writes.
var selfDescribedCBOR = []byte{0xd9, 0xd9, 0xf7}

// unstructuredEncoder writes CBOR as the dynamic client of client-go does.
var unstructuredEncoder = apicbor.NewSerializer(unstructuredCreater{}, unstructuredTyper{})

// Client is a dynamic client that asks for CBOR responses. After Probe finds
// that the API server reads CBOR, it also sends create and update bodies in
// CBOR.
type Client struct {
	*dynamic.DynamicClient

	rest   *rest.RESTClient
	bodies *atomic.Bool
}

// NewClient creates a Client. It makes no requests.
func NewClient(config *rest.Config) (*Client, error) {
	cfg := dynamic.ConfigFor(config)
	// As dynamic.NewForConfigAndClient does: the dynamic client builds
	// absolute paths.
	cfg.GroupVersion = nil
	cfg.APIPath = "/if-you-see-this-search-for-the-break"
	cfg.NegotiatedSerializer = newNegotiatedSerializer(cfg.NegotiatedSerializer)

	bodies := &atomic.Bool{}

	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return &bodyTransport{next: rt, enabled: bodies}
	})

	rc, err := rest.UnversionedRESTClientFor(cfg)
	if err != nil {
		return nil, err
	}

	return &Client{
		DynamicClient: dynamic.New(acceptCBOR{rc}),
		rest:          rc,
		bodies:        bodies,
	}, nil
}

// Probe asks the API server what it can do with CBOR and, if it reads CBOR
// bodies, makes c send them. It creates up to two SelfSubjectReviews.
//
// An API server without CBOR answers 406 or 415, which is not an error; an
// error means the API server could not be asked.
func (c *Client) Probe(ctx context.Context) (Support, error) {
	// API servers before v1.36 reject this body with 400: their CBOR decoder
	// accepts at most 1024 entries in a map.
	err := c.createReview(ctx, runtime.ContentTypeCBOR, largeMap)

	switch {
	case err == nil:
		c.bodies.Store(true)

		return ResponsesAndRequests, nil
	case apierrors.IsNotAcceptable(err), apierrors.IsUnsupportedMediaType(err):
		return None, nil
	case !apierrors.IsBadRequest(err):
		return None, err
	}

	err = c.createReview(ctx, runtime.ContentTypeJSON, 0)

	switch {
	case err == nil:
		return Responses, nil
	case apierrors.IsNotAcceptable(err):
		return None, nil
	}

	return None, err
}

// createReview creates a SelfSubjectReview with a body in contentType that
// has the given number of annotations and asks for the answer in CBOR. It
// fails unless the answer is CBOR that c can decode.
func (c *Client) createReview(ctx context.Context, contentType string, annotations int) error {
	body, err := reviewBody(contentType, annotations)
	if err != nil {
		return err
	}

	result := c.rest.Post().
		AbsPath(selfSubjectReviewPath).
		SetHeader("Content-Type", contentType).
		SetHeader("Accept", runtime.ContentTypeCBOR).
		Body(body).
		Do(ctx)
	if err := result.Error(); err != nil {
		return err
	}

	raw, err := result.Raw()
	if err != nil {
		return err
	}

	if !bytes.HasPrefix(raw, selfDescribedCBOR) {
		return errors.New("the API server did not answer in CBOR")
	}

	var review unstructured.Unstructured
	if err := result.Into(&review); err != nil {
		return fmt.Errorf("decode the CBOR answer of the API server: %w", err)
	}

	if review.GetKind() != "SelfSubjectReview" {
		return fmt.Errorf("the API server answered with %q instead of SelfSubjectReview", review.GetKind())
	}

	return nil
}

func reviewBody(contentType string, annotations int) ([]byte, error) {
	review := &unstructured.Unstructured{}
	review.SetAPIVersion("authentication.k8s.io/v1")
	review.SetKind("SelfSubjectReview")

	if annotations > 0 {
		values := make(map[string]string, annotations)
		for i := range annotations {
			values["kube-client-cbor-probe-"+strconv.Itoa(i)] = ""
		}

		review.SetAnnotations(values)
	}

	if contentType == runtime.ContentTypeCBOR {
		return runtime.Encode(unstructuredEncoder, review)
	}

	return runtime.Encode(unstructured.UnstructuredJSONScheme, review)
}

// acceptCBOR asks for CBOR in every request of the dynamic client. client-go
// removes CBOR from the Accept header of a client while its feature gate is
// off, so the header is set on each request instead.
type acceptCBOR struct {
	*rest.RESTClient
}

func (c acceptCBOR) Verb(verb string) *rest.Request {
	return c.RESTClient.Verb(verb).SetHeader("Accept", acceptHeader)
}

func (c acceptCBOR) Post() *rest.Request {
	return c.Verb(http.MethodPost)
}

func (c acceptCBOR) Put() *rest.Request {
	return c.Verb(http.MethodPut)
}

func (c acceptCBOR) Patch(pt types.PatchType) *rest.Request {
	return c.RESTClient.Patch(pt).SetHeader("Accept", acceptHeader)
}

func (c acceptCBOR) Get() *rest.Request {
	return c.Verb(http.MethodGet)
}

func (c acceptCBOR) Delete() *rest.Request {
	return c.Verb(http.MethodDelete)
}

// bodyTransport sends the JSON bodies of create and update requests in CBOR
// while enabled: client-go writes CBOR bodies only while its feature gate is
// on.
//
// If the API server rejects a CBOR body with 415 Unsupported Media Type or
// 400 Bad Request, it has not applied the request, so the request is sent
// again in JSON. CBOR bodies are then turned off, unless JSON got the same
// 400: that request was wrong in any format. A 400 only to CBOR comes, for
// example, from an API server before v1.36 behind the same address as a newer
// one, during an upgrade.
type bodyTransport struct {
	next    http.RoundTripper
	enabled *atomic.Bool
}

func (t *bodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.enabled.Load() || !isJSONWrite(req) {
		return t.next.RoundTrip(req)
	}

	jsonBody, err := io.ReadAll(req.Body)
	_ = req.Body.Close()

	if err != nil {
		return nil, err
	}

	cborBody, err := jsonToCBOR(jsonBody)
	if err != nil {
		// For example, an object without a kind, which the dynamic client
		// allows: send it as it is.
		return t.next.RoundTrip(withBody(req, runtime.ContentTypeJSON, jsonBody))
	}

	resp, err := t.next.RoundTrip(withBody(req, runtime.ContentTypeCBOR, cborBody))
	if err != nil || (resp.StatusCode != http.StatusUnsupportedMediaType && resp.StatusCode != http.StatusBadRequest) {
		return resp, err
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	retry, err := t.next.RoundTrip(withBody(req, runtime.ContentTypeJSON, jsonBody))
	if resp.StatusCode == http.StatusUnsupportedMediaType || (err == nil && retry.StatusCode != http.StatusBadRequest) {
		t.enabled.Store(false)
	}

	return retry, err
}

func isJSONWrite(req *http.Request) bool {
	if req.Method != http.MethodPost && req.Method != http.MethodPut {
		return false
	}

	if req.Body == nil || req.Body == http.NoBody {
		return false
	}

	mediaType, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))

	return err == nil && mediaType == runtime.ContentTypeJSON
}

// withBody returns a copy of req with the given body.
func withBody(req *http.Request, contentType string, body []byte) *http.Request {
	out := req.Clone(req.Context())
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	out.ContentLength = int64(len(body))
	out.Header.Set("Content-Type", contentType)

	return out
}

// jsonToCBOR converts an object as the dynamic client of client-go does when
// it writes CBOR itself: through Unstructured.
func jsonToCBOR(data []byte) ([]byte, error) {
	obj, _, err := unstructured.UnstructuredJSONScheme.Decode(data, nil, nil)
	if err != nil {
		return nil, err
	}

	return runtime.Encode(unstructuredEncoder, obj)
}
