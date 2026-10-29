package fdgate

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdroute"
	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdsig"
)

const confirmTarget = prefix + "/applications/APP-1/confirm"

func TestNPSignatureFailures(t *testing.T) {
	body := confirmMsg("m-sig").bytes(t)
	now := testNow.Unix()
	tests := []struct {
		name  string
		build func(t *testing.T) *http.Request
		code  string
		want  string // substring of the message
	}{
		{"missing", func(t *testing.T) *http.Request {
			r := npRequest(t, "POST", confirmTarget, body, signOpts{})
			r.Header.Del("Authorization")
			return r
		}, CodeSignatureMissing, "no Authorization"},
		{"malformed", func(t *testing.T) *http.Request {
			r := npRequest(t, "POST", confirmTarget, body, signOpts{})
			r.Header.Set("Authorization", "Bearer abc")
			return r
		}, CodeSignatureInvalid, "cannot be parsed"},
		{"request-target not signed", func(t *testing.T) *http.Request {
			return npRequest(t, "POST", confirmTarget, body, signOpts{headers: []string{"(created)", "(expires)", "digest"}})
		}, CodeRequestTargetNotSig, "(request-target)"},
		{"digest not signed", func(t *testing.T) *http.Request {
			return npRequest(t, "POST", confirmTarget, body, signOpts{headers: []string{"(created)", "(expires)", "(request-target)"}})
		}, CodeSignatureInvalid, "digest"},
		{"wrong algorithm", func(t *testing.T) *http.Request {
			r := npRequest(t, "POST", confirmTarget, body, signOpts{})
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "|ed25519", "|rsa", 1))
			return r
		}, CodeSignatureInvalid, "ed25519"},
		{"expired", func(t *testing.T) *http.Request {
			return npRequest(t, "POST", confirmTarget, body, signOpts{created: now - 600, expires: now - 60})
		}, CodeSignatureExpired, "expired"},
		{"created in the future", func(t *testing.T) *http.Request {
			return npRequest(t, "POST", confirmTarget, body, signOpts{created: now + 60, expires: now + 600})
		}, CodeSignatureExpired, "future"},
		{"expires before created", func(t *testing.T) *http.Request {
			return npRequest(t, "POST", confirmTarget, body, signOpts{created: now, expires: now - 1})
		}, CodeSignatureExpired, "before created"},
		{"signer unknown", func(t *testing.T) *http.Request {
			return npRequest(t, "POST", confirmTarget, body, signOpts{sub: appID, ukid: "rotated", priv: appKeys.priv})
		}, CodeSignerUnknown, appID + "|rotated"},
		{"wrong key", func(t *testing.T) *http.Request {
			return npRequest(t, "POST", confirmTarget, body, signOpts{sub: appID, ukid: appUKID, priv: otherKeys.priv})
		}, CodeSignatureInvalid, "does not verify"},
		{"body changed after signing", func(t *testing.T) *http.Request {
			return npRequest(t, "POST", confirmTarget, body, signOpts{body: confirmMsg("m-other").bytes(t)})
		}, CodeSignatureInvalid, "does not verify"},
		{"signed for another path", func(t *testing.T) *http.Request {
			return npRequest(t, "POST", confirmTarget, body, signOpts{target: prefix + "/applications/APP-2/confirm"})
		}, CodeSignatureInvalid, "does not verify"},
		{"digest over [object Object]", func(t *testing.T) *http.Request {
			return npRequest(t, "POST", confirmTarget, body, signOpts{body: []byte(fdsig.ObjectObjectLiteral)})
		}, CodeSignatureObjObj, "[object Object]"},
		{"signer is not the sender", func(t *testing.T) *http.Request {
			return npRequest(t, "POST", confirmTarget, body, signOpts{sub: issuerID, ukid: issuerUKID, priv: issuerKeys.priv})
		}, CodeSignerNotSender, "bap_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tg := newNPGate(t)
			rec := tg.serve(mustNotRun, tt.build(t))
			b := expectNack(t, rec, http.StatusOK, tt.code)
			if !strings.Contains(b.Error.Message, tt.want) {
				t.Fatalf("message %q does not mention %q", b.Error.Message, tt.want)
			}
			// The request's context is echoed.
			var c map[string]any
			if err := json.Unmarshal(b.Context, &c); err != nil || c["message_id"] != "m-sig" {
				t.Fatalf("context not echoed: %s", b.Context)
			}
		})
	}
}

