// Package fdgate is the FD (ONDC:FIS14:FD) edge middleware. It is the outermost middleware
// of an FD module's ONIX handler.
//
// In np mode (a participant under test calls the workbench) it strips internal headers and
// cookies, resolves the REST call (method + path) to an FD route, verifies the FD signature
// (which covers (request-target)), enforces the transport rules, answers replays from its
// replay store, translates the call into the internal POST .../<action> form the steps
// understand, and shapes the inner chain's answer into FD's: 202 + ACK for an accepted
// asynchronous request, 200 + ACK for a callback or webhook, 200 + NACK with an FD Error for
// any rejection.
//
// In mock mode (the playground sends the workbench's messages) it checks the playground's
// internal key, resolves /<action> by action, checks the body's action and direction and
// passes the answer through. In standalone mode it resolves the route only.
//
// See M2-DESIGN.md §3 and §6.
package fdgate

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdroute"
	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdwire"
	"github.com/beckn-one/beckn-onix/pkg/log"
)

// Deps are fd-gate's collaborators. New builds the production ones from the config; tests
// inject fakes through NewGate.
type Deps struct {
	Store    Store            // required in np and mock modes
	Keys     KeyLookup        // required in np mode when signatures are required
	Notifier Notifier         // optional: replay notices are skipped without one
	Auditor  Auditor          // optional: edge events go unrecorded without one
	Now      func() time.Time // optional: defaults to time.Now
}

// Gate is a configured fd-gate.
type Gate struct {
	cfg          *Config
	table        *fdroute.Table
	prefixSegs   []string
	store        Store
	keys         KeyLookup
	notifier     Notifier
	auditor      Auditor
	now          func() time.Time
	replayWait   time.Duration
	defaultParty string // obligedParty when no route resolves
}

// New builds the middleware from the plugin config map. The route table is loaded and
// verified here, so a bad table fails the plugin load.
func New(ctx context.Context, cfg map[string]string) (func(http.Handler) http.Handler, error) {
	c, err := ParseConfig(cfg)
	if err != nil {
		return nil, err
	}
	var d Deps
	if c.Mode != ModeStandalone {
		d.Store = NewRedisStore(c.Addr, c.RedisUsername, c.RedisPassword)
	}
	if c.Mode == ModeNP && c.RecorderTarget != "" {
		d.Auditor = NewGRPCAuditor(c.RecorderTarget)
	}
	if c.Mode == ModeNP && c.Signature == SignatureRequired {
		d.Keys = NewCachedLookup(&RegistryLookup{
			URL: c.RegistryURL, SubscriberID: c.SubscriberID, UniqueKeyID: c.UniqueKeyID,
			SigningPrivateKey: c.SigningPrivateKey,
		}, KeyCacheTTL)
	}
	if c.MockServiceURL != "" {
		d.Notifier = &HTTPNotifier{MockServiceURL: c.MockServiceURL}
	}
	g, err := NewGate(c, d)
	if err != nil {
		return nil, err
	}
	log.Infof(ctx, "fd-gate: loaded %d routes for %s %s (tableHash %s), mode=%s pathPrefix=%q signature=%s requireHTTPS=%t registry=%q mockServiceURL=%q",
		len(g.table.Routes), g.table.Domain, g.table.Version, g.table.TableHash, c.Mode, c.PathPrefix,
		c.Signature, c.RequireHTTPS, c.RegistryURL, c.MockServiceURL)
	if c.Mode == ModeNP && c.Signature == SignatureOff {
		log.Warnf(ctx, "fd-gate: !!! FD SIGNATURE VERIFICATION IS OFF (signature=off / %s=off) for %s: participants are NOT authenticated. Use this for local debugging only. !!!",
			EnvSignature, c.PathPrefix)
	}
	if c.Mode == ModeMock && c.InternalKey == "" {
		log.Warnf(ctx, "fd-gate: %s is not set; the mock module accepts calls without the playground's internal key", EnvInternalKey)
	}
	if c.PassThrough {
		log.Infof(ctx, "fd-gate: passThrough is ignored: M2 always forwards")
	}
	return g.Handler, nil
}

