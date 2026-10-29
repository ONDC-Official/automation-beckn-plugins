// Package fdtransport is the FD (ONDC:FIS14:FD) outbound transport wrapper. It wraps the
// ONIX handler's http.RoundTripper and owns the last hop to a participant (M2-DESIGN §4):
// it rebuilds the REST call from the internal POST .../<action> form using the route table
// (X-Pwb-Fd-Route, X-Pwb-Fd-Path-Params), checks the destination host, strips cookies and
// internal headers, signs the call with the FD signature (which covers (request-target))
// and times the participant's answer (X-Pwb-Upstream-Ms on the response).
//
// Requests to internal hosts (such as the mock service) pass unchanged. A request for a
// participant that cannot be rendered, sent to its host or signed is never sent: the caller
// gets a NACK response whose X-Pwb-Fd-Gate-Error header names the FD-PWB code.
package fdtransport

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdroute"
	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdsig"
	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdwire"
)

// Environment variables fd-transport reads.
const (
	EnvSubscriberID   = "SUBSCRIBER_ID"
	EnvUniqueKeyID    = "UNIQUE_KEY_ID"
	EnvSigningPrivate = "SIGNING_PRIVATE"
	EnvRequireHTTPS   = "FD_REQUIRE_HTTPS" // overrides requireHTTPS when set
)

// DefaultExpiresIn is the signature lifetime when expiresIn is not configured.
const DefaultExpiresIn = 5 * time.Minute

// FD-PWB codes of fd-transport's refusals. A refused request is never sent: the caller gets
// a NACK response carrying X-Pwb-Fd-Gate-Error: <code>.
const (
	CodeHostNotAllowed  = "FD-PWB-0032" // HOST_NOT_ALLOWED (HTTP 400): not https, or not in allowedHosts
	CodeNotRenderable   = "FD-PWB-0033" // NOT_RENDERABLE (HTTP 400): no or unknown X-Pwb-Fd-Route, bad path parameters
	CodeSigningDisabled = "FD-PWB-0034" // SIGNING_UNAVAILABLE (HTTP 500): no signing key configured, or signing failed
)

// SignedHeaders is the header list fd-transport signs.
var SignedHeaders = []string{fdsig.PseudoCreated, fdsig.PseudoExpires, fdsig.HeaderDigest, fdsig.PseudoRequestTarget}

// Config is fd-transport's configuration.
type Config struct {
	// InternalHosts are the workbench-internal destinations, as host or host:port. A bare
	// host matches any port; host:port matches that port only (an absent port in the
	// request URL counts as the scheme's default).
	InternalHosts []string
	// AllowedHosts, when set, are the only participant hosts requests may go to (same
	// matching as InternalHosts). Empty: any host.
	AllowedHosts []string
	// RouteTable is the path to fd_routes.yaml (required).
	RouteTable string
	// The workbench's signing identity (default env SUBSCRIBER_ID, UNIQUE_KEY_ID,
	// SIGNING_PRIVATE). SigningPrivateKey is base64 of an Ed25519 seed.
	SubscriberID      string
	UniqueKeyID       string
	SigningPrivateKey string
	// ExpiresIn is the signature lifetime (expiresIn, ISO 8601 or Go duration; default PT5M).
	ExpiresIn time.Duration
	// RequireHTTPS refuses non-https participants (requireHTTPS, default true; env
	// FD_REQUIRE_HTTPS overrides).
	RequireHTTPS bool
}

// ParseConfig reads the plugin config map, whose values are usually strings (lists may be
// comma-separated strings or lists), and the environment.
func ParseConfig(cfg map[string]any) (*Config, error) {
	return parseConfig(cfg, os.Getenv)
}

func parseConfig(cfg map[string]any, getenv func(string) string) (*Config, error) {
	c := &Config{ExpiresIn: DefaultExpiresIn, RequireHTTPS: true}
	var err error
	if c.InternalHosts, err = stringList(cfg, "internalHosts"); err != nil {
		return nil, err
	}
	if c.AllowedHosts, err = stringList(cfg, "allowedHosts"); err != nil {
		return nil, err
	}
	str := func(key, envKey string) (string, error) {
		v, err := stringValue(cfg, key)
		if err != nil || v != "" || envKey == "" {
			return v, err
		}
		return strings.TrimSpace(getenv(envKey)), nil
	}
	if c.RouteTable, err = str("routeTable", ""); err != nil {
		return nil, err
	}
	if c.SubscriberID, err = str("subscriberId", EnvSubscriberID); err != nil {
		return nil, err
	}
	if c.UniqueKeyID, err = str("uniqueKeyId", EnvUniqueKeyID); err != nil {
		return nil, err
	}
	if c.SigningPrivateKey, err = str("signingPrivateKey", EnvSigningPrivate); err != nil {
		return nil, err
	}
	raw, err := stringValue(cfg, "expiresIn")
	if err != nil {
		return nil, err
	}
	if raw != "" {
		d, err := fdwire.ParseDuration(raw)
		if err != nil || d < time.Second {
			return nil, fmt.Errorf("fd-transport: invalid expiresIn %q (an ISO 8601 or Go duration of at least 1s)", raw)
		}
		c.ExpiresIn = d
	}
	raw, err = stringValue(cfg, "requireHTTPS")
	if err != nil {
		return nil, err
	}
	if v := strings.TrimSpace(getenv(EnvRequireHTTPS)); v != "" {
		raw = v
	}
	if raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("fd-transport: invalid requireHTTPS %q", raw)
		}
		c.RequireHTTPS = v
	}
	return c, nil
}