func TestNPSignatureWithinSkew(t *testing.T) {
	tg := newNPGate(t)
	now := testNow.Unix()
	// created 4s ahead and expired 4s ago are both inside the 5s skew.
	for _, o := range []signOpts{{created: now + 4, expires: now + 300}, {created: now - 300, expires: now - 4}} {
		in := ackInner()
		rec := tg.serve(in, npRequest(t, "POST", confirmTarget, confirmMsg("m-skew-"+time.Unix(o.created, 0).Format("150405")).bytes(t), o))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%+v: %d %s", o, rec.Code, rec.Body.String())
		}
	}
}

func TestNPSignatureOff(t *testing.T) {
	tg := newNPGate(t, func(c *Config) { c.Signature = SignatureOff })
	in := ackInner()
	req := httptest.NewRequest("POST", confirmTarget, bytes.NewReader(confirmMsg("m-off").bytes(t)))
	rec := tg.serve(in, req)
	if rec.Code != http.StatusAccepted || tg.keys.calls.Load() != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	// The sender from the context scopes the replay key.
	if _, ok := tg.store.get(ReplayKey(domain, appID, "application_confirm", "m-off")); !ok {
		t.Fatal("replay key not scoped by the context's sender")
	}
}

func TestNPTransportRules(t *testing.T) {
	type mut func(m msg)
	set := func(path string, v any) mut {
		return func(m msg) {
			parts := strings.Split(path, ".")
			cur := map[string]any(m)
			for _, p := range parts[:len(parts)-1] {
				cur = cur[p].(map[string]any)
			}
			if v == nil {
				delete(cur, parts[len(parts)-1])
			} else {
				cur[parts[len(parts)-1]] = v
			}
		}
	}
	tests := []struct {
		name      string
		target    string
		raw       []byte // instead of a message
		mutate    []mut
		code      string
		fieldPath string
	}{
		{name: "not JSON", raw: []byte("<xml/>"), code: CodeBodyMalformed},
		{name: "JSON array", raw: []byte("[]"), code: CodeBodyMalformed},
		{name: "no context", mutate: []mut{set("context", nil)}, code: CodeBodyMalformed, fieldPath: "/context"},
		{name: "no fdEnvelope", mutate: []mut{set("fdEnvelope", nil)}, code: CodeBodyMalformed, fieldPath: "/fdEnvelope"},
		{name: "no message", mutate: []mut{set("message", nil)}, code: CodeBodyMalformed, fieldPath: "/message"},
		{name: "correlation not an object", mutate: []mut{set("fdEnvelope.correlation", "APP-1")}, code: CodeBodyMalformed, fieldPath: "/fdEnvelope/correlation"},
		{name: "action mismatch", mutate: []mut{set("context.action", "application_cancel")}, code: CodeActionPathMismatch, fieldPath: "/context/action"},
		{name: "version mismatch", mutate: []mut{set("context.version", "0.1.3")}, code: CodeVersionMismatch, fieldPath: "/context/version"},
		{name: "domain mismatch", mutate: []mut{set("context.domain", "ONDC:FIS12")}, code: CodeVersionMismatch, fieldPath: "/context/domain"},
		{name: "wrong direction", mutate: []mut{set("fdEnvelope.originatorRole", "DEPOSIT_ISSUER")}, code: CodeWrongDirection, fieldPath: "/fdEnvelope/originatorRole"},
		{name: "path vs correlation", mutate: []mut{set("fdEnvelope.correlation.applicationId", "APP-2")}, code: CodePathCorrelation, fieldPath: "/fdEnvelope/correlation/applicationId"},
		{name: "correlation id absent", mutate: []mut{set("fdEnvelope.correlation.applicationId", nil)}, code: CodePathCorrelation, fieldPath: "/fdEnvelope/correlation/applicationId"},
		{name: "ttl expired", mutate: []mut{set("context.timestamp", testNow.Add(-36*time.Second).Format(time.RFC3339))}, code: CodeTTLExpired, fieldPath: "/context/ttl"},
		{name: "timestamp in the future", mutate: []mut{set("context.timestamp", testNow.Add(6*time.Second).Format(time.RFC3339))}, code: CodeTimestampInFuture, fieldPath: "/context/timestamp"},
		{name: "timestamp unparseable", mutate: []mut{set("context.timestamp", "yesterday")}, code: CodeContextInvalid, fieldPath: "/context/timestamp"},
		{name: "ttl unparseable", mutate: []mut{set("context.ttl", "30s")}, code: CodeContextInvalid, fieldPath: "/context/ttl"},
		{name: "message_id missing", mutate: []mut{set("context.message_id", nil)}, code: CodeContextInvalid, fieldPath: "/context/message_id"},
		{name: "bpp_uri without bpp_id", mutate: []mut{set("context.bpp_id", nil)}, code: CodeContextInvalid, fieldPath: "/context/bpp_id"},
		{name: "bpp pair missing", mutate: []mut{set("context.bpp_id", nil), set("context.bpp_uri", nil)}, code: CodeContextInvalid, fieldPath: "/context/bpp_id"},
		{name: "http bap_uri", mutate: []mut{set("context.bap_uri", "http://fd.exampledistributor.in/cb")}, code: CodeContextInvalid, fieldPath: "/context/bap_uri"},
		{name: "relative bpp_uri", mutate: []mut{set("context.bpp_uri", "/fd")}, code: CodeContextInvalid, fieldPath: "/context/bpp_uri"},
		{name: "broadcast with half a bpp pair", target: prefix + "/discovery/schemes/query",
			mutate: []mut{set("context.action", "discovery_schemes_query"), set("context.bpp_id", nil)}, code: CodeContextInvalid, fieldPath: "/context/bpp_uri"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tg := newNPGate(t)
			body := tt.raw
			if body == nil {
				m := confirmMsg("m-rule")
				for _, f := range tt.mutate {
					f(m)
				}
				body = m.bytes(t)
			}
			target := tt.target
			if target == "" {
				target = confirmTarget
			}
			b := expectNack(t, tg.serve(mustNotRun, npRequest(t, "POST", target, body, signOpts{})), http.StatusOK, tt.code)
			if b.Error.FieldPath != tt.fieldPath {
				t.Fatalf("fieldPath = %q, want %q", b.Error.FieldPath, tt.fieldPath)
			}
		})
	}
}

