package fdtransport

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdroute"
	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdsig"
	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdwire"
)

var testNow = time.Date(2027, 4, 19, 6, 0, 0, 0, time.UTC)

const (
	wbID   = "workbench.ondc.org"
	wbUKID = "wb-k1"
)

func keys() (priv, pub string) {
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	k := ed25519.NewKeyFromSeed(seed)
	return base64.StdEncoding.EncodeToString(seed), base64.StdEncoding.EncodeToString(k.Public().(ed25519.PublicKey))
}

func strp(s string) *string { return &s }

func writeTable(t *testing.T) string {
	t.Helper()
	data, err := fdroute.Marshal(&fdroute.Table{
		FormatVersion: 1, Domain: "ONDC:FIS14:FD", Version: "0.1.4",
		Routes: []fdroute.Route{
			{Action: "application_confirm", Kind: fdroute.KindRequest, Method: "POST", Path: "/applications/{applicationId}/confirm",
				PathParams: []string{"applicationId"}, Mode: fdroute.ModeAsyncAckCallback, Caller: fdroute.RoleInvestorApp,
				Callee: fdroute.RoleDepositIssuer, Callback: strp("on_application_confirm"), HasBody: true, OperationID: "confirmApplication"},
			{Action: "deposit_events_read", Kind: fdroute.KindRequest, Method: "GET", Path: "/deposits/{depositId}/events",
				PathParams: []string{"depositId"}, Mode: fdroute.ModeSync, Caller: fdroute.RoleInvestorApp,
				Callee: fdroute.RoleDepositIssuer, OperationID: "readDepositEvents"},
			{Action: "on_application_confirm", Kind: fdroute.KindCallback, Method: "POST", Path: "/on_application_confirm",
				Mode: fdroute.ModeAsyncAckCallback, Caller: fdroute.RoleDepositIssuer, Callee: fdroute.RoleInvestorApp,
				InReplyTo: strp("application_confirm"), HasBody: true, OperationID: "onApplicationConfirm"},
			{Action: "on_deposit_booked", Kind: fdroute.KindWebhook, Method: "POST", Path: "/on_deposit_booked",
				Mode: fdroute.ModeWebhook, Caller: fdroute.RoleDepositIssuer, Callee: fdroute.RoleInvestorApp,
				HasBody: true, OperationID: "onDepositBooked"},
			{Action: "quote_request", Kind: fdroute.KindRequest, Method: "POST", Path: "/quotes",
				Mode: fdroute.ModeSync, Caller: fdroute.RoleInvestorApp, Callee: fdroute.RoleDepositIssuer, HasBody: true, OperationID: "requestQuote"},
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

func testConfig(t *testing.T) *Config {
	priv, _ := keys()
	return &Config{
		InternalHosts: []string{"mock-service:3000"}, RouteTable: writeTable(t),
		SubscriberID: wbID, UniqueKeyID: wbUKID, SigningPrivateKey: priv,
		ExpiresIn: DefaultExpiresIn, RequireHTTPS: true,
	}
}

func newWrapper(t *testing.T, mutate ...func(*Config)) *Wrapper {
	t.Helper()
	c := testConfig(t)
	for _, m := range mutate {
		m(c)
	}
	w, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	w.now = func() time.Time { return testNow }
	return w
}

// fakeRT records the request it was given, with its body.
type fakeRT struct {
	mu   sync.Mutex
	got  *http.Request
	body []byte
	n    int
}

func (f *fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	f.got = req
	f.body = nil
	if req.Body != nil {
		f.body, _ = io.ReadAll(req.Body)
		req.Body.Close()
	}
	return &http.Response{StatusCode: http.StatusAccepted, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"message":{"ack":{"status":"ACK"}}}`)), Request: req}, nil
}

// internalRequest is what the stdHandler proxy hands the transport: POST <target>/<action>
// with the internal body and the FD headers.
func internalRequest(target, action, params string, body []byte) *http.Request {
	req := httptest.NewRequest("POST", target, bytes.NewReader(body))
	req.RequestURI = "" // client request
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "subscriber_url=x")
	req.Header.Set(fdroute.HeaderInternalKey, "secret")
	req.Header["x-pwb-raw"] = []string{"z"}
	req.Header.Set("Authorization", "Signature stale")
	if action != "" {
		req.Header.Set(fdroute.HeaderFdRoute, action)
	}
	if params != "" {
		req.Header.Set(fdroute.HeaderFdPathParams, params)
	}
	return req
}

// verifyOutbound checks the outbound request's FD signature against what was sent.
func verifyOutbound(t *testing.T, method, requestURI string, header http.Header, body []byte) *fdsig.Authorization {
	t.Helper()
	_, pub := keys()
	auth, err := fdsig.ParseAuthorization(header.Get("Authorization"))
	if err != nil {
		t.Fatal(err)
	}
	if auth.KeyID != wbID+"|"+wbUKID+"|ed25519" || auth.Algorithm != "ed25519" ||
		!reflect.DeepEqual(auth.Headers, []string{"(created)", "(expires)", "digest", "(request-target)"}) {
		t.Fatalf("authorization %+v", auth)
	}
	if auth.Created != testNow.Unix() || auth.Expires != testNow.Unix()+300 {
		t.Fatalf("window %d..%d", auth.Created, auth.Expires)
	}
	if header.Get("Digest") != fdsig.Digest(body) {
		t.Fatalf("Digest header %q", header.Get("Digest"))
	}
	ss, err := fdsig.SigningString(auth.Headers, fdsig.Inputs{Method: method, RequestTarget: requestURI, Created: auth.Created, Expires: auth.Expires, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if err := fdsig.Verify(ss, auth.Signature, pub); err != nil {
		t.Fatalf("signature does not verify over %q: %v", ss, err)
	}
	return auth
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestParseConfig(t *testing.T) {
	tests := []struct {
		in   map[string]any
		want []string
	}{
		{map[string]any{}, nil},
		{map[string]any{"internalHosts": ""}, nil},
		{map[string]any{"internalHosts": " mock-service:3000 , localhost,, [::1]:8080 "}, []string{"mock-service:3000", "localhost", "[::1]:8080"}},
		{map[string]any{"internalHosts": []string{"a", " b "}}, []string{"a", "b"}},
		{map[string]any{"internalHosts": []any{"a", "b:1"}}, []string{"a", "b:1"}},
	}
	for _, tt := range tests {
		c, err := parseConfig(tt.in, envOf(nil))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(c.InternalHosts, tt.want) {
			t.Fatalf("%v: got %v want %v", tt.in, c.InternalHosts, tt.want)
		}
	}
	for _, bad := range []map[string]any{
		{"internalHosts": 5}, {"internalHosts": []any{"a", 1}}, {"allowedHosts": 1.5},
		{"routeTable": 3}, {"expiresIn": "soon"}, {"expiresIn": "PT0S"}, {"requireHTTPS": "maybe"},
	} {
		if _, err := parseConfig(bad, envOf(nil)); err == nil {
			t.Fatalf("want error for %v", bad)
		}
	}
}

func TestParseConfigKeysAndDefaults(t *testing.T) {
	env := map[string]string{"SUBSCRIBER_ID": "env.sub", "UNIQUE_KEY_ID": "env-uk", "SIGNING_PRIVATE": "ZW52"}
	c, err := parseConfig(map[string]any{"routeTable": "/r.yaml", "allowedHosts": "np.example.com, other.example:8443"}, envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{
		AllowedHosts: []string{"np.example.com", "other.example:8443"}, RouteTable: "/r.yaml",
		SubscriberID: "env.sub", UniqueKeyID: "env-uk", SigningPrivateKey: "ZW52",
		ExpiresIn: 5 * time.Minute, RequireHTTPS: true,
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v\nwant %+v", c, want)
	}
	// Config beats env for the identity; FD_REQUIRE_HTTPS beats config.
	c, err = parseConfig(map[string]any{
		"routeTable": "/r.yaml", "subscriberId": "cfg.sub", "uniqueKeyId": "cfg-uk", "signingPrivateKey": "Y2Zn",
		"expiresIn": "PT2M", "requireHTTPS": true,
	}, envOf(map[string]string{"SUBSCRIBER_ID": "env.sub", "FD_REQUIRE_HTTPS": "false"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.SubscriberID != "cfg.sub" || c.UniqueKeyID != "cfg-uk" || c.SigningPrivateKey != "Y2Zn" || c.ExpiresIn != 2*time.Minute || c.RequireHTTPS {
		t.Fatalf("got %+v", c)
	}
	if c, _ := parseConfig(map[string]any{"requireHTTPS": "false"}, envOf(nil)); c.RequireHTTPS {
		t.Fatal("requireHTTPS=false ignored")
	}
	// Through the real environment.
	t.Setenv("FD_REQUIRE_HTTPS", "false")
	t.Setenv("SUBSCRIBER_ID", "real.env")
	if c, err := ParseConfig(map[string]any{}); err != nil || c.RequireHTTPS || c.SubscriberID != "real.env" {
		t.Fatalf("ParseConfig: %+v %v", c, err)
	}
}

func TestNewValidates(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"no route table":  func(c *Config) { c.RouteTable = "" },
		"missing table":   func(c *Config) { c.RouteTable = filepath.Join(t.TempDir(), "absent.yaml") },
		"partial key":     func(c *Config) { c.UniqueKeyID = "" },
		"key not base64":  func(c *Config) { c.SigningPrivateKey = "not base64!" },
		"key too short":   func(c *Config) { c.SigningPrivateKey = base64.StdEncoding.EncodeToString([]byte("short")) },
		"only subscriber": func(c *Config) { c.UniqueKeyID, c.SigningPrivateKey = "", "" },
	} {
		c := testConfig(t)
		mutate(c)
		if _, err := New(c); err == nil {
			t.Fatalf("%s: want error", name)
		}
	}
	c := testConfig(t)
	c.SubscriberID, c.UniqueKeyID, c.SigningPrivateKey = "", "", ""
	w, err := New(c)
	if err != nil || w.CanSign() {
		t.Fatalf("no key: %v %v", w, err)
	}
}

func TestRenderAndSign(t *testing.T) {
	tests := []struct {
		name, target, action, params string
		body                         string
		method, wantURL              string
		wantBody                     bool
	}{
		{"async request", "https://np.example.com/fd/v0.1/application_confirm", "application_confirm", `{"applicationId":"APP 1/2"}`,
			`{"context":{"action":"application_confirm"},"fdEnvelope":{},"message":{"confirm":true}}`,
			"POST", "https://np.example.com/fd/v0.1/applications/APP%201%2F2/confirm", true},
		{"sync request without params", "https://np.example.com/fd/v0.1/quote_request", "quote_request", "",
			`{"context":{"action":"quote_request"},"fdEnvelope":{},"message":{}}`,
			"POST", "https://np.example.com/fd/v0.1/quotes", true},
		{"GET read", "https://np.example.com:8443/fd/deposit_events_read", "deposit_events_read", `{"depositId":"D-1"}`,
			`{"context":{"action":"deposit_events_read"},"fdEnvelope":{},"message":{"pathParams":{"depositId":"D-1"},"query":{"since":"2026-01-01","limit":5}}}`,
			"GET", "https://np.example.com:8443/fd/deposits/D-1/events?limit=5&since=2026-01-01", false},
		{"callback", "https://app.example.com/fd/callbacks/on_application_confirm", "on_application_confirm", "",
			`{"context":{"action":"on_application_confirm"},"fdEnvelope":{"inReplyTo":"m-1"},"message":{}}`,
			"POST", "https://app.example.com/fd/callbacks/on_application_confirm", true},
		{"webhook", "https://app.example.com/hooks/on_deposit_booked", "on_deposit_booked", "",
			`{"context":{"action":"on_deposit_booked"},"fdEnvelope":{},"message":{}}`,
			"POST", "https://app.example.com/hooks/on_deposit_booked", true},
		{"target without the action", "https://app.example.com/hooks/", "on_deposit_booked", "",
			`{"context":{},"fdEnvelope":{},"message":{}}`,
			"POST", "https://app.example.com/hooks/on_deposit_booked", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := &fakeRT{}
			rt := newWrapper(t).Wrap(base)
			req := internalRequest(tt.target, tt.action, tt.params, []byte(tt.body))
			before := req.Header.Clone()
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			out := base.got
			if out == req {
				t.Fatal("request was not cloned")
			}
			if out.Method != tt.method || out.URL.String() != tt.wantURL || out.Host != out.URL.Host {
				t.Fatalf("got %s %s (Host %s), want %s %s", out.Method, out.URL, out.Host, tt.method, tt.wantURL)
			}
			wantSent := []byte(tt.body)
			if !tt.wantBody {
				wantSent = []byte{}
				if out.ContentLength != 0 || len(base.body) != 0 || out.Header.Get("Content-Type") != "" {
					t.Fatalf("GET carries a body: len %d, %q, %v", out.ContentLength, base.body, out.Header)
				}
			} else if !bytes.Equal(base.body, wantSent) || out.ContentLength != int64(len(wantSent)) {
				t.Fatalf("body %q (len %d)", base.body, out.ContentLength)
			}
			verifyOutbound(t, out.Method, out.URL.RequestURI(), out.Header, wantSent)

			if out.Header.Get("Cookie") != "" || len(out.Header["x-pwb-raw"]) != 0 {
				t.Fatalf("headers not stripped: %v", out.Header)
			}
			for name := range out.Header {
				if fdroute.IsInternalHeader(name) {
					t.Fatalf("internal header %s sent", name)
				}
			}
			if !reflect.DeepEqual(req.Header, before) || req.Method != "POST" {
				t.Fatal("the caller's request was modified")
			}
			if ms, err := strconv.Atoi(resp.Header.Get(fdroute.HeaderUpstreamMs)); err != nil || ms < 0 {
				t.Fatalf("%s = %q", fdroute.HeaderUpstreamMs, resp.Header.Get(fdroute.HeaderUpstreamMs))
			}
		})
	}
}

// TestWireRequest sends through a real TLS server, which checks the signature over the
// request line it actually received.
func TestWireRequest(t *testing.T) {
	type seen struct {
		method, uri string
		header      http.Header
		body        []byte
		cl          int64
	}
	got := make(chan seen, 2)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.Method, r.RequestURI, r.Header.Clone(), b, r.ContentLength}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"events":[]}`)
	}))
	defer srv.Close()
	rt := newWrapper(t).Wrap(srv.Client().Transport)

	read := internalRequest(srv.URL+"/fd/deposit_events_read", "deposit_events_read", `{"depositId":"D:1"}`,
		[]byte(`{"context":{},"fdEnvelope":{},"message":{"query":{"since":"a b"}}}`))
	resp, err := rt.RoundTrip(read)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	s := <-got
	if s.method != "GET" || s.uri != "/fd/deposits/D:1/events?since=a+b" || len(s.body) != 0 || s.cl != 0 {
		t.Fatalf("server saw %s %s body %q cl %d", s.method, s.uri, s.body, s.cl)
	}
	verifyOutbound(t, s.method, s.uri, s.header, nil)
	if resp.Header.Get(fdroute.HeaderUpstreamMs) == "" {
		t.Fatal("no upstream time")
	}

	body := []byte(`{"context":{"action":"application_confirm"},"fdEnvelope":{},"message":{}}`)
	post := internalRequest(srv.URL+"/fd/application_confirm", "application_confirm", `{"applicationId":"A-1"}`, body)
	resp, err = rt.RoundTrip(post)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	s = <-got
	if s.method != "POST" || s.uri != "/fd/applications/A-1/confirm" || !bytes.Equal(s.body, body) {
		t.Fatalf("server saw %s %s %q", s.method, s.uri, s.body)
	}
	verifyOutbound(t, s.method, s.uri, s.header, body)
	if s.header.Get("Cookie") != "" || s.header.Get(fdroute.HeaderInternalKey) != "" || s.header.Get(fdroute.HeaderFdRoute) != "" {
		t.Fatalf("internal headers on the wire: %v", s.header)
	}
}

