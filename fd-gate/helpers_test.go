package fdgate

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdroute"
	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdsig"
	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdwire"
)

const (
	domain        = "ONDC:FIS14:FD"
	version       = "0.1.4"
	prefix        = "/api-service/ONDC:FIS14:FD/0.1.4/issuer"
	appPrefix     = "/api-service/ONDC:FIS14:FD/0.1.4/investor-app"
	mockPrefix    = "/api-service/ONDC:FIS14:FD/0.1.4/mock"
	appID         = "fd.exampledistributor.in"
	appURI        = "https://fd.exampledistributor.in/fd/callbacks"
	issuerID      = "fd.examplebank.in"
	issuerURI     = "https://fd.examplebank.in/fd"
	appUKID       = "app-k1"
	issuerUKID    = "iss-k1"
	transactionID = "6a0b6b2e-64c4-4c8a-9b2a-3f6f1c2d55aa"
)

// testNow is the workbench clock in every test.
var testNow = time.Date(2027, 4, 19, 6, 0, 0, 0, time.UTC)

func strp(s string) *string { return &s }

// writeTable writes a small FD route table covering every kind and mode.
func writeTable(t *testing.T) string {
	t.Helper()
	req := func(action, method, path string, params []string, mode string) fdroute.Route {
		return fdroute.Route{
			Action: action, Kind: fdroute.KindRequest, Method: method, Path: path, PathParams: params,
			Mode: mode, Caller: fdroute.RoleInvestorApp, Callee: fdroute.RoleDepositIssuer,
			HasBody: method == "POST", OperationID: action,
		}
	}
	confirm := req("application_confirm", "POST", "/applications/{applicationId}/confirm", []string{"applicationId"}, fdroute.ModeAsyncAckCallback)
	confirm.Callback = strp("on_application_confirm")
	withdraw := req("consent_withdraw", "POST", "/consents/{consentId}/withdraw", []string{"consentId"}, fdroute.ModeAsyncAckCallback)
	withdraw.Callback = strp("on_consent_withdraw")
	discovery := req("discovery_schemes_query", "POST", "/discovery/schemes/query", nil, fdroute.ModeSync)
	discovery.Callee = fdroute.RoleIndex
	data, err := fdroute.Marshal(&fdroute.Table{
		FormatVersion: 1, Domain: domain, Version: version,
		Routes: []fdroute.Route{
			confirm,
			withdraw,
			discovery,
			req("deposit_events_read", "GET", "/deposits/{depositId}/events", []string{"depositId"}, fdroute.ModeSync),
			req("deposit_read", "GET", "/deposits/{depositId}", []string{"depositId"}, fdroute.ModeSync),
			req("document_read", "GET", "/documents/{documentId}", []string{"documentId"}, fdroute.ModeSync),
			req("portfolio_query", "POST", "/deposits/query", nil, fdroute.ModeSync),
			req("quote_read", "GET", "/quotes/{quoteId}", []string{"quoteId"}, fdroute.ModeSync),
			req("quote_request", "POST", "/quotes", nil, fdroute.ModeSync),
			{
				Action: "on_application_confirm", Kind: fdroute.KindCallback, Method: "POST", Path: "/on_application_confirm",
				Mode: fdroute.ModeAsyncAckCallback, Caller: fdroute.RoleDepositIssuer, Callee: fdroute.RoleInvestorApp,
				InReplyTo: strp("application_confirm"), HasBody: true, OperationID: "onApplicationConfirm",
			},
			{
				Action: "on_deposit_booked", Kind: fdroute.KindWebhook, Method: "POST", Path: "/on_deposit_booked",
				Mode: fdroute.ModeWebhook, Caller: fdroute.RoleDepositIssuer, Callee: fdroute.RoleInvestorApp,
				HasBody: true, OperationID: "onDepositBooked",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "fd_routes.yaml")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// ---- keys ----

type keyPair struct{ priv, pub string }

func newKeyPair(seedByte byte) keyPair {
	seed := bytes.Repeat([]byte{seedByte}, ed25519.SeedSize)
	k := ed25519.NewKeyFromSeed(seed)
	return keyPair{
		priv: base64.StdEncoding.EncodeToString(seed),
		pub:  base64.StdEncoding.EncodeToString(k.Public().(ed25519.PublicKey)),
	}
}

var (
	appKeys    = newKeyPair(1)
	issuerKeys = newKeyPair(2)
	otherKeys  = newKeyPair(3)
)

// fakeKeys is a KeyLookup over a fixed map.
type fakeKeys struct {
	keys  map[string]string // "sub|ukid" -> public key
	calls atomic.Int32
}

func newFakeKeys() *fakeKeys {
	return &fakeKeys{keys: map[string]string{
		appID + "|" + appUKID:       appKeys.pub,
		issuerID + "|" + issuerUKID: issuerKeys.pub,
	}}
}

func (f *fakeKeys) LookupSigningKey(_ context.Context, sub, ukid string) (string, error) {
	f.calls.Add(1)
	if k, ok := f.keys[sub+"|"+ukid]; ok {
		return k, nil
	}
	return "", ErrSignerNotFound
}

// signOpts controls how a test request is signed.
type signOpts struct {
	sub, ukid, priv  string
	created, expires int64
	headers          []string
	body             []byte // digest over these bytes instead of the request body
	target           string // request target instead of the request's
}

// sign sets an FD Authorization header on req over body.
func sign(t *testing.T, req *http.Request, body []byte, o signOpts) {
	t.Helper()
	if o.sub == "" {
		o.sub, o.ukid, o.priv = appID, appUKID, appKeys.priv
	}
	if o.created == 0 {
		o.created, o.expires = testNow.Unix()-1, testNow.Unix()+300
	}
	if o.headers == nil {
		o.headers = []string{"(created)", "(expires)", "digest", "(request-target)"}
	}
	if o.body != nil {
		body = o.body
	}
	target := o.target
	if target == "" {
		target = req.URL.RequestURI()
	}
	ss, err := fdsig.SigningString(o.headers, fdsig.Inputs{
		Method: req.Method, RequestTarget: target, Created: o.created, Expires: o.expires, Body: body, Header: req.Header,
	})
	if err != nil {
		t.Fatal(err)
	}
	sig, err := fdsig.Sign(ss, o.priv)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", (&fdsig.Authorization{
		KeyID: o.sub + "|" + o.ukid + "|ed25519", Algorithm: "ed25519",
		Created: o.created, Expires: o.expires, Headers: o.headers, Signature: sig,
	}).String())
}

// ---- store ----

// memStore is an in-memory Store with optional hooks.
type memStore struct {
	mu      sync.Mutex
	data    map[string][]byte
	gets    atomic.Int32
	failing error
	onGet   func(key string, value []byte)
}

func newMemStore() *memStore { return &memStore{data: map[string][]byte{}} }

func (s *memStore) SetNX(_ context.Context, key string, value []byte, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing != nil {
		return false, s.failing
	}
	if _, ok := s.data[key]; ok {
		return false, nil
	}
	s.data[key] = append([]byte(nil), value...)
	return true, nil
}

func (s *memStore) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	if s.failing != nil {
		s.mu.Unlock()
		return nil, s.failing
	}
	v, ok := s.data[key]
	hook := s.onGet
	s.mu.Unlock()
	s.gets.Add(1)
	if !ok {
		return nil, ErrNotFound
	}
	if hook != nil {
		hook(key, v)
	}
	return append([]byte(nil), v...), nil
}

