package fdgate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdroute"
	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdsig"
)

func newMockGate(t *testing.T, key string) *testGate {
	t.Helper()
	c := baseConfig(t, ModeMock, mockPrefix)
	c.Signature = SignatureOff
	c.InternalKey = key
	return newTestGate(t, c)
}

func mockRequest(action string, body []byte, key, params string) *http.Request {
	req := httptest.NewRequest("POST", mockPrefix+"/"+action, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(fdroute.HeaderInternalKey, key)
	}
	if params != "" {
		req.Header.Set(fdroute.HeaderFdPathParams, params)
	}
	return req
}

func TestMockHappyPath(t *testing.T) {
	tg := newMockGate(t, "s3cret")
	npAnswer := `{"context":{"action":"application_confirm"},"message":{"ack":{"status":"ACK"}}}`
	in := &inner{status: http.StatusAccepted, body: npAnswer, header: map[string]string{fdroute.HeaderUpstreamMs: "42"}}
	m := confirmMsg("m-mock")
	m.ctx()["bap_uri"] = "http://localhost:3000/cb" // the playground is not held to https here
	body := m.bytes(t)
	req := mockRequest("application_confirm", body, "s3cret", `{"applicationId":"APP-1"}`)
	req.Header.Set(fdroute.HeaderFdReplay, "1")
	req.Header.Set("X-Pwb-Other", "x")
	req.Header.Set("Cookie", "a=b")
	rec := tg.serve(in, req)

	if rec.Code != http.StatusAccepted || rec.Body.String() != npAnswer || rec.Header().Get(fdroute.HeaderUpstreamMs) != "42" {
		t.Fatalf("answer changed: %d %s %v", rec.Code, rec.Body.String(), rec.Header())
	}
	if rec.Header().Get(fdroute.HeaderFdGateError) != "" {
		t.Fatal("a participant's answer marked as the workbench's refusal")
	}
	if !rec.Flushed {
		t.Fatal("not flushed")
	}
	got := in.seen()
	h := got.header
	if h.Get(fdroute.HeaderFdRoute) != "application_confirm" || h.Get(fdroute.HeaderFdPathParams) != `{"applicationId":"APP-1"}` ||
		h.Get(fdroute.HeaderFdMethod) != "POST" || h.Get(fdroute.HeaderFdPath) != "/applications/APP-1/confirm" || h.Get(fdroute.HeaderFdReplay) != "1" ||
		h.Get(fdroute.HeaderFdOrigin) != fdroute.OriginWorkbench {
		t.Fatalf("FD headers %v", h)
	}
	if h.Get(fdroute.HeaderInternalKey) != "" || h.Get("X-Pwb-Other") != "" || h.Get("Cookie") != "" {
		t.Fatalf("internal headers passed on: %v", h)
	}
	if got.path != mockPrefix+"/application_confirm" || !bytes.Equal(got.body, body) {
		t.Fatalf("inner saw %s %s", got.path, got.body)
	}
	if got.res == nil || got.res.Route.Action != "application_confirm" || !got.res.Replay || got.res.PathParams["applicationId"] != "APP-1" {
		t.Fatalf("resolution %+v", got.res)
	}
	if _, ok := tg.store.get(ObjectKey(domain, "applicationId", "APP-1")); !ok {
		t.Fatal("correlation not indexed in mock mode")
	}
}

func TestMockCallbackAndGetRoutes(t *testing.T) {
	tg := newMockGate(t, "")
	in := ackInner()
	// A callback needs no path parameters.
	cb := newMsg("on_application_confirm", "DEPOSIT_ISSUER", "m-cb", map[string]any{"applicationId": "APP-1", "depositId": "D-7"})
	rec := tg.serve(in, mockRequest("on_application_confirm", cb.bytes(t), "", ""))
	if rec.Code != http.StatusOK || in.seen().header.Get(fdroute.HeaderFdPath) != "/on_application_confirm" || in.seen().header.Get(fdroute.HeaderFdPathParams) != "{}" {
		t.Fatalf("%d %v", rec.Code, in.seen().header)
	}
	if _, ok := tg.store.get(ObjectKey(domain, "depositId", "D-7")); !ok {
		t.Fatal("depositId not indexed")
	}
	// A read is sent by the playground as its internal body.
	rd := newMsg("deposit_read", "INVESTOR_APP", "m-rd", map[string]any{"depositId": "D/7"})
	rec = tg.serve(in, mockRequest("deposit_read", rd.bytes(t), "", `{"depositId":"D/7"}`))
	if rec.Code != http.StatusOK || in.seen().header.Get(fdroute.HeaderFdMethod) != "GET" || in.seen().header.Get(fdroute.HeaderFdPath) != "/deposits/D%2F7" {
		t.Fatalf("%d %v", rec.Code, in.seen().header)
	}
}