// stringValue reads a scalar config value. The plugin manager passes strings; a YAML bool
// written unquoted is accepted too.
func stringValue(cfg map[string]any, key string) (string, error) {
	switch v := cfg[key].(type) {
	case nil:
		return "", nil
	case string:
		return strings.TrimSpace(v), nil
	case bool:
		return strconv.FormatBool(v), nil
	default:
		return "", fmt.Errorf("fd-transport: %s must be a string, got %T", key, cfg[key])
	}
}

// stringList reads a comma-separated string or a list of strings.
func stringList(cfg map[string]any, key string) ([]string, error) {
	raw, ok := cfg[key]
	if !ok || raw == nil {
		return nil, nil
	}
	var items []string
	switch v := raw.(type) {
	case string:
		items = strings.Split(v, ",")
	case []string:
		items = v
	case []any:
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("fd-transport: %s entry %v is not a string", key, e)
			}
			items = append(items, s)
		}
	default:
		return nil, fmt.Errorf("fd-transport: %s must be a comma-separated string, got %T", key, raw)
	}
	var out []string
	for _, it := range items {
		if it = strings.TrimSpace(it); it != "" {
			out = append(out, it)
		}
	}
	return out, nil
}

type hostKey struct {
	host string // lower-case, no brackets
	port string // "" = any port
}

// Wrapper implements beckn-onix's definition.TransportWrapper.
type Wrapper struct {
	internal     map[hostKey]bool
	allowed      map[hostKey]bool
	table        *fdroute.Table
	keyID        string // "sub|ukid|ed25519"; "" when no signing key is configured
	privateKey   string
	expiresIn    time.Duration
	requireHTTPS bool
	now          func() time.Time
}

// New builds a Wrapper: it loads the route table and checks the signing key.
func New(c *Config) (*Wrapper, error) {
	if strings.TrimSpace(c.RouteTable) == "" {
		return nil, errors.New("fd-transport: routeTable is required")
	}
	table, err := fdroute.Load(c.RouteTable)
	if err != nil {
		return nil, fmt.Errorf("fd-transport: %w", err)
	}
	w := &Wrapper{
		internal: map[hostKey]bool{}, allowed: map[hostKey]bool{}, table: table,
		expiresIn: c.ExpiresIn, requireHTTPS: c.RequireHTTPS, now: time.Now,
	}
	if w.expiresIn <= 0 {
		w.expiresIn = DefaultExpiresIn
	}
	for _, h := range c.InternalHosts {
		w.internal[parseHost(h, "")] = true
	}
	for _, h := range c.AllowedHosts {
		w.allowed[parseHost(h, "")] = true
	}
	set := 0
	for _, v := range []string{c.SubscriberID, c.UniqueKeyID, c.SigningPrivateKey} {
		if v != "" {
			set++
		}
	}
	switch set {
	case 0: // requests for participants are refused until a key is configured
	case 3:
		seed, err := base64.StdEncoding.DecodeString(c.SigningPrivateKey)
		if err != nil || len(seed) < 32 {
			return nil, errors.New("fd-transport: signingPrivateKey must be base64 of an Ed25519 seed (32+ bytes)")
		}
		w.keyID = c.SubscriberID + "|" + c.UniqueKeyID + "|ed25519"
		w.privateKey = c.SigningPrivateKey
	default:
		return nil, errors.New("fd-transport: subscriberId, uniqueKeyId and signingPrivateKey must be set together")
	}
	return w, nil
}

// CanSign reports whether a signing key is configured.
func (w *Wrapper) CanSign() bool { return w.keyID != "" }

// Wrap returns base wrapped by fd-transport. A nil base means http.DefaultTransport.
func (w *Wrapper) Wrap(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &roundTripper{base: base, w: w}
}