// expectRefusal checks a refusal response: status, NACK body, code and gate-error header.
func expectRefusal(t *testing.T, resp *http.Response, err error, status int, code string) fdwire.Body {
	t.Helper()
	if err != nil {
		t.Fatalf("want a refusal response, got error %v", err)
	}
	if resp.StatusCode != status || resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get(fdroute.HeaderFdGateError) != code {
		t.Fatalf("status %d, headers %v", resp.StatusCode, resp.Header)
	}
	data, _ := io.ReadAll(resp.Body)
	var b fdwire.Body
	if err := json.Unmarshal(data, &b); err != nil || b.Message.Ack.Status != "NACK" || b.Error == nil || b.Error.Code != code ||
		b.Error.Class != "PROTOCOL" || b.Error.Reason != "FIELD_CONSTRAINT_VIOLATED" || b.Error.Retryable {
		t.Fatalf("body %s", data)
	}
	return b
}

func TestRefusesWithoutRoute(t *testing.T) {
	for name, tt := range map[string]struct {
		action, params string
		mutate         func(*Config)
		status         int
		code           string
	}{
		"no route header":         {"", "", nil, 400, CodeNotRenderable},
		"unknown action":          {"application_teleport", "", nil, 400, CodeNotRenderable},
		"missing path param":      {"application_confirm", `{}`, nil, 400, CodeNotRenderable},
		"path params not JSON":    {"application_confirm", `applicationId=A`, nil, 400, CodeNotRenderable},
		"path params not strings": {"application_confirm", `{"applicationId":1}`, nil, 400, CodeNotRenderable},
		"no signing key": {"application_confirm", `{"applicationId":"A"}`, func(c *Config) {
			c.SubscriberID, c.UniqueKeyID, c.SigningPrivateKey = "", "", ""
		}, 500, CodeSigningDisabled},
	} {
		t.Run(name, func(t *testing.T) {
			base := &fakeRT{}
			var muts []func(*Config)
			if tt.mutate != nil {
				muts = append(muts, tt.mutate)
			}
			rt := newWrapper(t, muts...).Wrap(base)
			resp, err := rt.RoundTrip(internalRequest("https://np.example.com/fd/application_confirm", tt.action, tt.params,
				[]byte(`{"context":{"message_id":"m-1"}}`)))
			b := expectRefusal(t, resp, err, tt.status, tt.code)
			if string(b.Context) != `{"message_id":"m-1"}` {
				t.Fatalf("context not echoed: %s", b.Context)
			}
			if base.n != 0 {
				t.Fatal("request was sent")
			}
		})
	}
}