func TestMockRejections(t *testing.T) {
	good := confirmMsg("m-bad")
	wrongAction := confirmMsg("m-bad")
	wrongAction.ctx()["action"] = "application_cancel"
	wrongRole := confirmMsg("m-bad")
	wrongRole.env()["originatorRole"] = "DEPOSIT_ISSUER"
	tests := []struct {
		name   string
		key    string // configured PWB_INTERNAL_KEY
		req    func(t *testing.T) *http.Request
		status int
		code   string
	}{
		{"wrong internal key", "s3cret", func(t *testing.T) *http.Request {
			return mockRequest("application_confirm", good.bytes(t), "guess", `{"applicationId":"APP-1"}`)
		}, http.StatusForbidden, CodeInternalKeyInvalid},
		{"missing internal key", "s3cret", func(t *testing.T) *http.Request {
			return mockRequest("application_confirm", good.bytes(t), "", `{"applicationId":"APP-1"}`)
		}, http.StatusForbidden, CodeInternalKeyInvalid},
		{"unknown action", "", func(t *testing.T) *http.Request {
			return mockRequest("application_teleport", good.bytes(t), "", "")
		}, http.StatusNotFound, CodeRouteNotFound},
		{"REST path instead of action", "", func(t *testing.T) *http.Request {
			return mockRequest("applications/APP-1/confirm", good.bytes(t), "", "")
		}, http.StatusNotFound, CodeRouteNotFound},
		{"outside the module", "", func(t *testing.T) *http.Request {
			return httptest.NewRequest("POST", "/elsewhere/application_confirm", nil)
		}, http.StatusNotFound, CodeRouteNotFound},
		{"GET", "", func(t *testing.T) *http.Request {
			return httptest.NewRequest("GET", mockPrefix+"/deposit_read", nil)
		}, http.StatusNotFound, CodeRouteNotFound},
		{"malformed body", "", func(t *testing.T) *http.Request {
			return mockRequest("application_confirm", []byte("nope"), "", `{"applicationId":"APP-1"}`)
		}, http.StatusBadRequest, CodeBodyMalformed},
		{"action mismatch", "", func(t *testing.T) *http.Request {
			return mockRequest("application_confirm", wrongAction.bytes(t), "", `{"applicationId":"APP-1"}`)
		}, http.StatusBadRequest, CodeActionPathMismatch},
		{"wrong direction", "", func(t *testing.T) *http.Request {
			return mockRequest("application_confirm", wrongRole.bytes(t), "", `{"applicationId":"APP-1"}`)
		}, http.StatusBadRequest, CodeWrongDirection},
		{"path params not JSON", "", func(t *testing.T) *http.Request {
			return mockRequest("application_confirm", good.bytes(t), "", `applicationId=APP-1`)
		}, http.StatusBadRequest, CodePathParamsInvalid},
		{"path params not strings", "", func(t *testing.T) *http.Request {
			return mockRequest("application_confirm", good.bytes(t), "", `{"applicationId":1}`)
		}, http.StatusBadRequest, CodePathParamsInvalid},
		{"path param missing", "", func(t *testing.T) *http.Request {
			return mockRequest("application_confirm", good.bytes(t), "", "")
		}, http.StatusBadRequest, CodePathParamsInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tg := newMockGate(t, tt.key)
			rec := tg.serve(mustNotRun, tt.req(t))
			expectNack(t, rec, tt.status, tt.code)
			if got := rec.Header().Get(fdroute.HeaderFdGateError); got != tt.code {
				t.Fatalf("%s = %q, want %s", fdroute.HeaderFdGateError, got, tt.code)
			}
		})
	}
}

func TestMockWithoutConfiguredKey(t *testing.T) {
	tg := newMockGate(t, "")
	in := ackInner()
	rec := tg.serve(in, mockRequest("application_confirm", confirmMsg("m-nokey").bytes(t), "anything", `{"applicationId":"APP-1"}`))
	if rec.Code != http.StatusOK || in.calls.Load() != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestMockPassesTransportRefusalThrough(t *testing.T) {
	// fd-transport refused to send (host check): its NACK and gate-error header reach the playground.
	tg := newMockGate(t, "")
	refusal := `{"message":{"ack":{"status":"NACK"}},"error":{"code":"FD-PWB-0032","class":"PROTOCOL","reason":"FIELD_CONSTRAINT_VIOLATED","message":"HOST_NOT_ALLOWED: x","obligedParty":"N-01 INVESTOR_APP","retryable":false,"fieldPath":""}}`
	in := &inner{status: http.StatusBadRequest, body: refusal, header: map[string]string{fdroute.HeaderFdGateError: "FD-PWB-0032"}}
	rec := tg.serve(in, mockRequest("application_confirm", confirmMsg("m-host").bytes(t), "", `{"applicationId":"APP-1"}`))
	if rec.Code != http.StatusBadRequest || rec.Body.String() != refusal || rec.Header().Get(fdroute.HeaderFdGateError) != "FD-PWB-0032" {
		t.Fatalf("%d %s %v", rec.Code, rec.Body.String(), rec.Header())
	}
}

func TestMockPanicIsMarked(t *testing.T) {
	tg := newMockGate(t, "")
	rec := tg.serve(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }),
		mockRequest("application_confirm", confirmMsg("m-p").bytes(t), "", `{"applicationId":"APP-1"}`))
	expectNack(t, rec, http.StatusOK, CodeInternal)
	if rec.Header().Get(fdroute.HeaderFdGateError) != CodeInternal {
		t.Fatalf("headers %v", rec.Header())
	}
}