// NewGate loads the route table named by c and wires d.
func NewGate(c *Config, d Deps) (*Gate, error) {
	table, err := fdroute.Load(c.RouteTable)
	if err != nil {
		return nil, fmt.Errorf("fd-gate: %w", err)
	}
	if c.Mode != ModeStandalone && d.Store == nil {
		return nil, fmt.Errorf("fd-gate: %s mode needs a store", c.Mode)
	}
	if c.Mode == ModeNP && c.Signature == SignatureRequired && d.Keys == nil {
		return nil, errors.New("fd-gate: np mode with signature=required needs a key lookup")
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = DefaultMaxBodyBytes
	}
	g := &Gate{
		cfg: c, table: table, store: d.Store, keys: d.Keys, notifier: d.Notifier, auditor: d.Auditor,
		now: d.Now, replayWait: ReplayWait, defaultParty: fdwire.Party(fdroute.RoleInvestorApp),
	}
	if g.now == nil {
		g.now = time.Now
	}
	if c.PathPrefix != "" {
		g.prefixSegs = strings.Split(strings.TrimPrefix(c.PathPrefix, "/"), "/")
		// The investor-app module is called by issuers; every other module by investor apps.
		if g.prefixSegs[len(g.prefixSegs)-1] == "investor-app" {
			g.defaultParty = fdwire.Party(fdroute.RoleDepositIssuer)
		}
	}
	return g, nil
}

// Handler wraps next.
func (g *Gate) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &trackingWriter{ResponseWriter: w}
		method, uri := r.Method, requestURIOf(r) // serve rewrites r
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler {
				panic(p)
			}
			log.Errorf(r.Context(), fmt.Errorf("panic: %v", p), "fd-gate: recovered panic on %s %s", method, uri)
			if !rw.wroteHeader {
				e := reject(CodeInternal, "", "the Protocol Workbench failed while handling this request")
				if g.cfg.Mode == ModeMock {
					g.mockNack(rw, nil, g.defaultParty, e)
				} else {
					writeNack(rw, nil, g.defaultParty, e)
				}
			}
		}()
		switch g.cfg.Mode {
		case ModeNP:
			g.serveNP(rw, r, next)
		case ModeMock:
			g.serveMock(rw, r, next)
		default:
			g.serveStandalone(rw, r, next)
		}
	})
}

// resolve strips the module prefix and matches the route, writing the 404 itself.
func (g *Gate) resolve(w http.ResponseWriter, r *http.Request) (*fdroute.Route, map[string]string, string, bool) {
	path, ok := g.stripPrefix(r.URL.EscapedPath())
	if !ok {
		writeNack(w, nil, g.defaultParty, reject(CodeRouteNotFound, "",
			"%s %s is outside this FD module (%s)", r.Method, r.URL.EscapedPath(), g.cfg.PathPrefix).withStatus(http.StatusNotFound))
		return nil, nil, "", false
	}
	route, params, err := g.table.Match(r.Method, path)
	if err != nil {
		msg := fmt.Sprintf("no FD %s route for %s %s", g.table.Version, r.Method, path)
		var nre *fdroute.NoRouteError
		if errors.As(err, &nre) && len(nre.Allowed) > 0 {
			msg += fmt.Sprintf(" (this path takes %s)", strings.Join(nre.Allowed, ", "))
		}
		writeNack(w, nil, g.defaultParty, reject(CodeRouteNotFound, "", "%s", msg).withStatus(http.StatusNotFound))
		return nil, nil, "", false
	}
	return route, params, path, true
}

func (g *Gate) serveStandalone(w http.ResponseWriter, r *http.Request, next http.Handler) {
	fdroute.StripInternal(r.Header)
	r.Header.Del("Cookie")
	route, params, _, ok := g.resolve(w, r)
	if !ok {
		return
	}
	res := &fdroute.Resolution{
		Route: route, PathParams: params, Query: r.URL.Query(),
		OriginalMethod: r.Method, OriginalRequestURI: requestURIOf(r),
	}
	next.ServeHTTP(w, r.WithContext(fdroute.WithResolution(r.Context(), res)))
}

