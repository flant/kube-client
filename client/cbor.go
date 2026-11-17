package client

import (
	"context"
	"log/slog"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	internalcbor "github.com/flant/kube-client/internal/cbor"
)

// WithCBOR turns CBOR for the dynamic client off or back on. It is on by
// default.
//
// With CBOR on, Init asks the API server whether it supports CBOR by creating
// up to two SelfSubjectReviews (allowed to every authenticated user, nothing
// is stored). If it does, the dynamic client receives CBOR and, from API
// server v1.36, also sends create and update bodies in CBOR. If the API server
// answers that it does not, the dynamic client uses JSON. Typed clients keep
// using protobuf.
//
// If the API server gives no answer at Init (it is unavailable, fails or does
// not answer within 5 seconds), the dynamic client starts with JSON and asks
// again until the API server answers, at intervals growing from 10 seconds to
// 5 minutes. It asks while it is used: an idle client asks nothing. Once the
// answer is yes, the same dynamic client switches to CBOR.
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

	ctx, cancel := context.WithTimeout(context.Background(), internalcbor.ProbeTimeout)
	defer cancel()

	support, err := cborClient.Probe(ctx)
	if err != nil {
		c.logger.Warn("could not check whether the API server supports CBOR, the dynamic client uses JSON until the API server answers",
			slog.String("error", err.Error()))
		cborClient.ProbeLater(c.logCBORSupport)

		return cborClient.DynamicClient, nil
	}

	c.logCBORSupport(support, nil)

	if support == internalcbor.None {
		return dynamic.NewForConfig(config)
	}

	return cborClient.DynamicClient, nil
}

// logCBORSupport logs what the API server answered about CBOR.
func (c *Client) logCBORSupport(support internalcbor.Support, err error) {
	switch {
	case err != nil:
		c.logger.Debug("could not check again whether the API server supports CBOR", slog.String("error", err.Error()))
	case support == internalcbor.ResponsesAndRequests:
		c.logger.Info("the dynamic client uses CBOR")
	case support == internalcbor.Responses:
		c.logger.Info("the dynamic client receives CBOR and sends JSON: the API server rejects large CBOR bodies, as servers before v1.36 do")
	default:
		c.logger.Debug("the API server does not support CBOR, the dynamic client uses JSON")
	}
}
