// Package cbortest provides a fake API server for tests of CBOR support.
package cbortest

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	fxcbor "github.com/fxamacker/cbor/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured/unstructuredscheme"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apicbor "k8s.io/apimachinery/pkg/runtime/serializer/cbor"
)

// Widgets is served in namespace "default" at WidgetsPath.
var Widgets = schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"}

const (
	WidgetsPath           = "/apis/example.com/v1/namespaces/default/widgets"
	SelfSubjectReviewPath = "/apis/authentication.k8s.io/v1/selfsubjectreviews"
	// BadRequestName is the name of an object the server rejects with 400.
	BadRequestName = "bad-request"
)

// Request is a request the server received.
type Request struct {
	Method string
	Path   string
	Accept string
	// ContentType is the media type of the request body.
	ContentType string
	// Response is the media type of the response.
	Response string
	Status   int
}

// Server is a fake API server. Like kube-apiserver, it picks the media type
// of the response from the Accept header and encodes CBOR with apimachinery.
type Server struct {
	*httptest.Server

	mu          sync.Mutex
	unavailable bool
	cbor        bool
	smallBodies bool
	widgets     int
	requests    []Request
}

// NewServer starts a server without CBOR that lists 3 widgets.
func NewServer(t testing.TB) *Server {
	t.Helper()

	s := &Server{widgets: 3}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)

	return s
}

// SetUnavailable makes the server answer every request with 503 Service
// Unavailable.
func (s *Server) SetUnavailable(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.unavailable = on
}

// SetCBOR makes the server send and read CBOR, as the CBORServingAndStorage
// feature gate of kube-apiserver does.
func (s *Server) SetCBOR(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cbor = on
}

// SetSmallBodies makes the server reject CBOR bodies with more than 1024
// elements in an array or map, as API servers before v1.36 do.
func (s *Server) SetSmallBodies(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.smallBodies = on
}

// SetWidgets sets the number of widgets.
func (s *Server) SetWidgets(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.widgets = n
}

// Requests returns the requests received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]Request(nil), s.requests...)
}

// LastRequest returns the last request received.
func (s *Server) LastRequest() Request {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.requests) == 0 {
		return Request{}
	}

	return s.requests[len(s.requests)-1]
}

// RequestsTo returns the number of requests received for path.
func (s *Server) RequestsTo(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0

	for _, req := range s.requests {
		if req.Path == path {
			n++
		}
	}

	return n
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	unavailable, cbor, smallBodies, widgets := s.unavailable, s.cbor, s.smallBodies, s.widgets
	s.mu.Unlock()

	req := Request{Method: r.Method, Path: r.URL.Path, Accept: r.Header.Get("Accept")}
	req.ContentType, _, _ = mime.ParseMediaType(r.Header.Get("Content-Type"))

	mediaType := negotiate(req.Accept, cbor)

	switch {
	case unavailable:
		req.Response, req.Status = writeStatus(w, runtime.ContentTypeJSON, http.StatusServiceUnavailable)
	case mediaType == "":
		req.Response, req.Status = writeStatus(w, runtime.ContentTypeJSON, http.StatusNotAcceptable)
	case r.Method == http.MethodPost && (r.URL.Path == SelfSubjectReviewPath || r.URL.Path == WidgetsPath):
		req.Response, req.Status = create(w, r, mediaType, cbor, smallBodies)
	case r.Method == http.MethodGet && r.URL.Path == WidgetsPath && r.URL.Query().Get("watch") == "true":
		req.Response, req.Status = watchWidgets(w, mediaType, widgets)
	case r.Method == http.MethodGet && r.URL.Path == WidgetsPath:
		req.Response, req.Status = write(w, mediaType, http.StatusOK, widgetList(widgets))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, WidgetsPath+"/widget-"):
		req.Response, req.Status = getWidget(w, mediaType, strings.TrimPrefix(r.URL.Path, WidgetsPath+"/widget-"), widgets)
	default:
		req.Response, req.Status = writeStatus(w, mediaType, http.StatusNotFound)
	}

	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()
}

// negotiate returns the media type of the response, or "" if the server
// cannot send any type the client accepts. CBOR wins when both are accepted.
func negotiate(accept string, cbor bool) string {
	if accept == "" {
		return runtime.ContentTypeJSON
	}

	var acceptsCBOR, acceptsJSON bool

	for part := range strings.SplitSeq(accept, ",") {
		mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}

		switch mediaType {
		case runtime.ContentTypeCBOR:
			acceptsCBOR = true
		case runtime.ContentTypeJSON, "*/*":
			acceptsJSON = true
		}
	}

	switch {
	case acceptsCBOR && cbor:
		return runtime.ContentTypeCBOR
	case acceptsJSON:
		return runtime.ContentTypeJSON
	}

	return ""
}

// create answers with the object in the request body, as kube-apiserver
// answers a create request.
func create(w http.ResponseWriter, r *http.Request, mediaType string, cbor, smallBodies bool) (string, int) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return writeStatus(w, mediaType, http.StatusBadRequest)
	}

	var obj *unstructured.Unstructured

	switch contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); {
	case contentType == runtime.ContentTypeJSON:
		decoded, _, err := unstructured.UnstructuredJSONScheme.Decode(data, nil, nil)
		if err != nil {
			return writeStatus(w, mediaType, http.StatusBadRequest)
		}

		obj, _ = decoded.(*unstructured.Unstructured)
	case contentType == runtime.ContentTypeCBOR && cbor:
		mode := largeBodies
		if smallBodies {
			mode = smallBodiesMode
		}

		var content map[string]any
		if err := mode.Unmarshal(data, &content); err != nil {
			return writeStatus(w, mediaType, http.StatusBadRequest)
		}

		obj = &unstructured.Unstructured{Object: content}
	default:
		return writeStatus(w, mediaType, http.StatusUnsupportedMediaType)
	}

	// A request that is wrong in any format.
	if obj.GetName() == BadRequestName {
		return writeStatus(w, mediaType, http.StatusBadRequest)
	}

	if obj.GetKind() == "SelfSubjectReview" {
		obj = &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "authentication.k8s.io/v1",
			"kind":       "SelfSubjectReview",
			"status":     map[string]any{"userInfo": map[string]any{"username": "test"}},
		}}
	}

	return write(w, mediaType, http.StatusCreated, obj)
}

