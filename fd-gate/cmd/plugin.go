package main

import (
	"context"
	"errors"
	"net/http"

	fdgate "github.com/ONDC-Official/automation-beckn-plugins/fd-gate"
)

// middlewareProvider implements beckn-onix's definition.MiddlewareProvider.
type middlewareProvider struct{}

// New returns the fd-gate middleware. Config keys (all strings): routeTable and mode
// (np|mock|standalone) are required; addr (Redis) is required in np and mock modes;
// pathPrefix, mockServiceURL, registryURL, signature (required|off), requireHTTPS,
// clockSkew, replayTTL, objectTTL and maxBodyBytes are optional; passThrough is accepted
// and ignored. Env FD_REQUIRE_HTTPS, FD_REGISTRY_URL, FD_SIGNATURE and MOCK_SERVER_URL
// override the config when set. See fdgate.Config.
func (p middlewareProvider) New(ctx context.Context, c map[string]string) (func(http.Handler) http.Handler, error) {
	if ctx == nil {
		return nil, errors.New("context cannot be nil")
	}
	return fdgate.New(ctx, c)
}

// Provider is the exported symbol the beckn-onix plugin manager looks for.
var Provider = middlewareProvider{}