func (s *memStore) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing != nil {
		return s.failing
	}
	s.data[key] = append([]byte(nil), value...)
	return nil
}

func (s *memStore) Del(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}

func (s *memStore) get(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	return v, ok
}

func (s *memStore) put(t *testing.T, key string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.data[key] = data
	s.mu.Unlock()
}

// ---- notifier ----

type fakeNotifier struct {
	mu      sync.Mutex
	notices []ReplayNotice
	ch      chan ReplayNotice
}

func newFakeNotifier() *fakeNotifier { return &fakeNotifier{ch: make(chan ReplayNotice, 16)} }

func (f *fakeNotifier) NotifyReplay(_ context.Context, n ReplayNotice) error {
	f.mu.Lock()
	f.notices = append(f.notices, n)
	f.mu.Unlock()
	f.ch <- n
	return nil
}

func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.notices)
}

// ---- gate ----

type testGate struct {
	g        *Gate
	store    *memStore
	keys     *fakeKeys
	notifier *fakeNotifier
}

func baseConfig(t *testing.T, mode Mode, pathPrefix string) *Config {
	return &Config{
		RouteTable: writeTable(t), Mode: mode, PathPrefix: pathPrefix, Addr: "redis:6379",
		Signature: SignatureRequired, RequireHTTPS: true, ClockSkew: DefaultClockSkew,
		ReplayTTL: DefaultReplayTTL, ObjectTTL: DefaultObjectTTL, MaxBodyBytes: DefaultMaxBodyBytes,
	}
}

