// Package fdgate is the FD (ONDC:FIS14:FD) edge middleware. It sits in front of an FD
// module's ONIX handler, resolves the REST call (method + path) to an FD route, and — in
// later milestones — verifies the FD signature, translates the call into the internal
// POST .../<action> form the existing steps understand and rewrites ACKs.
//
// M1 skeleton: it strips internal and cookie headers, resolves the route and either passes
// the request on with the resolution on its context (passThrough) or answers 501.
package fdgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdroute"
	"github.com/beckn-one/beckn-onix/pkg/log"
)

// Mode is the workbench side fd-gate serves.
type Mode string

// Modes.
const (
	ModeNP         Mode = "np"         // in front of a participant under test
	ModeMock       Mode = "mock"       // in front of the workbench's mock participant
	ModeStandalone Mode = "standalone" // no participant behind it
)

// Error codes fd-gate answers with.
const (
	CodeRouteNotFound  = "FD-PWB-ROUTE-NOT-FOUND"
	CodeNotImplemented = "FD-PWB-NOT-IMPLEMENTED"
	CodeInternal       = "FD-PWB-INTERNAL"
)

// Config is fd-gate's configuration.
type Config struct {
	RouteTable  string // path to fd_routes.yaml (config key routeTable)
	Mode        Mode   // config key mode
	PassThrough bool   // config key passThrough: call next after resolving
	PathPrefix  string // config key pathPrefix: the module's URL prefix, removed before matching
}

// ParseConfig reads fd-gate's plugin config map.
func ParseConfig(cfg map[string]string) (*Config, error) {
	c := &Config{RouteTable: strings.TrimSpace(cfg["routeTable"])}
	if c.RouteTable == "" {
		return nil, errors.New("fd-gate: routeTable is required")
	}
	switch m := Mode(strings.TrimSpace(cfg["mode"])); m {
	case ModeNP, ModeMock, ModeStandalone:
		c.Mode = m
	case "":
		return nil, errors.New("fd-gate: mode is required (np|mock|standalone)")
	default:
		return nil, fmt.Errorf("fd-gate: invalid mode %q (np|mock|standalone)", m)
	}
	if raw := strings.TrimSpace(cfg["passThrough"]); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("fd-gate: invalid passThrough %q", raw)
		}
		c.PassThrough = v
	}
	c.PathPrefix = strings.TrimRight(strings.TrimSpace(cfg["pathPrefix"]), "/")
	if c.PathPrefix != "" && !strings.HasPrefix(c.PathPrefix, "/") {
		return nil, fmt.Errorf("fd-gate: pathPrefix %q must start with /", c.PathPrefix)
	}
	return c, nil
}

// Gate is a configured fd-gate.
type Gate struct {
	cfg        *Config
	table      *fdroute.Table
	prefixSegs []string
}

// New builds the middleware from the plugin config map. The route table is loaded and
// verified here, so a bad table fails the plugin load.
func New(ctx context.Context, cfg map[string]string) (func(http.Handler) http.Handler, error) {
	c, err := ParseConfig(cfg)
	if err != nil {
		return nil, err
	}
	g, err := NewGate(c)
	if err != nil {
		return nil, err
	}
	log.Infof(ctx, "fd-gate: loaded %d routes for %s %s (tableHash %s), mode=%s passThrough=%t pathPrefix=%q",
		len(g.table.Routes), g.table.Domain, g.table.Version, g.table.TableHash, c.Mode, c.PassThrough, c.PathPrefix)
	return g.Handler, nil
}

// NewGate loads the route table named by c.
func NewGate(c *Config) (*Gate, error) {
	table, err := fdroute.Load(c.RouteTable)
	if err != nil {
		return nil, fmt.Errorf("fd-gate: %w", err)
	}
	g := &Gate{cfg: c, table: table}
	if c.PathPrefix != "" {
		g.prefixSegs = strings.Split(strings.TrimPrefix(c.PathPrefix, "/"), "/")
	}
	return g, nil
}