// serveNP handles a participant's call (§3, np mode).
func (g *Gate) serveNP(w http.ResponseWriter, r *http.Request, next http.Handler) {
	ctx := r.Context()
	requestURI := requestURIOf(r)
	stripInbound(r.Header)

	route, params, path, ok := g.resolve(w, r)
	if !ok {
		return
	}
	obliged := fdwire.Party(route.Caller)
	isGet := r.Method == http.MethodGet

	var body []byte
	if !isGet {
		var gerr *gateError
		if body, gerr = g.readBody(r); gerr != nil {
			writeNack(w, nil, obliged, gerr)
			return
		}
	} else if r.Body != nil {
		r.Body.Close() // a read has no body; its digest covers no bytes
	}
	echo := peekContext(body)
	// What fd-gate answers itself is recorded too (network-observability runs inside it).
	edge := EdgeEvent{Route: route, Method: r.Method, Path: edgePath(path, r), Body: body}

	var signer, ukid string
	if g.cfg.Signature == SignatureRequired {
		auth, gerr := g.verifySignature(ctx, r, requestURI, body)
		if gerr != nil {
			if isGet {
				echo = mustJSON(g.newReadContext(route, "", nil))
				writeNack(w, echo, obliged, gerr)
				return
			}
			g.refuse(w, edge, echo, obliged, gerr)
			return
		}
		signer, ukid = auth.SubscriberID, auth.UniqueKeyID
	}

	var internal []byte
	var fb *fdBody
	if isGet {
		var gerr *gateError
		internal, echo, gerr = g.buildReadBody(ctx, route, params, r.URL.Query(), signer)
		if gerr != nil {
			writeNack(w, echo, obliged, gerr)
			return
		}
	} else {
		var gerr *gateError
		if fb, gerr = parseFDBody(body); gerr != nil {
			g.refuse(w, edge, echo, obliged, gerr)
			return
		}
		if signer != "" {
			if gerr := checkSigner(route, fb, signer); gerr != nil {
				g.refuse(w, edge, echo, obliged, gerr)
				return
			}
		} else {
			signer, _ = senderID(route, fb)
		}
		if gerr := g.checkTransportRules(route, params, fb); gerr != nil {
			g.refuse(w, edge, echo, obliged, gerr)
			return
		}
		internal = body
	}

	var claim *replayClaim
	if !isGet {
		key := ReplayKey(g.table.Domain, signer, route.Action, fb.str("message_id"))
		outcome, rec, c, err := g.replayCheck(ctx, key, body)
		if err != nil {
			log.Errorf(ctx, err, "fd-gate: replay check for %s failed", key)
			writeNack(w, echo, obliged, reject(CodeInternal, "", "the workbench cannot check this message for replay: %v", err))
			return
		}
		switch outcome {
		case replayConflict:
			g.refuse(w, edge, echo, obliged, reject(CodeMessageIDReused, "/context/message_id",
				"message_id %s was already used for a different %s from %s; a retry must resend the identical body",
				fb.str("message_id"), route.Action, signer))
			return
		case replayStored, replayAwaited:
			g.writeRecord(w, rec)
			edge.Kind, edge.Status, edge.Response = EdgeReplay, rec.Status, []byte(rec.Body)
			g.audit(ctx, edge)
			if outcome == replayStored && isAsyncRequest(route) && rec.Status == http.StatusAccepted {
				g.notifyReplay(ctx, ReplayNotice{
					Domain: g.table.Domain, Version: g.table.Version, Action: route.Action,
					MessageID: fb.str("message_id"), TransactionID: fb.str("transaction_id"),
					SubscriberURL: senderURI(route, fb),
				})
			}
			return
		}
		claim = c
		defer claim.release(ctx) // a no-op once finished; frees the key on a panic
		g.indexBody(ctx, fb)
	}

	fdPath := path
	if r.URL.RawQuery != "" {
		fdPath += "?" + r.URL.RawQuery
	}
	res := &fdroute.Resolution{
		Route: route, PathParams: params, Query: r.URL.Query(),
		OriginalMethod: r.Method, OriginalRequestURI: requestURI,
		CallerSubscriberID: signer, CallerUniqueKeyID: ukid,
	}
	r.Header.Set(fdroute.HeaderFdMethod, r.Method)
	r.Header.Set(fdroute.HeaderFdPath, fdPath)
	r.Header.Set(fdroute.HeaderFdOrigin, fdroute.OriginNP)
	inner := g.translate(r, route, params, internal, res)

	bw := newBufferWriter()
	next.ServeHTTP(bw, inner)

	resp := g.shapeNP(route, echo, bw, obliged)
	stripForParticipant(resp.header)
	if resp.status < 300 {
		g.indexResponse(ctx, resp)
	}
	claim.finish(ctx, resp.status, resp.contentType(), resp.body, resp.storable)
	writeBody(w, resp.status, resp.header, resp.body)
}