// IsInternal reports whether a request to hostport (scheme supplies the default port) stays
// inside the workbench.
func (w *Wrapper) IsInternal(scheme, hostport string) bool {
	return matches(w.internal, scheme, hostport)
}

func matches(set map[hostKey]bool, scheme, hostport string) bool {
	k := parseHost(hostport, defaultPort(scheme))
	return set[k] || set[hostKey{host: k.host}]
}

type roundTripper struct {
	base http.RoundTripper
	w    *Wrapper
}

func (rt *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	if host == "" {
		host = req.Host
	}
	if rt.w.IsInternal(req.URL.Scheme, host) {
		return rt.base.RoundTrip(req)
	}
	out, refusal := rt.w.prepare(req)
	if refusal != nil {
		return refusal, nil
	}
	start := time.Now()
	resp, err := rt.base.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	if resp.Header == nil {
		resp.Header = http.Header{}
	}
	resp.Header.Set(fdroute.HeaderUpstreamMs, strconv.FormatInt(time.Since(start).Milliseconds(), 10))
	return resp, nil
}

// prepare renders, checks and signs a participant request. It consumes and closes
// req.Body; the caller's request is otherwise untouched. A request that cannot be rendered,
// sent to its host or signed yields a NACK response (with X-Pwb-Fd-Gate-Error) instead.
func (w *Wrapper) prepare(req *http.Request) (*http.Request, *http.Response) {
	body, err := readAndClose(req.Body)
	party := fdwire.Party(fdroute.RoleInvestorApp)
	refuse := func(status int, code, name, format string, args ...any) (*http.Request, *http.Response) {
		return nil, refusal(req, body, status, code, party, name+": "+fmt.Sprintf(format, args...))
	}
	if err != nil {
		return refuse(http.StatusInternalServerError, CodeNotRenderable, "NOT_RENDERABLE", "cannot read the body: %v", err)
	}

	action := strings.TrimSpace(req.Header.Get(fdroute.HeaderFdRoute))
	if action == "" {
		return refuse(http.StatusBadRequest, CodeNotRenderable, "NOT_RENDERABLE",
			"%s %s has no %s header; a request for a participant is never sent unrendered or unsigned", req.Method, req.URL.Redacted(), fdroute.HeaderFdRoute)
	}
	route, ok := w.table.ByAction(action)
	if !ok {
		return refuse(http.StatusBadRequest, CodeNotRenderable, "NOT_RENDERABLE", "%s %q is not an FD %s action", fdroute.HeaderFdRoute, action, w.table.Version)
	}
	party = fdwire.Party(route.Caller)
	if !w.CanSign() {
		return refuse(http.StatusInternalServerError, CodeSigningDisabled, "SIGNING_UNAVAILABLE",
			"no signing key is configured (subscriberId/uniqueKeyId/signingPrivateKey or env %s/%s/%s)", EnvSubscriberID, EnvUniqueKeyID, EnvSigningPrivate)
	}
	params := map[string]string{}
	if raw := strings.TrimSpace(req.Header.Get(fdroute.HeaderFdPathParams)); raw != "" {
		if err := json.Unmarshal([]byte(raw), &params); err != nil || params == nil {
			return refuse(http.StatusBadRequest, CodeNotRenderable, "NOT_RENDERABLE", "%s is not a JSON object of strings: %s", fdroute.HeaderFdPathParams, raw)
		}
	}
	restPath, err := route.RenderPath(params)
	if err != nil {
		return refuse(http.StatusBadRequest, CodeNotRenderable, "NOT_RENDERABLE", "%v", err)
	}

	// The router's target is <subscriber_url>/<action>: the base is what precedes /<action>.
	basePath := req.URL.EscapedPath()
	basePath = strings.TrimSuffix(basePath, "/"+action)
	basePath = strings.TrimRight(basePath, "/")
	raw := req.URL.Scheme + "://" + req.URL.Host + basePath + restPath
	isGet := route.Method == http.MethodGet
	if isGet {
		if q := readQuery(body); q != "" {
			raw += "?" + q
		}
	}
	target, err := url.Parse(raw)
	if err != nil {
		return refuse(http.StatusBadRequest, CodeNotRenderable, "NOT_RENDERABLE", "cannot build the participant URL %q: %v", raw, err)
	}
	if msg := w.checkHost(target); msg != "" {
		return refuse(http.StatusBadRequest, CodeHostNotAllowed, "HOST_NOT_ALLOWED", "%s", msg)
	}

	out := req.Clone(req.Context())
	out.URL = target
	out.Host = target.Host
	out.Method = route.Method
	out.RequestURI = ""
	out.TransferEncoding = nil
	out.Header.Del("Cookie")
	fdroute.StripInternal(out.Header)
	out.Header.Del("Content-Length")
	out.Header.Del("Authorization")
	sent := body
	if isGet {
		sent = []byte{}
		out.Body = http.NoBody
		out.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
		out.ContentLength = 0
		out.Header.Del("Content-Type")
	} else {
		out.Body = io.NopCloser(bytes.NewReader(sent))
		out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(sent)), nil }
		out.ContentLength = int64(len(sent))
		if out.Header.Get("Content-Type") == "" {
			out.Header.Set("Content-Type", "application/json")
		}
	}

	created := w.now().Unix()
	expires := created + int64(w.expiresIn/time.Second)
	ss, err := fdsig.SigningString(SignedHeaders, fdsig.Inputs{
		Method: out.Method, RequestTarget: target.RequestURI(), Created: created, Expires: expires, Body: sent,
	})
	if err != nil {
		return refuse(http.StatusInternalServerError, CodeSigningDisabled, "SIGNING_UNAVAILABLE", "signing string: %v", err)
	}
	sig, err := fdsig.Sign(ss, w.privateKey)
	if err != nil {
		return refuse(http.StatusInternalServerError, CodeSigningDisabled, "SIGNING_UNAVAILABLE", "sign: %v", err)
	}
	out.Header.Set("Authorization", (&fdsig.Authorization{
		KeyID: w.keyID, Algorithm: "ed25519", Created: created, Expires: expires,
		Headers: SignedHeaders, Signature: sig,
	}).String())
	out.Header.Set("Digest", fdsig.Digest(sent))
	return out, nil
}

