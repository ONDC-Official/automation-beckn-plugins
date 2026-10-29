package main

import (
	"context"
	"errors"
	"net/http"

	fdgate "github.com/ONDC-Official/automation-beckn-plugins/fd-gate"
)

// middlewareProvider implements beckn-onix's definition.MiddlewareProvider.
type middlewareProvider struct{}

// New returns the fd-gate middleware. Config keys: routeTable (required), mode
// (np|mock|standalone, required), passThrough ("true" to call the next handler after
// resolving; default false), pathPrefix (the module's URL prefix).
func (p middlewareProvider) New(ctx context.Context, c map[string]string) (func(http.Handler) http.Handler, error) {
	if ctx == nil {
		return nil, errors.New("context cannot be nil")
	}
	return fdgate.New(ctx, c)
}

// Provider is the exported symbol the beckn-onix plugin manager looks for.
var Provider = middlewareProvider{}
