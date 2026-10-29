package fdroute

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// Resolution is what fd-gate learned about an inbound FD request, carried on the request
// context to the steps and plugins after it.
type Resolution struct {
	Route              *Route
	PathParams         map[string]string
	Query              url.Values
	OriginalMethod     string
	OriginalRequestURI string
	CallerSubscriberID string
	CallerUniqueKeyID  string
}

type resolutionKey struct{}

// WithResolution returns a copy of ctx carrying res.
func WithResolution(ctx context.Context, res *Resolution) context.Context {
	return context.WithValue(ctx, resolutionKey{}, res)
}

// FromContext returns the Resolution on ctx, or nil.
func FromContext(ctx context.Context) *Resolution {
	if ctx == nil {
		return nil
	}
	res, _ := ctx.Value(resolutionKey{}).(*Resolution)
	return res
}

// Internal headers. Every workbench-internal header starts with InternalHeaderPrefix; they
// are stripped from inbound requests at the edge (fd-gate) and from requests leaving for
// participants (fd-transport).
const (
	InternalHeaderPrefix = "X-Pwb-"

	HeaderFdRoute      = "X-Pwb-Fd-Route"
	HeaderFdPathParams = "X-Pwb-Fd-Path-Params"
	HeaderFdEnvelope   = "X-Pwb-Fd-Envelope"
	HeaderUpstreamMs   = "X-Pwb-Upstream-Ms"
	HeaderInternalKey  = "X-Pwb-Internal-Key"
)

// IsInternalHeader reports whether name (any case) is a workbench-internal header.
func IsInternalHeader(name string) bool {
	return len(name) >= len(InternalHeaderPrefix) && strings.EqualFold(name[:len(InternalHeaderPrefix)], InternalHeaderPrefix)
}

// StripInternal removes every X-Pwb-* header from h, whatever the key's case.
func StripInternal(h http.Header) {
	for name := range h {
		if IsInternalHeader(name) {
			delete(h, name)
		}
	}
}
