package api

import (
	"context"
	"errors"
)

var errRewriterNotConfigured = errors.New(
	"rewriter client is not configured",
)

// unavailableRewriterClient keeps the in-memory constructor convenient for
// tests that do not exercise rewriting. The capability fails closed with 503;
// production always injects the configured HTTP client.
type unavailableRewriterClient struct{}

func (unavailableRewriterClient) Models(
	context.Context,
) (RewriteModelsResponse, error) {
	return RewriteModelsResponse{}, errRewriterNotConfigured
}

func (unavailableRewriterClient) Rewrite(
	context.Context,
	RewriterRequest,
) (RewriterResult, error) {
	return RewriterResult{}, errRewriterNotConfigured
}

var _ RewriterClient = unavailableRewriterClient{}
