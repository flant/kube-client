package client

import (
	"context"
	"log/slog"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	internalcbor "github.com/flant/kube-client/internal/cbor"
)

// cborProbeTimeout bounds the requests Init makes to find out whether the API
// server supports CBOR.
const cborProbeTimeout = 5 * time.Second

// WithCBOR turns CBOR for the dynamic client off or back on. It is on by
// default.
//
// With CBOR on, Init asks the API server whether it supports CBOR by creating
// up to two SelfSubjectReviews (allowed to every authenticated user, nothing
// is stored). If it does, the dynamic client receives CBOR and, from API
// server v1.36, also sends create and update bodies in CBOR. Otherwise, or if
// the API server does not answer within 5 seconds, the dynamic client uses
// JSON. Typed clients keep using protobuf.
//
// If the API server drops CBOR later, it answers in JSON, and a CBOR body it
// rejects (415, or 400 that JSON does not get) is sent again in JSON, as are
// all bodies after it.
//
// CBOR works with client-go v0.32 and later and does not use the process-wide
// CBOR feature gates of client-go, so other clients in the process are not
// affected. WithAcceptOnlyJSONContentType turns CBOR off as well.
func (c *Client) WithCBOR(enabled bool) {
	c.cborDisabled = !enabled
}

// newDynamicClient creates the dynamic client: a CBOR one if the API server
// supports CBOR, a JSON one otherwise.
func (c *Client) newDynamicClient(config *rest.Config) (dynamic.Interface, error) {
	if c.cborDisabled || c.acceptOnlyJSONContentType {
		return dynamic.NewForConfig(config)
	}

	cborClient, err := internalcbor.NewClient(config)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), cborProbeTimeout)
	defer cancel()

	support, err := cborClient.Probe(ctx)

	switch {
	case support == internalcbor.ResponsesAndRequests:
		c.logger.Info("the dynamic client uses CBOR")

		return cborClient.DynamicClient, nil
	case support == internalcbor.Responses:
		c.logger.Info("the dynamic client receives CBOR and sends JSON: the API server rejects large CBOR bodies, as servers before v1.36 do")

		return cborClient.DynamicClient, nil
	case err != nil:
		c.logger.Warn("could not check whether the API server supports CBOR, the dynamic client uses JSON",
			slog.String("error", err.Error()))
	default:
		c.logger.Debug("the API server does not support CBOR, the dynamic client uses JSON")
	}

	return dynamic.NewForConfig(config)
}