func newTestGate(t *testing.T, c *Config) *testGate {
	t.Helper()
	tg := &testGate{store: newMemStore(), keys: newFakeKeys(), notifier: newFakeNotifier()}
	g, err := NewGate(c, Deps{Store: tg.store, Keys: tg.keys, Notifier: tg.notifier, Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	tg.g = g
	return tg
}

func newNPGate(t *testing.T, mutate ...func(*Config)) *testGate {
	t.Helper()
	c := baseConfig(t, ModeNP, prefix)
	for _, m := range mutate {
		m(c)
	}
	return newTestGate(t, c)
}

// ---- messages ----

// msg is an FD message body under construction.
type msg map[string]any

func (m msg) ctx() map[string]any { return m["context"].(map[string]any) }
func (m msg) env() map[string]any { return m["fdEnvelope"].(map[string]any) }
func (m msg) corr() map[string]any {
	return m.env()["correlation"].(map[string]any)
}

func (m msg) bytes(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// newMsg builds a valid message for action, sent by role.
func newMsg(action, role, messageID string, corr map[string]any) msg {
	if corr == nil {
		corr = map[string]any{}
	}
	return msg{
		"context": map[string]any{
			"domain": domain, "location": map[string]any{"country": map[string]any{"code": "IND"}, "city": map[string]any{"code": "*"}},
			"version": version, "action": action,
			"bap_id": appID, "bap_uri": appURI, "bpp_id": issuerID, "bpp_uri": issuerURI,
			"transaction_id": transactionID, "message_id": messageID,
			"timestamp": testNow.Add(-time.Second).Format(time.RFC3339Nano), "ttl": "PT30S",
		},
		"fdEnvelope": map[string]any{
			"envelopeVersion": "1.0", "payloadVersion": "1.0", "mode": "ASYNC_ACK_CALLBACK",
			"originatorRole": role, "correlation": corr,
		},
		"message": map[string]any{"confirm": true},
	}
}

func confirmMsg(messageID string) msg {
	return newMsg("application_confirm", "INVESTOR_APP", messageID, map[string]any{"applicationId": "APP-1", "applicationVersion": 3})
}

// npRequest builds a signed participant request.
func npRequest(t *testing.T, method, target string, body []byte, o signOpts) *http.Request {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	req.Header.Set("Content-Type", "application/json")
	if body == nil {
		body = []byte{}
	}
	sign(t, req, body, o)
	return req
}

// ---- inner handlers ----

// captured is what the inner chain saw.
type captured struct {
	method, path, requestURI string
	header                   http.Header
	body                     []byte
	res                      *fdroute.Resolution
}

// inner is a scripted inner chain.
type inner struct {
	mu     sync.Mutex
	calls  atomic.Int32
	last   *captured
	status int
	body   string
	header map[string]string
	before func() // runs before answering
}

func (h *inner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.calls.Add(1)
	data, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	h.last = &captured{method: r.Method, path: r.URL.Path, requestURI: r.RequestURI, header: r.Header.Clone(), body: data, res: fdroute.FromContext(r.Context())}
	h.mu.Unlock()
	if h.before != nil {
		h.before()
	}
	for k, v := range h.header {
		w.Header().Set(k, v)
	}
	status := h.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, h.body)
}

func (h *inner) seen() *captured {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}

const (
	becknACK  = `{"message":{"ack":{"status":"ACK"}}}`
	becknNACK = `{"message":{"ack":{"status":"NACK"}},"error":{"code":"30000","paths":"$.message.confirm","message":"[FROM ONIX WORKBENCH] confirm must be an object"}}`
)

func ackInner() *inner { return &inner{body: becknACK} }

var mustNotRun = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	panic("next must not be called")
})

// serve runs one request through the gate in front of next.
func (tg *testGate) serve(next http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	tg.g.Handler(next).ServeHTTP(rec, req)
	return rec
}

// decodeAck decodes an acknowledgement body.
func decodeAck(t *testing.T, rec *httptest.ResponseRecorder) fdwire.Body {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q (body %s)", ct, rec.Body.String())
	}
	var b fdwire.Body
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return b
}

// expectNack checks a NACK's status and code and the FD Error's fixed fields.
func expectNack(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) fdwire.Body {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, status, rec.Body.String())
	}
	b := decodeAck(t, rec)
	if b.Message.Ack.Status != "NACK" || b.Error == nil {
		t.Fatalf("not a NACK: %s", rec.Body.String())
	}
	e := b.Error
	if e.Code != code {
		t.Fatalf("code = %s, want %s (%s)", e.Code, code, e.Message)
	}
	if e.Class != "PROTOCOL" || e.Reason != "FIELD_CONSTRAINT_VIOLATED" || e.Retryable {
		t.Fatalf("error fields = %+v", e)
	}
	if e.ObligedParty != "N-01 INVESTOR_APP" && e.ObligedParty != "N-02 DEPOSIT_ISSUER" {
		t.Fatalf("obligedParty = %q", e.ObligedParty)
	}
	if name := CodeName(code); name == "" || !strings.HasPrefix(e.Message, name+": ") {
		t.Fatalf("message %q does not start with the code name", e.Message)
	}
	if !strings.Contains(rec.Body.String(), `"fieldPath":`) {
		t.Fatalf("fieldPath missing: %s", rec.Body.String())
	}
	return b
}

var errBoom = errors.New("boom")