// Handler wraps next.
func (g *Gate) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &trackingWriter{ResponseWriter: w}
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler {
				panic(p)
			}
			log.Errorf(r.Context(), fmt.Errorf("panic: %v", p), "fd-gate: recovered panic on %s %s", r.Method, r.URL.RequestURI())
			if !rw.wroteHeader {
				writeError(rw, http.StatusInternalServerError, CodeInternal, "TECHNICAL", "PWB",
					"the Protocol Workbench failed while handling this request")
			}
		}()
		g.serve(rw, r, next)
	})
}

func (g *Gate) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	fdroute.StripInternal(r.Header)
	r.Header.Del("Cookie")

	path, ok := g.stripPrefix(r.URL.EscapedPath())
	if !ok {
		writeError(w, http.StatusNotFound, CodeRouteNotFound, "PROTOCOL", "N-01",
			fmt.Sprintf("%s %s is outside this FD module (%s)", r.Method, r.URL.EscapedPath(), g.cfg.PathPrefix))
		return
	}
	route, params, err := g.table.Match(r.Method, path)
	if err != nil {
		msg := fmt.Sprintf("no FD %s route for %s %s", g.table.Version, r.Method, path)
		var nre *fdroute.NoRouteError
		if errors.As(err, &nre) && len(nre.Allowed) > 0 {
			msg += fmt.Sprintf(" (this path takes %s)", strings.Join(nre.Allowed, ", "))
		}
		writeError(w, http.StatusNotFound, CodeRouteNotFound, "PROTOCOL", "N-01", msg)
		return
	}

	if !g.cfg.PassThrough {
		writeError(w, http.StatusNotImplemented, CodeNotImplemented, "PROTOCOL", "N-01",
			fmt.Sprintf("%s %s resolves to %s, which this workbench build does not handle yet", r.Method, path, route.Action))
		return
	}

	requestURI := r.RequestURI
	if requestURI == "" {
		requestURI = r.URL.RequestURI()
	}
	res := &fdroute.Resolution{
		Route:              route,
		PathParams:         params,
		Query:              r.URL.Query(),
		OriginalMethod:     r.Method,
		OriginalRequestURI: requestURI,
	}
	next.ServeHTTP(w, r.WithContext(fdroute.WithResolution(r.Context(), res)))
}

// stripPrefix removes the configured module prefix from an escaped path, comparing decoded
// segments (so "ONDC%3AFIS14%3AFD" matches "ONDC:FIS14:FD"). It returns the remainder,
// still escaped, starting with "/".
func (g *Gate) stripPrefix(escaped string) (string, bool) {
	if len(g.prefixSegs) == 0 {
		return escaped, true
	}
	segs := strings.Split(strings.TrimPrefix(escaped, "/"), "/")
	if len(segs) < len(g.prefixSegs) {
		return "", false
	}
	for i, want := range g.prefixSegs {
		got, err := url.PathUnescape(segs[i])
		if err != nil || got != want {
			return "", false
		}
	}
	return "/" + strings.Join(segs[len(g.prefixSegs):], "/"), true
}

// ErrorBody is fd-gate's error response: an ONDC NACK carrying an FD Error.
type ErrorBody struct {
	Message struct {
		Ack struct {
			Status string `json:"status"`
		} `json:"ack"`
	} `json:"message"`
	Error FDError `json:"error"`
}

// FDError is the FD protocol's error object (the fields fd-gate fills).
type FDError struct {
	Code         string `json:"code"`
	Class        string `json:"class"`
	Message      string `json:"message"`
	ObligedParty string `json:"obligedParty"`
	Retryable    bool   `json:"retryable"`
}

func writeError(w http.ResponseWriter, status int, code, class, obliged, message string) {
	var body ErrorBody
	body.Message.Ack.Status = "NACK"
	body.Error = FDError{Code: code, Class: class, Message: message, ObligedParty: obliged}
	data, err := json.Marshal(body)
	if err != nil { // cannot happen for this struct
		http.Error(w, message, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// trackingWriter records whether a response has started, so a recovered panic does not
// write a second status line.
type trackingWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *trackingWriter) WriteHeader(code int) {
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *trackingWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController (used by httputil.ReverseProxy) reach the underlying
// writer's Flush/Hijack.
func (w *trackingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