func TestHostCheck(t *testing.T) {
	body := []byte(`{"context":{"action":"application_confirm","message_id":"m-1"},"fdEnvelope":{},"message":{}}`)
	tests := []struct {
		name   string
		target string
		mutate func(*Config)
		ok     bool
	}{
		{"https, any host", "https://np.example.com/fd/application_confirm", nil, true},
		{"http refused", "http://np.example.com/fd/application_confirm", nil, false},
		{"http allowed when not https-only", "http://np.example.com/fd/application_confirm", func(c *Config) { c.RequireHTTPS = false }, true},
		{"listed host", "https://np.example.com/fd/application_confirm", func(c *Config) { c.AllowedHosts = []string{"NP.example.com"} }, true},
		{"listed host:port", "https://np.example.com:8443/fd/application_confirm", func(c *Config) { c.AllowedHosts = []string{"np.example.com:8443"} }, true},
		{"other port", "https://np.example.com:9443/fd/application_confirm", func(c *Config) { c.AllowedHosts = []string{"np.example.com:8443"} }, false},
		{"unlisted host", "https://evil.example.com/fd/application_confirm", func(c *Config) { c.AllowedHosts = []string{"np.example.com"} }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := &fakeRT{}
			var muts []func(*Config)
			if tt.mutate != nil {
				muts = append(muts, tt.mutate)
			}
			rt := newWrapper(t, muts...).Wrap(base)
			resp, err := rt.RoundTrip(internalRequest(tt.target, "application_confirm", `{"applicationId":"A-1"}`, body))
			if err != nil {
				t.Fatal(err)
			}
			if tt.ok {
				if base.n != 1 || resp.StatusCode != http.StatusAccepted {
					t.Fatalf("not sent: %d", resp.StatusCode)
				}
				return
			}
			if base.n != 0 {
				t.Fatal("refused request was sent")
			}
			b := expectRefusal(t, resp, err, http.StatusBadRequest, CodeHostNotAllowed)
			if b.Error.ObligedParty != "N-01 INVESTOR_APP" || !strings.HasPrefix(b.Error.Message, "HOST_NOT_ALLOWED: ") {
				t.Fatalf("error %+v", b.Error)
			}
			if string(b.Context) != `{"action":"application_confirm","message_id":"m-1"}` {
				t.Fatalf("context not echoed: %s", b.Context)
			}
		})
	}
}

