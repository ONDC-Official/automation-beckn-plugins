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
	// Replay is true when the playground re-sends a stored message (X-Pwb-Fd-Replay: 1).
	Replay bool
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

	HeaderFdRoute      = "X-Pwb-Fd-Route"       // the resolved action
	HeaderFdPathParams = "X-Pwb-Fd-Path-Params" // JSON object of path parameters
	HeaderFdMethod     = "X-Pwb-Fd-Method"      // the REST method as sent
	HeaderFdPath       = "X-Pwb-Fd-Path"        // the REST path (and query) as sent, without the module prefix
	HeaderFdReplay     = "X-Pwb-Fd-Replay"      // "1" when the playground re-sends a stored message
	HeaderFdOrigin     = "X-Pwb-Fd-Origin"      // who sent the message: OriginNP or OriginWorkbench
	HeaderFdScenario   = "X-Pwb-Fd-Scenario"    // the playground's scenario directives for a deliberate deviation (JSON), kept for the recorder
	HeaderFdGateError  = "X-Pwb-Fd-Gate-Error"  // on a response: the FD-PWB code of the workbench's own refusal (internal hops only)
	HeaderFdEnvelope   = "X-Pwb-Fd-Envelope"
	HeaderUpstreamMs   = "X-Pwb-Upstream-Ms" // participant response time in ms, set by fd-transport
	HeaderInternalKey  = "X-Pwb-Internal-Key"
)

// Values of HeaderFdOrigin, which fd-gate sets so that the recorder (and the report judging a run
// from what it recorded) knows who sent each message.
const (
	OriginNP        = "np"        // the participant under test called the workbench (np mode)
	OriginWorkbench = "workbench" // the workbench's mock sent it through mock/ (mock mode)
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