// translate turns r into the internal POST <prefix>/<action> with body, carrying the FD
// headers and the Resolution. The headers are set on the request before the inner chain
// runs, so network-observability records them.
func (g *Gate) translate(r *http.Request, route *fdroute.Route, params map[string]string, body []byte, res *fdroute.Resolution) *http.Request {
	r.Method = http.MethodPost
	r.URL.Path = g.cfg.PathPrefix + "/" + route.Action
	r.URL.RawPath = ""
	r.URL.RawQuery = ""
	r.RequestURI = r.URL.RequestURI()
	setBody(r, body)
	r.Header.Set(fdroute.HeaderFdRoute, route.Action)
	r.Header.Set(fdroute.HeaderFdPathParams, string(mustJSON(nonNil(params))))
	return r.WithContext(fdroute.WithResolution(r.Context(), res))
}

// serveMock handles the playground's call (§3, mock mode).
func (g *Gate) serveMock(w http.ResponseWriter, r *http.Request, next http.Handler) {
	ctx := r.Context()
	requestURI := requestURIOf(r)
	key := r.Header.Get(fdroute.HeaderInternalKey)
	rawParams := strings.TrimSpace(r.Header.Get(fdroute.HeaderFdPathParams))
	replay := strings.TrimSpace(r.Header.Get(fdroute.HeaderFdReplay)) == "1"
	stripInbound(r.Header)

	if g.cfg.InternalKey != "" && subtle.ConstantTimeCompare([]byte(key), []byte(g.cfg.InternalKey)) != 1 {
		g.mockNack(w, nil, g.defaultParty, reject(CodeInternalKeyInvalid, "",
			"the mock module takes calls from the playground only: %s is missing or wrong", fdroute.HeaderInternalKey).withStatus(http.StatusForbidden))
		return
	}
	path, ok := g.stripPrefix(r.URL.EscapedPath())
	action := strings.TrimPrefix(path, "/")
	var route *fdroute.Route
	if ok && action != "" && !strings.Contains(action, "/") {
		route, ok = g.table.ByAction(action)
	} else {
		ok = false
	}
	if !ok {
		g.mockNack(w, nil, g.defaultParty, reject(CodeRouteNotFound, "",
			"no FD %s action for %s %s", g.table.Version, r.Method, r.URL.EscapedPath()).withStatus(http.StatusNotFound))
		return
	}
	obliged := fdwire.Party(route.Caller)
	if r.Method != http.MethodPost {
		g.mockNack(w, nil, obliged, reject(CodeRouteNotFound, "",
			"the mock module takes POST /%s, not %s", route.Action, r.Method).withStatus(http.StatusNotFound))
		return
	}
	body, gerr := g.readBody(r)
	if gerr != nil {
		g.mockNack(w, nil, obliged, gerr)
		return
	}
	echo := peekContext(body)
	bad := func(e *gateError) { g.mockNack(w, echo, obliged, e.withStatus(http.StatusBadRequest)) }

	fb, gerr := parseFDBody(body)
	if gerr != nil {
		bad(gerr)
		return
	}
	if a := fb.str("action"); a != route.Action {
		bad(reject(CodeActionPathMismatch, "/context/action", "context.action is %q but the playground called /%s", a, route.Action))
		return
	}
	if role, _ := fb.envelope["originatorRole"].(string); role != route.Caller {
		bad(reject(CodeWrongDirection, "/fdEnvelope/originatorRole", "fdEnvelope.originatorRole is %q but %s is sent by %s", role, route.Action, route.Caller))
		return
	}
	params := map[string]string{}
	if rawParams != "" {
		if err := json.Unmarshal([]byte(rawParams), &params); err != nil || params == nil {
			bad(reject(CodePathParamsInvalid, "", "%s is not a JSON object of strings: %s", fdroute.HeaderFdPathParams, rawParams))
			return
		}
	}
	restPath, err := route.RenderPath(params)
	if err != nil {
		bad(reject(CodePathParamsInvalid, "", "%s: %v", fdroute.HeaderFdPathParams, err))
		return
	}
	g.indexBody(ctx, fb)

	r.Header.Set(fdroute.HeaderFdRoute, route.Action)
	r.Header.Set(fdroute.HeaderFdPathParams, string(mustJSON(params)))
	r.Header.Set(fdroute.HeaderFdMethod, route.Method)
	r.Header.Set(fdroute.HeaderFdPath, restPath)
	r.Header.Set(fdroute.HeaderFdOrigin, fdroute.OriginWorkbench)
	if replay {
		r.Header.Set(fdroute.HeaderFdReplay, "1")
	}
	setBody(r, body)
	res := &fdroute.Resolution{
		Route: route, PathParams: params, OriginalMethod: r.Method, OriginalRequestURI: requestURI, Replay: replay,
	}

	bw := newBufferWriter()
	next.ServeHTTP(bw, r.WithContext(fdroute.WithResolution(ctx, res)))
	status := bw.status
	if status == 0 {
		status = http.StatusOK
	}
	resp := &response{status: status, header: cleanHeader(bw.header, false), body: bw.body.Bytes()}
	if status < 300 {
		g.indexResponse(ctx, resp)
	}
	writeBody(w, resp.status, resp.header, resp.body)
}