func TestNPNeverSendsGateErrorHeader(t *testing.T) {
	tg := newNPGate(t)
	// fd-gate's own refusal in np mode carries no internal header...
	rec := tg.serve(mustNotRun, npRequest(t, "POST", confirmTarget, []byte("nope"), signOpts{}))
	expectNack(t, rec, http.StatusOK, CodeBodyMalformed)
	if rec.Header().Get(fdroute.HeaderFdGateError) != "" {
		t.Fatal("gate-error header sent to a participant")
	}
	// ...and one from the inner chain is stripped.
	in := &inner{status: 400, body: `{"message":{"ack":{"status":"NACK"}},"error":{"code":"FD-PWB-0032","class":"PROTOCOL","message":"x","obligedParty":"N-01 INVESTOR_APP","retryable":false}}`,
		header: map[string]string{fdroute.HeaderFdGateError: "FD-PWB-0032"}}
	rec = tg.serve(in, npRequest(t, "POST", prefix+"/quotes", newMsg("quote_request", "INVESTOR_APP", "m-ge", nil).bytes(t), signOpts{}))
	if rec.Code != http.StatusOK || rec.Header().Get(fdroute.HeaderFdGateError) != "" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
}

func TestMockPassesProxyFailureThrough(t *testing.T) {
	tg := newMockGate(t, "")
	rec := tg.serve(&inner{status: http.StatusBadGateway}, mockRequest("application_confirm", confirmMsg("m-502").bytes(t), "", `{"applicationId":"APP-1"}`))
	if rec.Code != http.StatusBadGateway || rec.Body.Len() != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// ---- Registry lookup ----

func TestRegistryLookup(t *testing.T) {
	wb := newKeyPair(9)
	var lastBody map[string]string
	var lastAuth string
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/v2.0/lookup" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &lastBody)
		lastAuth = r.Header.Get("Authorization")
		// The lookup is signed by the workbench.
		auth, err := fdsig.ParseAuthorization(lastAuth)
		if err == nil {
			ss, _ := fdsig.SigningString(auth.Headers, fdsig.Inputs{Created: auth.Created, Expires: auth.Expires, Body: data})
			err = fdsig.Verify(ss, auth.Signature, wb.pub)
		}
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch lastBody["subscriber_id"] {
		case appID:
			_, _ = io.WriteString(w, `[{"subscriber_id":"`+appID+`","ukId":"old","signing_public_key":"OLD"},{"subscriber_id":"`+appID+`","ukId":"`+appUKID+`","signing_public_key":"`+appKeys.pub+`"}]`)
		case "bare.example":
			_, _ = io.WriteString(w, `[{"signing_public_key":"BARE"}]`)
		case "gone.example":
			_, _ = io.WriteString(w, `[]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	l := &RegistryLookup{URL: srv.URL + "/v2.0", SubscriberID: "wb.example", UniqueKeyID: "wb-k", SigningPrivateKey: wb.priv}
	key, err := l.LookupSigningKey(context.Background(), appID, appUKID)
	if err != nil || key != appKeys.pub {
		t.Fatalf("key %q, %v", key, err)
	}
	if lastBody["subscriber_id"] != appID || lastBody["ukId"] != appUKID || !strings.Contains(lastAuth, `keyId="wb.example|wb-k|ed25519"`) {
		t.Fatalf("lookup request %v %s", lastBody, lastAuth)
	}
	if key, err := l.LookupSigningKey(context.Background(), "bare.example", "k"); err != nil || key != "BARE" {
		t.Fatalf("bare: %q %v", key, err)
	}
	for _, sub := range []string{"gone.example", "unknown.example"} {
		if _, err := l.LookupSigningKey(context.Background(), sub, "k"); !errors.Is(err, ErrSignerNotFound) {
			t.Fatalf("%s: want ErrSignerNotFound, got %v", sub, err)
		}
	}
	if _, err := l.LookupSigningKey(context.Background(), appID, "rotated"); !errors.Is(err, ErrSignerNotFound) {
		t.Fatalf("unknown key id: %v", err)
	}
	// Without workbench keys the lookup goes unsigned (and this registry refuses it).
	if _, err := (&RegistryLookup{URL: srv.URL + "/v2.0/"}).LookupSigningKey(context.Background(), appID, appUKID); err == nil || errors.Is(err, ErrSignerNotFound) {
		t.Fatalf("unsigned lookup: %v", err)
	}
	if _, err := (&RegistryLookup{URL: "http://127.0.0.1:1/"}).LookupSigningKey(context.Background(), appID, appUKID); err == nil {
		t.Fatal("want transport error")
	}
}

type countingLookup struct {
	calls atomic.Int32
	err   error
}

func (c *countingLookup) LookupSigningKey(context.Context, string, string) (string, error) {
	c.calls.Add(1)
	return "KEY", c.err
}

func TestCachedLookup(t *testing.T) {
	next := &countingLookup{}
	now := testNow
	c := NewCachedLookup(next, KeyCacheTTL)
	c.Now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if k, err := c.LookupSigningKey(context.Background(), "a", "k"); err != nil || k != "KEY" {
			t.Fatal(k, err)
		}
	}
	if next.calls.Load() != 1 {
		t.Fatalf("calls %d", next.calls.Load())
	}
	_, _ = c.LookupSigningKey(context.Background(), "a", "k2") // another key id
	now = now.Add(KeyCacheTTL + time.Second)                   // expired
	_, _ = c.LookupSigningKey(context.Background(), "a", "k")
	if next.calls.Load() != 3 {
		t.Fatalf("calls %d", next.calls.Load())
	}
	// Failures are not cached.
	failing := &countingLookup{err: errBoom}
	fc := NewCachedLookup(failing, KeyCacheTTL)
	for i := 0; i < 2; i++ {
		if _, err := fc.LookupSigningKey(context.Background(), "a", "k"); err == nil {
			t.Fatal("want error")
		}
	}
	if failing.calls.Load() != 2 {
		t.Fatalf("failure cached: %d calls", failing.calls.Load())
	}
}

func TestNPSignatureUsesKeyCache(t *testing.T) {
	// End to end through New's production wiring would need a registry; here the cache sits in
	// front of a fake and is consulted once for two requests.
	tg := newNPGate(t)
	cached := NewCachedLookup(tg.keys, KeyCacheTTL)
	tg.g.keys = cached
	in := ackInner()
	tg.serve(in, npRequest(t, "POST", confirmTarget, confirmMsg("m-c1").bytes(t), signOpts{}))
	tg.serve(in, npRequest(t, "POST", confirmTarget, confirmMsg("m-c2").bytes(t), signOpts{}))
	if tg.keys.calls.Load() != 1 || in.calls.Load() != 2 {
		t.Fatalf("lookups %d, calls %d", tg.keys.calls.Load(), in.calls.Load())
	}
}

func TestMockKeepsScenarioDirectives(t *testing.T) {
	tg := newMockGate(t, "s3cret")
	in := &inner{status: http.StatusOK, body: `{"message":{"ack":{"status":"ACK"}}}`}
	m := confirmMsg("m-scn")
	m.ctx()["bap_uri"] = "http://localhost:3000/cb"
	req := mockRequest("application_confirm", m.bytes(t), "s3cret", `{"applicationId":"APP-1"}`)
	req.Header.Set(fdroute.HeaderFdScenario, `{"expectError":"FD-PRODUCT-0007"}`)
	tg.serve(in, req)
	if got := in.seen().header.Get(fdroute.HeaderFdScenario); got != `{"expectError":"FD-PRODUCT-0007"}` {
		t.Fatalf("scenario header = %q", got)
	}

	// Not JSON, or too long: dropped.
	for _, bad := range []string{"not json", `{"x":"` + strings.Repeat("a", maxScenarioHeader) + `"}`} {
		in := &inner{status: http.StatusOK, body: `{"message":{"ack":{"status":"ACK"}}}`}
		req := mockRequest("application_confirm", confirmMsgWithURI(t, "m-scn-bad"), "s3cret", `{"applicationId":"APP-1"}`)
		req.Header.Set(fdroute.HeaderFdScenario, bad)
		tg.serve(in, req)
		if got := in.seen().header.Get(fdroute.HeaderFdScenario); got != "" {
			t.Fatalf("kept %q", got)
		}
	}
}

func confirmMsgWithURI(t *testing.T, messageID string) []byte {
	m := confirmMsg(messageID)
	m.ctx()["bap_uri"] = "http://localhost:3000/cb"
	return m.bytes(t)
}
