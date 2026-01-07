package cmd

import (
	"context"
	"net/http"
	"networkobservability"
)

type networkObservabilityProvider struct{}

func (p networkObservabilityProvider) New(ctx context.Context, c map[string]string) (func(http.Handler) http.Handler, error) {
	return networkobservability.NewNetworkObservabilityMiddleware(ctx, c)
}

// Provider is the exported symbol the host uses to load this plugin.
var Provider = networkObservabilityProvider{}