// refusal is the NACK response for a request fd-transport will not send. It echoes the
// internal body's context and names the code in X-Pwb-Fd-Gate-Error, so the playground can
// tell the workbench's refusal from a participant's answer.
func refusal(req *http.Request, body []byte, status int, code, party, message string) *http.Response {
	nack := fdwire.NackBody(contextOf(body), fdwire.WorkbenchError(code, message, party, ""))
	return &http.Response{
		Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), StatusCode: status,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{
			"Content-Type": {"application/json"},
			http.CanonicalHeaderKey(fdroute.HeaderFdGateError): {code},
		},
		Body:          io.NopCloser(bytes.NewReader(nack)),
		ContentLength: int64(len(nack)), Request: req,
	}
}

// checkHost returns why target may not be called, or "".
func (w *Wrapper) checkHost(target *url.URL) string {
	if w.requireHTTPS && target.Scheme != "https" {
		return fmt.Sprintf("participant URL %s is not https", target.Redacted())
	}
	if target.Scheme != "https" && target.Scheme != "http" {
		return fmt.Sprintf("participant URL %s has scheme %q", target.Redacted(), target.Scheme)
	}
	if len(w.allowed) > 0 && !matches(w.allowed, target.Scheme, target.Host) {
		return fmt.Sprintf("participant host %s is not in allowedHosts", target.Host)
	}
	return ""
}

// readQuery renders a read's query from the internal body's message.query (string values,
// keys sorted).
func readQuery(body []byte) string {
	var b struct {
		Message struct {
			Query map[string]any `json:"query"`
		} `json:"message"`
	}
	if json.Unmarshal(body, &b) != nil || len(b.Message.Query) == 0 {
		return ""
	}
	keys := make([]string, 0, len(b.Message.Query))
	for k := range b.Message.Query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	q := url.Values{}
	for _, k := range keys {
		switch v := b.Message.Query[k].(type) {
		case string:
			q.Add(k, v)
		case float64, bool:
			q.Add(k, fmt.Sprint(v))
		case []any:
			for _, e := range v {
				q.Add(k, fmt.Sprint(e))
			}
		}
	}
	return q.Encode()
}

// contextOf returns the internal body's context, for echoing on a NACK.
func contextOf(body []byte) json.RawMessage {
	var b struct {
		Context json.RawMessage `json:"context"`
	}
	if json.Unmarshal(body, &b) != nil {
		return nil
	}
	return b.Context
}

func readAndClose(rc io.ReadCloser) ([]byte, error) {
	if rc == nil || rc == http.NoBody {
		return []byte{}, nil
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// parseHost splits "host[:port]" (IPv6 in brackets) into a lower-case key; port defaults
// to def.
func parseHost(hostport, def string) hostKey {
	hostport = strings.TrimSpace(hostport)
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return hostKey{host: strings.ToLower(h), port: p}
	}
	return hostKey{host: strings.ToLower(strings.Trim(hostport, "[]")), port: def}
}

func defaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}
