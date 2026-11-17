// Package cbor lets the dynamic client of client-go talk CBOR to API servers
// that support it, with client-go v0.32 and later.
//
// client-go itself reads and writes CBOR only while its process-wide feature
// gates ClientsAllowCBOR and ClientsPreferCBOR are on, which changes every
// client in the process, and before v0.36 its decoder rejects arrays and maps
// with more than 1024 elements, so it cannot read a list of more than 1024
// objects. This package leaves the gates alone: once the API server confirms
// CBOR, it asks for CBOR in each request, decodes it without that limit and
// converts request bodies to CBOR in the transport.
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
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	apicbor "k8s.io/apimachinery/pkg/runtime/serializer/cbor"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// Support is what an API server can do with CBOR.
type Support int32

const (
	// None means the API server talks only JSON.
	None Support = iota
	// Responses means the API server sends CBOR but rejects CBOR bodies with
	// more than 1024 elements in an array or map, as servers before v1.36 do.
	Responses
	// ResponsesAndRequests means the API server sends and reads CBOR.
	ResponsesAndRequests
)

// ProbeTimeout bounds one call of Probe that ProbeLater makes.
const ProbeTimeout = 5 * time.Second

// Intervals between the calls of Probe that ProbeLater makes.
var (
	retryMinInterval = 10 * time.Second
	retryMaxInterval = 5 * time.Minute
)

// SetRetryIntervals changes the intervals between the calls of Probe that
// ProbeLater makes, for clients created after it, and returns a function that
// restores them. It is meant for tests.
func SetRetryIntervals(minInterval, maxInterval time.Duration) func() {
	oldMin, oldMax := retryMinInterval, retryMaxInterval
	retryMinInterval, retryMaxInterval = minInterval, maxInterval

	return func() {
		retryMinInterval, retryMaxInterval = oldMin, oldMax
	}
}

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

// Client is a dynamic client that talks JSON until Probe finds that the API
// server supports CBOR. Then it asks for CBOR responses and, if the server
// reads CBOR, sends create and update bodies in CBOR.
type Client struct {
	*dynamic.DynamicClient

	rest    *rest.RESTClient
	support *atomic.Int32
	bodies  *atomic.Bool
	retry   *retrier
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

	c := &Client{
		rest:    rc,
		support: &atomic.Int32{},
		bodies:  bodies,
		retry:   &retrier{},
	}
	c.retry.probe = c.Probe
	c.DynamicClient = dynamic.New(acceptCBOR{RESTClient: rc, support: c.support, retry: c.retry})

	return c, nil
}

// ProbeLater calls Probe again until the API server answers it, at intervals
// growing from 10 seconds to 5 minutes. Calls are made in the background when
// c builds a request, not on a timer, so a client nobody uses asks nothing
// and leaves no goroutine behind. report gets the result of every call.
func (c *Client) ProbeLater(report func(Support, error)) {
	c.retry.plan(report)
}

// Probe asks the API server what it can do with CBOR and switches c to it. It
// creates up to two SelfSubjectReviews.
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
		c.support.Store(int32(ResponsesAndRequests))

		return ResponsesAndRequests, nil
	case apierrors.IsNotAcceptable(err), apierrors.IsUnsupportedMediaType(err):
		return None, nil
	case !apierrors.IsBadRequest(err):
		return None, err
	}

	err = c.createReview(ctx, runtime.ContentTypeJSON, 0)

	switch {
	case err == nil:
		c.support.Store(int32(Responses))

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

// acceptCBOR asks for CBOR in the requests of the dynamic client once the API
// server has confirmed it. client-go removes CBOR from the Accept header of a
// client while its feature gate is off, so the header is set on each request.
// Building a request also starts a due call of Probe planned by ProbeLater.
type acceptCBOR struct {
	*rest.RESTClient

	support *atomic.Int32
	retry   *retrier
}

func (c acceptCBOR) accept(req *rest.Request) *rest.Request {
	c.retry.trigger()

	if Support(c.support.Load()) != None {
		req.SetHeader("Accept", acceptHeader)
	}

	return req
}

func (c acceptCBOR) Verb(verb string) *rest.Request {
	return c.accept(c.RESTClient.Verb(verb))
}

func (c acceptCBOR) Post() *rest.Request {
	return c.Verb(http.MethodPost)
}

func (c acceptCBOR) Put() *rest.Request {
	return c.Verb(http.MethodPut)
}

func (c acceptCBOR) Patch(pt types.PatchType) *rest.Request {
	return c.accept(c.RESTClient.Patch(pt))
}

func (c acceptCBOR) Get() *rest.Request {
	return c.Verb(http.MethodGet)
}

func (c acceptCBOR) Delete() *rest.Request {
	return c.Verb(http.MethodDelete)
}

// retrier calls Probe until the API server answers it. A call is started in
// the background by trigger, once the interval since the previous call has
// passed.
type retrier struct {
	probe func(context.Context) (Support, error)

	planned atomic.Bool

	mu          sync.Mutex
	report      func(Support, error)
	running     bool
	next        time.Time
	interval    time.Duration
	maxInterval time.Duration
}

func (r *retrier) plan(report func(Support, error)) {
	r.mu.Lock()
	r.report = report
	r.interval = retryMinInterval
	r.maxInterval = retryMaxInterval
	r.next = time.Now().Add(r.interval)
	r.mu.Unlock()

	r.planned.Store(true)
}

func (r *retrier) trigger() {
	if !r.planned.Load() {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.running || time.Now().Before(r.next) {
		return
	}

	r.running = true

	go r.run()
}

func (r *retrier) run() {
	ctx, cancel := context.WithTimeout(context.Background(), ProbeTimeout)
	defer cancel()

	support, err := r.probe(ctx)

	r.mu.Lock()
	r.running = false

	if err == nil {
		r.planned.Store(false)
	} else {
		r.interval = min(2*r.interval, r.maxInterval)
		r.next = time.Now().Add(wait.Jitter(r.interval, 0.2))
	}

	report := r.report
	r.mu.Unlock()

	if report != nil {
		report(support, err)
	}
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