func getWidget(w http.ResponseWriter, mediaType, suffix string, widgets int) (string, int) {
	i, err := strconv.Atoi(suffix)
	if err != nil || i < 0 || i >= widgets {
		return writeStatus(w, mediaType, http.StatusNotFound)
	}

	obj := widget(i)

	return write(w, mediaType, http.StatusOK, &obj)
}

// watchWidgets sends an ADDED event for each widget: a CBOR sequence or one
// JSON object per line, as kube-apiserver does.
func watchWidgets(w http.ResponseWriter, mediaType string, widgets int) (string, int) {
	contentType := mediaType
	if mediaType == runtime.ContentTypeCBOR {
		contentType = runtime.ContentTypeCBORSequence
	}

	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)

	for i := range widgets {
		obj := widget(i)

		raw, err := encode(mediaType, &obj)
		if err != nil {
			return contentType, http.StatusInternalServerError
		}

		event := &metav1.WatchEvent{Type: "ADDED", Object: runtime.RawExtension{Raw: raw}}

		var frame []byte
		if mediaType == runtime.ContentTypeCBOR {
			frame, err = runtime.Encode(eventEncoder, event)
		} else {
			frame, err = json.Marshal(event)
		}

		if err != nil {
			return contentType, http.StatusInternalServerError
		}

		_, _ = w.Write(frame)
	}

	return contentType, http.StatusOK
}

func write(w http.ResponseWriter, mediaType string, status int, obj runtime.Object) (string, int) {
	data, err := encode(mediaType, obj)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return "text/plain", http.StatusInternalServerError
	}

	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(status)
	_, _ = w.Write(data)

	return mediaType, status
}

// writeStatus answers with a Status, as kube-apiserver answers errors.
func writeStatus(w http.ResponseWriter, mediaType string, code int) (string, int) {
	reasons := map[int]metav1.StatusReason{
		http.StatusBadRequest:           metav1.StatusReasonBadRequest,
		http.StatusNotFound:             metav1.StatusReasonNotFound,
		http.StatusNotAcceptable:        metav1.StatusReasonNotAcceptable,
		http.StatusUnsupportedMediaType: metav1.StatusReasonUnsupportedMediaType,
		http.StatusServiceUnavailable:   metav1.StatusReasonServiceUnavailable,
	}

	return write(w, mediaType, code, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Status",
		"status":     metav1.StatusFailure,
		"message":    http.StatusText(code),
		"reason":     string(reasons[code]),
		"code":       int64(code),
	}})
}

func encode(mediaType string, obj runtime.Object) ([]byte, error) {
	if mediaType == runtime.ContentTypeCBOR {
		return runtime.Encode(objectEncoder, obj)
	}

	return runtime.Encode(unstructured.UnstructuredJSONScheme, obj)
}

// widgetList returns a list whose items, like those of typed lists, have no kind.
func widgetList(widgets int) *unstructured.UnstructuredList {
	list := &unstructured.UnstructuredList{Object: map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "WidgetList",
		"metadata":   map[string]any{"resourceVersion": "1"},
	}}

	for i := range widgets {
		item := widget(i)
		delete(item.Object, "apiVersion")
		delete(item.Object, "kind")

		list.Items = append(list.Items, item)
	}

	return list
}

// widget returns an object with fields of every JSON type, to compare CBOR
// with JSON.
func widget(i int) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "Widget",
		"metadata": map[string]any{
			"name":              "widget-" + strconv.Itoa(i),
			"namespace":         "default",
			"creationTimestamp": "2026-01-02T03:04:05Z",
			"labels":            map[string]any{"app": "test"},
		},
		"spec": map[string]any{
			"replicas": int64(i),
			"ratio":    0.25,
			"enabled":  i%2 == 0,
			"nothing":  nil,
			"tags":     []any{"a", "б", "🙂"},
			"data":     "dmFsdWU=",
			"nested":   map[string]any{"list": []any{map[string]any{"x": int64(-1)}}},
		},
	}}
}

var (
	objectEncoder = apicbor.NewSerializer(unstructuredscheme.NewUnstructuredCreator(), unstructuredscheme.NewUnstructuredObjectTyper())
	eventEncoder  = apicbor.NewSerializer(nil, nil)

	largeBodies     = bodyMode(2097152)
	smallBodiesMode = bodyMode(1024)
)

// bodyMode reads CBOR request bodies into maps with the given limit on the
// number of elements in an array or map.
func bodyMode(limit int) fxcbor.DecMode {
	mode, err := fxcbor.DecOptions{
		MaxArrayElements:      limit,
		MaxMapPairs:           limit,
		IntDec:                fxcbor.IntDecConvertSignedOrFail,
		DefaultMapType:        reflect.TypeFor[map[string]any](),
		DefaultByteStringType: reflect.TypeFor[string](),
		ByteStringToString:    fxcbor.ByteStringToStringAllowedWithExpectedLaterEncoding,
	}.DecMode()
	if err != nil {
		panic(err)
	}

	return mode
}