func TestInternalRequestPassesUnchanged(t *testing.T) {
	base := &fakeRT{}
	rt := newWrapper(t, func(c *Config) { c.InternalHosts = []string{"mock-service:3000", "LocalHost", "[::1]:8080", "api:80"} }).Wrap(base)
	for _, target := range []string{
		"http://mock-service:3000/mock/on_search",
		"http://localhost:5555/x", // bare host matches any port
		"http://LOCALHOST/x",
		"http://[::1]:8080/x",
		"http://api/x", // default port 80
	} {
		req := internalRequest(target, "", "", []byte(`{}`))
		before := req.Header.Clone()
		if _, err := rt.RoundTrip(req); err != nil {
			t.Fatal(err)
		}
		if base.got != req || !reflect.DeepEqual(base.got.Header, before) {
			t.Fatalf("%s: internal request was changed", target)
		}
	}
	// Not internal: refused without a route header (and so never sent unsigned).
	for _, target := range []string{
		"http://mock-service:3001/x", // other port
		"https://api/x",              // default port 443, entry is :80
		"http://[::1]:9090/x",
		"http://mock-service.evil.com:3000/x",
	} {
		base.n = 0
		resp, err := rt.RoundTrip(internalRequest(target, "", "", []byte(`{}`)))
		expectRefusal(t, resp, err, http.StatusBadRequest, CodeNotRenderable)
		if base.n != 0 {
			t.Fatalf("%s: treated as internal", target)
		}
	}
}

func TestWrapNilBase(t *testing.T) {
	rt := newWrapper(t).Wrap(nil).(*roundTripper)
	if rt.base != http.DefaultTransport {
		t.Fatal("nil base should mean http.DefaultTransport")
	}
}

func TestBaseErrorPropagates(t *testing.T) {
	rt := newWrapper(t).Wrap(errRT{})
	_, err := rt.RoundTrip(internalRequest("https://np.example.com/fd/quote_request", "quote_request", "", []byte(`{}`)))
	if err == nil || !strings.Contains(err.Error(), "dial") {
		t.Fatalf("err %v", err)
	}
}

type errRT struct{}

func (errRT) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp: refused")
}