func TestNPTransportRulesAccept(t *testing.T) {
	tests := []struct {
		name   string
		target string
		m      msg
		cfg    func(*Config)
	}{
		{"refusal carries error instead of message", confirmTarget, func() msg {
			m := confirmMsg("m-err")
			delete(m, "message")
			m["error"] = map[string]any{"code": "FD-PRODUCT-0007"}
			return m
		}(), nil},
		{"non-correlation path param need not be repeated", prefix + "/consents/C-1/withdraw",
			newMsg("consent_withdraw", "INVESTOR_APP", "m-cw", nil), nil},
		{"broadcast without bpp", prefix + "/discovery/schemes/query", func() msg {
			m := newMsg("discovery_schemes_query", "INVESTOR_APP", "m-bc", nil)
			delete(m.ctx(), "bpp_id")
			delete(m.ctx(), "bpp_uri")
			return m
		}(), nil},
		{"http allowed locally", confirmTarget, func() msg {
			m := confirmMsg("m-http")
			m.ctx()["bap_uri"] = "http://localhost:3000/cb"
			return m
		}(), func(c *Config) { c.RequireHTTPS = false }},
		{"timestamp with offset", confirmTarget, func() msg {
			m := confirmMsg("m-tz")
			m.ctx()["timestamp"] = testNow.In(time.FixedZone("IST", 19800)).Format("2006-01-02T15:04:05.000-07:00")
			return m
		}(), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var muts []func(*Config)
			if tt.cfg != nil {
				muts = append(muts, tt.cfg)
			}
			tg := newNPGate(t, muts...)
			in := ackInner()
			rec := tg.serve(in, npRequest(t, "POST", tt.target, tt.m.bytes(t), signOpts{}))
			if rec.Code >= 300 || in.calls.Load() != 1 {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestNPBodyTooLarge(t *testing.T) {
	tg := newNPGate(t, func(c *Config) { c.MaxBodyBytes = 64 })
	body := confirmMsg("m-big").bytes(t)
	expectNack(t, tg.serve(mustNotRun, npRequest(t, "POST", confirmTarget, body, signOpts{})), http.StatusRequestEntityTooLarge, CodeBodyTooLarge)
	// Without a Content-Length the limit holds while reading.
	req := npRequest(t, "POST", confirmTarget, body, signOpts{})
	req.ContentLength = -1
	expectNack(t, tg.serve(mustNotRun, req), http.StatusRequestEntityTooLarge, CodeBodyTooLarge)
}

func TestNPAsyncRequestAccepted(t *testing.T) {
	tg := newNPGate(t)
	in := ackInner()
	in.header = map[string]string{"X-Pwb-Upstream-Ms": "3", "Set-Cookie": "mock_url=x"}
	body := confirmMsg("m-ok").bytes(t)
	req := npRequest(t, "POST", confirmTarget+"?trace=1", body, signOpts{})
	req.Header.Set("Cookie", "custom-response-body=e30=")
	req.Header.Set("X-Pwb-Fd-Route", "forged")
	req.Header.Set("Accept-Encoding", "gzip")
	rec := tg.serve(in, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	if !rec.Flushed {
		t.Fatal("the 202 was not flushed")
	}
	wantBody := `{"context":` + string(peekContext(body)) + `,"message":{"ack":{"status":"ACK"}}}`
	if rec.Body.String() != wantBody {
		t.Fatalf("body\n got %s\nwant %s", rec.Body.String(), wantBody)
	}
	if rec.Header().Get("X-Pwb-Upstream-Ms") != "" || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("internal headers reached the participant: %v", rec.Header())
	}

	got := in.seen()
	if got.method != "POST" || got.path != prefix+"/application_confirm" || got.requestURI != prefix+"/application_confirm" {
		t.Fatalf("inner saw %s %s (%s)", got.method, got.path, got.requestURI)
	}
	if !bytes.Equal(got.body, body) {
		t.Fatal("body changed on the way in")
	}
	h := got.header
	if h.Get("X-Pwb-Fd-Route") != "application_confirm" || h.Get("X-Pwb-Fd-Method") != "POST" ||
		h.Get("X-Pwb-Fd-Path") != "/applications/APP-1/confirm?trace=1" || h.Get("X-Pwb-Fd-Path-Params") != `{"applicationId":"APP-1"}` ||
		h.Get(fdroute.HeaderFdOrigin) != fdroute.OriginNP {
		t.Fatalf("FD headers: %v", h)
	}
	if h.Get("Cookie") != "" || h.Get("Accept-Encoding") != "" || h.Get("Authorization") == "" {
		t.Fatalf("inbound headers not cleaned: %v", h)
	}
	res := got.res
	if res == nil || res.Route.Action != "application_confirm" || res.CallerSubscriberID != appID || res.CallerUniqueKeyID != appUKID ||
		res.OriginalMethod != "POST" || res.OriginalRequestURI != confirmTarget+"?trace=1" || res.PathParams["applicationId"] != "APP-1" {
		t.Fatalf("resolution = %+v", res)
	}

	// The correlation ids are indexed for later reads.
	data, ok := tg.store.get(ObjectKey(domain, "applicationId", "APP-1"))
	if !ok {
		t.Fatal("applicationId not indexed")
	}
	var ref ObjectRef
	_ = json.Unmarshal(data, &ref)
	if ref != (ObjectRef{TransactionID: transactionID, BapID: appID, BapURI: appURI, BppID: issuerID, BppURI: issuerURI}) {
		t.Fatalf("index entry = %+v", ref)
	}
	if _, ok := tg.store.get(ObjectKey(domain, "applicationVersion", "3")); ok {
		t.Fatal("a non-string correlation value was indexed")
	}
}

func TestNPCallbackAndWebhookReceipt(t *testing.T) {
	for _, tt := range []struct{ action, path string }{
		{"on_application_confirm", "/on_application_confirm"},
		{"on_deposit_booked", "/on_deposit_booked"},
	} {
		t.Run(tt.action, func(t *testing.T) {
			tg := newTestGate(t, baseConfig(t, ModeNP, appPrefix))
			in := ackInner()
			m := newMsg(tt.action, "DEPOSIT_ISSUER", "m-cb", map[string]any{"applicationId": "APP-1", "depositId": "D-1"})
			body := m.bytes(t)
			rec := tg.serve(in, npRequest(t, "POST", appPrefix+tt.path, body, signOpts{sub: issuerID, ukid: issuerUKID, priv: issuerKeys.priv}))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
			}
			b := decodeAck(t, rec)
			if b.Message.Ack.Status != "ACK" || !bytes.Equal(b.Context, peekContext(body)) || b.Error != nil {
				t.Fatalf("body %s", rec.Body.String())
			}
			if in.seen().path != appPrefix+"/"+tt.action {
				t.Fatalf("inner path %s", in.seen().path)
			}
			if _, ok := tg.store.get(ObjectKey(domain, "depositId", "D-1")); !ok {
				t.Fatal("depositId not indexed")
			}
		})
	}
}

func TestNPInnerNack(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		code   string
		stored bool
	}{
		{"schema NACK, HTTP 200", 200, becknNACK, CodeWorkbenchRejected, true},
		{"workbench NACK, HTTP 400", 400, becknNACK, CodeWorkbenchRejected, true},
		{"sign NACK, HTTP 401", 401, becknNACK, CodeWorkbenchRejected, true},
		{"inner MESSAGE_ID_REUSED", 200, `{"message":{"ack":{"status":"NACK"}},"error":{"code":"MESSAGE_ID_REUSED","message":"[FROM ONIX WORKBENCH] message_id m-1 was already used"}}`, CodeMessageIDReused, true},
		{"inner EXPECTATION_EXPIRED", 400, `{"message":{"ack":{"status":"NACK"}},"error":{"code":"EXPECTATION_EXPIRED","message":"[FROM ONIX WORKBENCH] on_application_confirm was expected until 05:59"}}`, CodeExpectationExpired, true},
		{"internal NACK, HTTP 500", 500, `{"message":{"ack":{"status":"NACK"}},"error":{"code":"Internal Server Error","message":"Internal server error, MessageID: m-1"}}`, CodeInternal, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tg := newNPGate(t)
			in := &inner{status: tt.status, body: tt.body}
			body := confirmMsg("m-nack").bytes(t)
			b := expectNack(t, tg.serve(in, npRequest(t, "POST", confirmTarget, body, signOpts{})), http.StatusOK, tt.code)
			if !bytes.Equal(b.Context, peekContext(body)) {
				t.Fatalf("context not echoed: %s", b.Context)
			}
			switch tt.code {
			case CodeWorkbenchRejected:
				if b.Error.Message != "WORKBENCH_REJECTED: 30000: [FROM ONIX WORKBENCH] confirm must be an object" || b.Error.FieldPath != "/message/confirm" {
					t.Fatalf("error = %+v", b.Error)
				}
			case CodeMessageIDReused:
				if b.Error.Message != "MESSAGE_ID_REUSED: MESSAGE_ID_REUSED: [FROM ONIX WORKBENCH] message_id m-1 was already used" || b.Error.FieldPath != "/context/message_id" {
					t.Fatalf("error = %+v", b.Error)
				}
			case CodeExpectationExpired:
				if b.Error.Message != "EXPECTATION_EXPIRED: EXPECTATION_EXPIRED: [FROM ONIX WORKBENCH] on_application_confirm was expected until 05:59" {
					t.Fatalf("error = %+v", b.Error)
				}
			}
			_, stored := tg.store.get(ReplayKey(domain, appID, "application_confirm", "m-nack"))
			if stored != tt.stored {
				t.Fatalf("stored = %t, want %t", stored, tt.stored)
			}
		})
	}
}

func TestNPCounterpartyFailure(t *testing.T) {
	quote := newMsg("quote_request", "INVESTOR_APP", "", nil)
	for _, tt := range []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{"proxy 502", 502, "", CodeCounterpartyFailed},
		{"gateway timeout 504", 504, `<html>timeout</html>`, CodeCounterpartyFailed},
		{"empty 200", 200, "", CodeCounterpartyFailed},
		{"mock crashed 500", 500, `{"error":"boom"}`, CodeCounterpartyFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tg := newNPGate(t)
			in := &inner{status: tt.status, body: tt.body}
			quote.ctx()["message_id"] = "m-" + strings.ReplaceAll(tt.name, " ", "-")
			body := quote.bytes(t)
			expectNack(t, tg.serve(in, npRequest(t, "POST", prefix+"/quotes", body, signOpts{})), http.StatusOK, tt.code)
			// Not stored: a retry reaches the counterparty again.
			in2 := &inner{body: `{"context":{},"fdEnvelope":{},"message":{"quote":{}}}`}
			rec := tg.serve(in2, npRequest(t, "POST", prefix+"/quotes", body, signOpts{}))
			if rec.Code != http.StatusOK || in2.calls.Load() != 1 {
				t.Fatalf("retry not handled afresh: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
	// An asynchronous action whose inner chain fails without an ACK is INTERNAL.
	tg := newNPGate(t)
	expectNack(t, tg.serve(&inner{status: 500, body: `oops`}, npRequest(t, "POST", confirmTarget, confirmMsg("m-500").bytes(t), signOpts{})), http.StatusOK, CodeInternal)
}

func TestNPSyncPassThrough(t *testing.T) {
	tg := newNPGate(t)
	answer := newMsg("quote_request", "DEPOSIT_ISSUER", "m-ans", map[string]any{"quoteId": "Q-9"})
	answer["message"] = map[string]any{"quote": map[string]any{"rate": "7.10"}}
	answerBody := string(answer.bytes(t))
	in := &inner{body: answerBody, header: map[string]string{"X-Pwb-Fd-Route": "quote_request", "Set-Cookie": "a=b", "X-Request-Id": "r-1"}}
	rec := tg.serve(in, npRequest(t, "POST", prefix+"/quotes", newMsg("quote_request", "INVESTOR_APP", "m-q", nil).bytes(t), signOpts{}))
	if rec.Code != http.StatusOK || rec.Body.String() != answerBody {
		t.Fatalf("SYNC answer changed: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Pwb-Fd-Route") != "" || rec.Header().Get("Set-Cookie") != "" || rec.Header().Get("X-Request-Id") != "r-1" {
		t.Fatalf("response headers %v", rec.Header())
	}
	// The answer's correlation ids are indexed, so the quote can be read back.
	if _, ok := tg.store.get(ObjectKey(domain, "quoteId", "Q-9")); !ok {
		t.Fatal("quoteId from the SYNC answer not indexed")
	}

	// The mock's own FD NACK passes through, at HTTP 200.
	fdNack := `{"context":{"action":"quote_request"},"message":{"ack":{"status":"NACK"}},"error":{"code":"FD-PRODUCT-0007","class":"PRODUCT","message":"expired","obligedParty":"N-01 INVESTOR_APP","retryable":false}}`
	rec = tg.serve(&inner{status: 422, body: fdNack}, npRequest(t, "POST", prefix+"/quotes", newMsg("quote_request", "INVESTOR_APP", "m-q2", nil).bytes(t), signOpts{}))
	if rec.Code != http.StatusOK || rec.Body.String() != fdNack {
		t.Fatalf("FD NACK changed: %d %s", rec.Code, rec.Body.String())
	}
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNPGetRead(t *testing.T) {
	tg := newNPGate(t)
	tg.store.put(t, ObjectKey(domain, "depositId", "D/1"), ObjectRef{TransactionID: transactionID, BapID: "indexed.app", BapURI: appURI, BppID: issuerID, BppURI: issuerURI})
	events := `{"context":{"action":"deposit_events_read","transaction_id":"` + transactionID + `"},"fdEnvelope":{"correlation":{"depositId":"D/1","eventId":"E-1"}},"message":{"events":[]}}`
	in := &inner{body: events}
	target := prefix + "/deposits/D%2F1/events?since=2026-01-01&limit=5&limit=6"
	req := npRequest(t, "GET", target, nil, signOpts{})
	rec := tg.serve(in, req)
	if rec.Code != http.StatusOK || rec.Body.String() != events {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	got := in.seen()
	if got.method != "POST" || got.path != prefix+"/deposit_events_read" {
		t.Fatalf("inner saw %s %s", got.method, got.path)
	}
	if got.header.Get("X-Pwb-Fd-Method") != "GET" || got.header.Get("X-Pwb-Fd-Path") != "/deposits/D%2F1/events?since=2026-01-01&limit=5&limit=6" ||
		got.header.Get("X-Pwb-Fd-Route") != "deposit_events_read" || got.header.Get("X-Pwb-Fd-Path-Params") != `{"depositId":"D/1"}` {
		t.Fatalf("FD headers %v", got.header)
	}
	var b struct {
		Context    map[string]any `json:"context"`
		FdEnvelope map[string]any `json:"fdEnvelope"`
		Message    map[string]any `json:"message"`
	}
	if err := json.Unmarshal(got.body, &b); err != nil {
		t.Fatalf("internal body %s: %v", got.body, err)
	}
	c := b.Context
	msgID, _ := c["message_id"].(string)
	if !uuidRe.MatchString(msgID) {
		t.Fatalf("message_id %q is not a UUID", msgID)
	}
	delete(c, "message_id")
	wantCtx := map[string]any{
		"domain": domain, "location": map[string]any{"country": map[string]any{"code": "IND"}, "city": map[string]any{"code": "*"}},
		"version": version, "action": "deposit_events_read",
		"bap_id": appID, "bap_uri": appURI, "bpp_id": issuerID, "bpp_uri": issuerURI, "transaction_id": transactionID,
		"timestamp": "2027-04-19T06:00:00.000Z", "ttl": "PT30S",
	}
	if !reflect.DeepEqual(c, wantCtx) {
		t.Fatalf("context\n got %v\nwant %v", c, wantCtx)
	}
	wantEnv := map[string]any{"envelopeVersion": "1.0", "payloadVersion": "1.0", "mode": "SYNC", "originatorRole": "INVESTOR_APP",
		"correlation": map[string]any{"depositId": "D/1"}}
	if !reflect.DeepEqual(b.FdEnvelope, wantEnv) {
		t.Fatalf("envelope %v", b.FdEnvelope)
	}
	wantMsg := map[string]any{"pathParams": map[string]any{"depositId": "D/1"}, "query": map[string]any{"since": "2026-01-01", "limit": "5"}}
	if !reflect.DeepEqual(b.Message, wantMsg) {
		t.Fatalf("message %v", b.Message)
	}
	if got.res.OriginalMethod != "GET" || got.res.CallerSubscriberID != appID {
		t.Fatalf("resolution %+v", got.res)
	}
	// Ids in the read's answer are indexed too.
	if _, ok := tg.store.get(ObjectKey(domain, "eventId", "E-1")); !ok {
		t.Fatal("eventId from the answer not indexed")
	}
}

func TestNPGetUnknownObject(t *testing.T) {
	tg := newNPGate(t)
	rec := tg.serve(mustNotRun, npRequest(t, "GET", prefix+"/deposits/D-404", nil, signOpts{}))
	b := expectNack(t, rec, http.StatusOK, CodeObjectUnknown)
	if !strings.Contains(b.Error.Message, `depositId "D-404"`) {
		t.Fatalf("message %q", b.Error.Message)
	}
	var c map[string]any
	if err := json.Unmarshal(b.Context, &c); err != nil || c["action"] != "deposit_read" || c["bap_id"] != appID {
		t.Fatalf("context %s", b.Context)
	}
}

func TestNPGetSignatureFailureEchoesReadContext(t *testing.T) {
	tg := newNPGate(t)
	req := npRequest(t, "GET", prefix+"/deposits/D-1", nil, signOpts{body: []byte("{}")}) // digest over a body a GET does not have
	b := expectNack(t, tg.serve(mustNotRun, req), http.StatusOK, CodeSignatureInvalid)
	var c map[string]any
	if err := json.Unmarshal(b.Context, &c); err != nil || c["action"] != "deposit_read" {
		t.Fatalf("context %s", b.Context)
	}
}

func TestNPGetWithoutCorrelationParam(t *testing.T) {
	tg := newNPGate(t)
	in := &inner{body: `{"context":{},"fdEnvelope":{},"message":{"document":{}}}`}
	rec := tg.serve(in, npRequest(t, "GET", prefix+"/documents/DOC-1", nil, signOpts{}))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var b struct {
		Context map[string]any `json:"context"`
	}
	_ = json.Unmarshal(in.seen().body, &b)
	if tx, _ := b.Context["transaction_id"].(string); !uuidRe.MatchString(tx) || b.Context["bap_id"] != appID {
		t.Fatalf("context %v", b.Context)
	}
	if _, ok := b.Context["bpp_uri"]; ok {
		t.Fatalf("unknown endpoint invented: %v", b.Context)
	}
}

func TestNPIndexUnavailable(t *testing.T) {
	tg := newNPGate(t)
	tg.store.failing = errBoom
	expectNack(t, tg.serve(mustNotRun, npRequest(t, "GET", prefix+"/deposits/D-1", nil, signOpts{})), http.StatusOK, CodeInternal)
	expectNack(t, tg.serve(mustNotRun, npRequest(t, "POST", confirmTarget, confirmMsg("m-redis").bytes(t), signOpts{})), http.StatusOK, CodeInternal)
}

func TestNPEncodedInnerResponse(t *testing.T) {
	// A body fd-gate cannot read passes through untouched on a SYNC action.
	tg := newNPGate(t)
	in := &inner{body: "\x1f\x8b binary", header: map[string]string{"Content-Encoding": "gzip"}}
	rec := tg.serve(in, npRequest(t, "POST", prefix+"/quotes", newMsg("quote_request", "INVESTOR_APP", "m-gz", nil).bytes(t), signOpts{}))
	if rec.Code != http.StatusOK || rec.Body.String() != "\x1f\x8b binary" || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("%d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
}

func TestReadsCarryResolutionHeadersBeforeNext(t *testing.T) {
	// network-observability (next) must already see the FD headers on the request.
	tg := newNPGate(t)
	var seen http.Header
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write([]byte(becknACK))
	})
	tg.serve(next, npRequest(t, "POST", confirmTarget, confirmMsg("m-hdr").bytes(t), signOpts{}))
	for _, h := range []string{fdroute.HeaderFdRoute, fdroute.HeaderFdMethod, fdroute.HeaderFdPath, fdroute.HeaderFdPathParams} {
		if seen.Get(h) == "" {
			t.Fatalf("%s not set before next", h)
		}
	}
}

func TestReadRecorderIgnoresBody(t *testing.T) {
	// A GET's body (if any) is ignored: the signature covers the empty body.
	tg := newNPGate(t)
	tg.store.put(t, ObjectKey(domain, "depositId", "D-1"), ObjectRef{TransactionID: transactionID})
	req := npRequest(t, "GET", prefix+"/deposits/D-1", nil, signOpts{})
	req.Body = httptest.NewRequest("POST", "/", strings.NewReader("junk")).Body
	in := &inner{body: `{"message":{}}`}
	if rec := tg.serve(in, req); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
