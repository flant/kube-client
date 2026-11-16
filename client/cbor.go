package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/version"
	apimachineryversion "k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/features"
	"k8s.io/client-go/rest"
)

const (
	// minCBORVersion is the first Kubernetes release (k8s.io/apimachinery
	// v0.36.0, kube-apiserver v1.36.0) whose CBOR decoder accepts arrays and
	// maps longer than 1024 elements. An older client fails to read a list of
	// more than 1024 objects; an older API server rejects such a request body.
	minCBORVersion = "0.36.0"

	// cborProbeTimeout bounds the two requests WithCBORRequests adds to Init.
	cborProbeTimeout = 10 * time.Second
)

var selfSubjectReviewGVR = schema.GroupVersionResource{
	Group: "authentication.k8s.io", Version: "v1", Resource: "selfsubjectreviews",
}

// WithCBOR lets the dynamic client receive CBOR instead of JSON from API
// servers that support it. It is off by default.
//
// No probe request is made: the client adds CBOR to the Accept header and the
// API server picks the format for every response, so a server without CBOR
// keeps answering in JSON. Request bodies stay in JSON unless
// WithCBORRequests is set. Typed clients for built-in resources keep using
// protobuf.
//
// The switch is the client-go feature gate ClientsAllowCBOR, which is
// process-wide: it affects every client-go client created after Init. It is
// left untouched, with a warning, when:
//   - WithAcceptOnlyJSONContentType is set;
//   - the binary is built with k8s.io/apimachinery older than v0.36.0;
//   - the application replaced the client-go feature gates.
//
// An explicit KUBE_FEATURE_ClientsAllowCBOR environment variable wins over
// this option.
func (c *Client) WithCBOR(enabled bool) {
	c.cbor = enabled
}

// WithCBORRequests additionally sends request bodies of the dynamic client
// (create, update, patch, apply) in CBOR. It needs WithCBOR and is off by
// default.
//
// Unlike responses, a request body cannot be negotiated, so Init asks the API
// server once: it reads the server version and creates a SelfSubjectReview
// with a CBOR body (allowed to every authenticated user, nothing is stored).
// Bodies stay in JSON, with a warning, when the server is older than v1.36,
// does not accept CBOR, or does not answer within 10 seconds.
//
// The switch is the process-wide client-go feature gate ClientsPreferCBOR. An
// explicit KUBE_FEATURE_ClientsPreferCBOR environment variable wins over this
// option.
//
// ponytail: the server is asked once, at Init. If CBOR is later turned off on
// the server, client-go itself falls back to JSON after the first request
// rejected with 415, and that one request fails. A control plane that mixes
// v1.36 with older API servers that have CBOR enabled is not detected; turn
// the option on after the upgrade is finished.
func (c *Client) WithCBORRequests(enabled bool) {
	c.cborRequests = enabled
}

type featureGateSetter interface {
	Set(feature features.Feature, value bool) error
}

func featureEnvSet(feature features.Feature) bool {
	_, ok := os.LookupEnv("KUBE_FEATURE_" + string(feature))
	return ok
}

// enableCBOR turns ClientsAllowCBOR on and reports whether it did. It must run
// before the clients are created: they read the gate once.
func (c *Client) enableCBOR() bool {
	if featureEnvSet(features.ClientsAllowCBOR) {
		return false
	}

	if c.acceptOnlyJSONContentType {
		c.logger.Warn("CBOR is not enabled: the client is set to accept only JSON")
		return false
	}

	if v := apimachineryVersion(); !cborSupportedBy(v) {
		c.logger.Warn("CBOR is not enabled: k8s.io/apimachinery is too old",
			slog.String("version", v), slog.String("required", minCBORVersion))

		return false
	}

	gates, ok := features.FeatureGates().(featureGateSetter)
	if !ok {
		c.logger.Warn("CBOR is not enabled: client-go feature gates are replaced by the application, set ClientsAllowCBOR there")
		return false
	}

	if err := gates.Set(features.ClientsAllowCBOR, true); err != nil {
		c.logger.Warn("CBOR is not enabled", slog.String("error", err.Error()))
		return false
	}

	return true
}

// newDynamicClient creates the dynamic client. When CBOR request bodies are
// asked for and the API server accepts them, the client and the rest of the
// process send CBOR; otherwise everything stays as without the option.
func (c *Client) newDynamicClient(config *rest.Config, cborAllowed bool) (dynamic.Interface, error) {
	if !cborAllowed || !c.cborRequests || featureEnvSet(features.ClientsPreferCBOR) {
		return dynamic.NewForConfig(config)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cborProbeTimeout)
	defer cancel()

	if err := c.checkServerDecodesLargeCBOR(ctx); err != nil {
		c.logger.Warn("CBOR request bodies are not enabled", slog.String("error", err.Error()))
		return dynamic.NewForConfig(config)
	}

	// enableCBOR has already checked that the gates can be set.
	gates := features.FeatureGates().(featureGateSetter)

	// The dynamic client reads the gate when it is created, so the gate goes
	// first and is taken back if the probe fails.
	if err := gates.Set(features.ClientsPreferCBOR, true); err != nil {
		c.logger.Warn("CBOR request bodies are not enabled", slog.String("error", err.Error()))
		return dynamic.NewForConfig(config)
	}

	client, err := dynamic.NewForConfig(config)
	if err == nil {
		review := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": selfSubjectReviewGVR.GroupVersion().String(),
			"kind":       "SelfSubjectReview",
		}}

		_, err = client.Resource(selfSubjectReviewGVR).Create(ctx, review, metav1.CreateOptions{})
		if err == nil {
			return client, nil
		}
	}

	c.logger.Warn("CBOR request bodies are not enabled: the API server did not accept a CBOR request",
		slog.String("error", err.Error()))

	if err := gates.Set(features.ClientsPreferCBOR, false); err != nil {
		return nil, err
	}

	return dynamic.NewForConfig(config)
}

// checkServerDecodesLargeCBOR returns an error unless the API server is new
// enough to decode CBOR bodies without the 1024-element limit.
func (c *Client) checkServerDecodesLargeCBOR(ctx context.Context) error {
	body, err := c.Discovery().RESTClient().Get().AbsPath("/version").Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("get API server version: %w", err)
	}

	var info apimachineryversion.Info
	if err := json.Unmarshal(body, &info); err != nil {
		return fmt.Errorf("parse API server version: %w", err)
	}

	// kube-apiserver v1.N ships the decoder of k8s.io/apimachinery v0.N.
	if !cborSupportedBy("0." + info.Minor) {
		return fmt.Errorf("API server %s is older than v1.36", info.GitVersion)
	}

	return nil
}

// apimachineryVersion returns the k8s.io/apimachinery version the binary is
// built with, or an empty string when it is unknown (for example in tests).
func apimachineryVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}

	for _, dep := range info.Deps {
		if dep.Path != "k8s.io/apimachinery" {
			continue
		}

		if dep.Replace != nil {
			return dep.Replace.Version
		}

		return dep.Version
	}

	return ""
}

// cborSupportedBy reports whether the given k8s.io/apimachinery version can
// decode CBOR without the 1024-element limit. An unknown version is not trusted.
func cborSupportedBy(apimachinery string) bool {
	v, err := version.ParseGeneric(apimachinery)
	if err != nil {
		return false
	}

	return v.AtLeast(version.MustParseGeneric(minCBORVersion))
}