// mockNack writes fd-gate's own refusal of a playground call, naming its code in
// X-Pwb-Fd-Gate-Error so the playground can tell it from a participant's answer. Mock-mode
// responses go back to the playground only, never to a participant.
func (g *Gate) mockNack(w http.ResponseWriter, ctx json.RawMessage, obliged string, e *gateError) {
	w.Header().Set(fdroute.HeaderFdGateError, e.code)
	writeNack(w, ctx, obliged, e)
}

// indexResponse indexes the correlation ids of an FD message in a response body (a SYNC
// answer), so later reads of the objects it names resolve.
func (g *Gate) indexResponse(ctx context.Context, resp *response) {
	if encoded(resp.header) {
		return
	}
	if fb, err := parseFDBody(resp.body); err == nil {
		g.indexBody(ctx, fb)
	}
}

// writeRecord serves a stored response.
func (g *Gate) writeRecord(w http.ResponseWriter, rec *replayRecord) {
	h := http.Header{}
	for k, v := range rec.Headers {
		h.Set(k, v)
	}
	writeBody(w, rec.Status, h, []byte(rec.Body))
}

// edgePath is the REST path as received, without the module prefix, with any query: the
// value X-Pwb-Fd-Path carries.
func edgePath(path string, r *http.Request) string {
	if r.URL.RawQuery != "" {
		return path + "?" + r.URL.RawQuery
	}
	return path
}

// readBody reads a POST body up to maxBodyBytes.
func (g *Gate) readBody(r *http.Request) ([]byte, *gateError) {
	tooLarge := reject(CodeBodyTooLarge, "", "the body exceeds %d bytes", g.cfg.MaxBodyBytes).withStatus(http.StatusRequestEntityTooLarge)
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	if r.ContentLength > g.cfg.MaxBodyBytes {
		return nil, tooLarge
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, g.cfg.MaxBodyBytes+1))
	if err != nil {
		return nil, reject(CodeBodyMalformed, "", "the body cannot be read: %v", err)
	}
	if int64(len(data)) > g.cfg.MaxBodyBytes {
		return nil, tooLarge
	}
	return data, nil
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

// stripInbound removes what a caller must not be able to set: internal headers, cookies
// (which drive workbench behaviour such as custom-response-body) and Accept-Encoding (so the
// inner response arrives decoded and fd-gate can read it).
func stripInbound(h http.Header) {
	fdroute.StripInternal(h)
	h.Del("Cookie")
	h.Del("Accept-Encoding")
}

func setBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	r.Header.Set("Content-Type", "application/json")
}

func requestURIOf(r *http.Request) string {
	if r.RequestURI != "" {
		return r.RequestURI
	}
	return r.URL.RequestURI()
}

func isAsyncRequest(route *fdroute.Route) bool {
	return route.Kind == fdroute.KindRequest && route.Mode == fdroute.ModeAsyncAckCallback
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
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

// Unwrap lets http.ResponseController reach the underlying writer's Flush/Hijack.
func (w *trackingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
