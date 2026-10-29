package contextvalidatotors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/apiservice"
	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/cache"
	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/cache/transactioncache"
	"github.com/beckn-one/beckn-onix/pkg/model"
)

const (
	fdTxnID         = "txn-1"
	fdSubscriberURL = "https://np.example/investor-app"
	fdNPOwner       = "buyer_np"
)

var fdTestNow = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

type fdMemCache struct{ m map[string]string }

func (c *fdMemCache) Get(_ context.Context, key string) (string, error) {
	v, ok := c.m[key]
	if !ok {
		return "", errors.New("redis: nil")
	}
	return v, nil
}
func (c *fdMemCache) Set(_ context.Context, key, value string, _ time.Duration) error {
	c.m[key] = value
	return nil
}
func (c *fdMemCache) Delete(_ context.Context, key string) error { delete(c.m, key); return nil }
func (c *fdMemCache) Clear(_ context.Context) error              { c.m = map[string]string{}; return nil }

func fdProperties() apiservice.TransactionProperties {
	all := []string{"application_create", "on_application_create", "application_confirm", "on_application_confirm", "on_deposit_booked"}
	return apiservice.TransactionProperties{
		SupportedActions: map[string][]string{
			"null":                   {"application_create", "application_confirm", "on_deposit_booked"},
			"application_create":     all,
			"on_application_create":  all,
			"application_confirm":    all,
			"on_application_confirm": all,
			"on_deposit_booked":      all,
		},
		APIProperties: map[string]apiservice.ActionProperties{
			"application_create":     {},
			"on_application_create":  {AsyncPredecessor: strPtr("application_create"), TransactionPartner: []string{"application_create"}},
			"application_confirm":    {},
			"on_application_confirm": {AsyncPredecessor: strPtr("application_confirm"), TransactionPartner: []string{"application_confirm"}},
			"on_deposit_booked":      {}, // a webhook: no async predecessor
		},
	}
}

type fdFixture struct {
	t     *testing.T
	cv    *Validator
	svc   *transactioncache.Service
	key   string
	txn   *cache.TransactionCache
	owner string
}

func newFDFixture(t *testing.T, history ...cache.ApiData) *fdFixture {
	t.Helper()
	previous := fdNow
	fdNow = func() time.Time { return fdTestNow }
	t.Cleanup(func() { fdNow = previous })

	svc := &transactioncache.Service{Cache: &fdMemCache{m: map[string]string{}}}
	fx := &fdFixture{
		t:     t,
		cv:    NewContextValidator(svc, fdProperties()),
		svc:   svc,
		key:   svc.CreateTransactionKey(fdTxnID, fdSubscriberURL),
		txn:   &cache.TransactionCache{ApiList: []any{}, LatestTimestamp: time.Unix(0, 0).UTC().Format(time.RFC3339Nano)},
		owner: fdNPOwner,
	}
	for _, e := range history {
		fx.record(e)
	}
	fx.save()
	return fx
}

func (fx *fdFixture) save() {
	b, _ := json.Marshal(fx.txn)
	_ = fx.svc.Cache.Set(context.Background(), fx.key, string(b), 0)
}

// record appends an entry as the recorder does after a message has been answered.
func (fx *fdFixture) record(e cache.ApiData) {
	e.EntryType = "API"
	fx.txn.ApiList = append(fx.txn.ApiList, e)
	fx.txn.LatestAction = e.Action
	fx.txn.LatestTimestamp = e.Timestamp
	fx.save()
}

type fdMsg struct {
	action, messageID, inReplyTo string
	timestamp                    time.Time
	note                         string // changes the body without changing the context
}

func (m fdMsg) body() []byte {
	envelope := map[string]any{"envelopeVersion": "1.0", "payloadVersion": "0.1.4", "mode": "ASYNC_ACK_CALLBACK"}
	if m.inReplyTo != "" {
		envelope["inReplyTo"] = m.inReplyTo
	}
	ts := m.timestamp
	if ts.IsZero() {
		ts = fdTestNow
	}
	b, _ := json.Marshal(map[string]any{
		"context": map[string]any{
			"domain": "ONDC:FIS14:FD", "version": "0.1.4", "action": m.action,
			"bap_id": "app.example", "bap_uri": fdSubscriberURL, "bpp_id": "issuer.example", "bpp_uri": "https://wb.example/issuer",
			"transaction_id": fdTxnID, "message_id": m.messageID, "timestamp": ts.Format(time.RFC3339Nano), "ttl": "PT30S",
		},
		"fdEnvelope": envelope,
		"message":    map[string]any{"note": m.note},
	})
	return b
}

func (m fdMsg) entry() cache.ApiData {
	ttl := int64(30)
	ts := m.timestamp
	if ts.IsZero() {
		ts = fdTestNow
	}
	return cache.ApiData{
		Action: m.action, MessageId: m.messageID, InReplyTo: m.inReplyTo, TTL: &ttl,
		Timestamp: ts.Format(time.RFC3339Nano), RealTimestamp: ts.Format(time.RFC3339Nano),
	}
}

type fdResult struct {
	err error
	req *http.Request
}

func (fx *fdFixture) send(m fdMsg, fromNP bool) fdResult {
	fx.t.Helper()
	return fx.sendBody(m.body(), fromNP)
}

func (fx *fdFixture) sendBody(body []byte, fromNP bool) fdResult {
	fx.t.Helper()
	var env apiservice.PayloadEnvelope
	var raw apiservice.PayloadRaw
	if err := json.Unmarshal(body, &env); err != nil {
		fx.t.Fatalf("bad test body: %v", err)
	}
	_ = json.Unmarshal(body, &raw)
	req := httptest.NewRequest(http.MethodPost, "/issuer/"+env.Context.Action, nil)
	req.AddCookie(&http.Cookie{Name: cookieSubscriberURL, Value: fdSubscriberURL})
	req.AddCookie(&http.Cookie{Name: cookieRequestOwner, Value: fx.owner})
	err := fx.cv.ValidateFDContext(context.Background(), req, env, raw, FDMessage{Body: body, FromNP: fromNP})
	return fdResult{err: err, req: req}
}

func (r fdResult) cookie(name string) string {
	c, err := r.req.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

func (r fdResult) nack(t *testing.T) (code, message string) {
	t.Helper()
	var workbenchErr *model.WorkbenchErr
	if !errors.As(r.err, &workbenchErr) {
		t.Fatalf("error = %v (%T), want a workbench NACK", r.err, r.err)
	}
	if workbenchErr.Behavior != "NACK" {
		t.Fatalf("behavior = %s, want NACK", workbenchErr.Behavior)
	}
	return workbenchErr.Err.Code, workbenchErr.Err.Message
}

func (r fdResult) mustPass(t *testing.T) {
	t.Helper()
	if r.err != nil {
		t.Fatalf("ValidateFDContext() error = %v, want nil", r.err)
	}
	if r.cookie(cookieForwardRequest) != "true" {
		t.Fatalf("forward_request cookie = %q, want true", r.cookie(cookieForwardRequest))
	}
}

func (r fdResult) mustRefuse(t *testing.T, wantCode, wantMessage string) {
	t.Helper()
	code, message := r.nack(t)
	if code != wantCode || !strings.Contains(message, wantMessage) {
		t.Fatalf("NACK = %s %q, want %s containing %q", code, message, wantCode, wantMessage)
	}
}

var (
	createReq = fdMsg{action: "application_create", messageID: "m1", timestamp: fdTestNow.Add(-2 * time.Second)}
	createCb  = fdMsg{action: "on_application_create", messageID: "m2", inReplyTo: "m1"}
)

func TestFDCallbackPairsOnInReplyTo(t *testing.T) {
	fx := newFDFixture(t, createReq.entry())
	r := fx.send(createCb, true)
	r.mustPass(t)
	if r.cookie(cookieFDReplay) != "" {
		t.Fatalf("a first arrival must not be flagged as a replay")
	}
	if fx.svc.LoadFDBodyDigest(context.Background(), fx.key, "m2") != fdBodyDigest(createCb.body()) {
		t.Fatalf("the digest of a message let through must be kept")
	}
	if _, err := r.req.Cookie("custom-response-body"); err == nil {
		t.Fatalf("a callback within its request's TTL needs no NACK body")
	}
}

// Any request of the right action named by inReplyTo pairs, not only the latest one.
func TestFDCallbackPairsWithAnEarlierRequest(t *testing.T) {
	second := fdMsg{action: "application_create", messageID: "m3", timestamp: fdTestNow.Add(-time.Second)}
	fx := newFDFixture(t, createReq.entry(), second.entry())
	fx.send(createCb, true).mustPass(t)
}

func TestFDCallbackPairingRefusals(t *testing.T) {
	confirmReq := fdMsg{action: "application_confirm", messageID: "m5", timestamp: fdTestNow.Add(-time.Second)}
	tests := []struct {
		name     string
		history  []cache.ApiData
		callback fdMsg
		want     string
	}{
		{
			name:     "inReplyTo missing",
			history:  []cache.ApiData{createReq.entry()},
			callback: fdMsg{action: "on_application_create", messageID: "m2"},
			want:     "fdEnvelope.inReplyTo is missing",
		},
		{
			name:     "inReplyTo names no request",
			history:  []cache.ApiData{createReq.entry()},
			callback: fdMsg{action: "on_application_create", messageID: "m2", inReplyTo: "m9"},
			want:     "inReplyTo mismatch: no <application_create> with message_id m9",
		},
		{
			name:     "inReplyTo names a request of another action",
			history:  []cache.ApiData{createReq.entry(), confirmReq.entry()},
			callback: fdMsg{action: "on_application_create", messageID: "m2", inReplyTo: "m5"},
			want:     "names a <application_confirm>, but <on_application_create> answers a <application_create>",
		},
		{
			name:     "no request at all",
			history:  []cache.ApiData{confirmReq.entry()},
			callback: fdMsg{action: "on_application_create", messageID: "m2", inReplyTo: "m1"},
			want:     "application_create for on_application_create not found in the flow history",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFDFixture(t, tt.history...)
			r := fx.send(tt.callback, true)
			r.mustRefuse(t, "400", tt.want)
			if r.cookie(cookieForwardRequest) != "false" {
				t.Fatalf("forward_request = %q, want false", r.cookie(cookieForwardRequest))
			}
			if fx.svc.LoadFDBodyDigest(context.Background(), fx.key, tt.callback.messageID) != "" {
				t.Fatalf("a refused message must not be kept for replay")
			}
		})
	}
}

// A callback needs a fresh message_id: one reusing its request's id, as Beckn does, is refused.
func TestFDCallbackMustUseFreshMessageID(t *testing.T) {
	becknStyle := fdMsg{action: "on_application_create", messageID: "m1", inReplyTo: "m1"}

	t.Run("request let through by the workbench", func(t *testing.T) {
		fx := newFDFixture(t)
		fx.send(createReq, true).mustPass(t)
		fx.record(createReq.entry())
		fx.send(becknStyle, true).mustRefuse(t, "MESSAGE_ID_REUSED",
			"message_id m1 of <on_application_create> is already used in this transaction by <application_create>")
	})
	t.Run("request only in the history", func(t *testing.T) {
		fx := newFDFixture(t, createReq.entry())
		fx.send(becknStyle, true).mustRefuse(t, "MESSAGE_ID_REUSED",
			"<on_application_create> answers <application_create> with a fresh message_id and names the request in fdEnvelope.inReplyTo")
	})
	t.Run("from the mock too", func(t *testing.T) {
		fx := newFDFixture(t, createReq.entry())
		fx.send(becknStyle, false).mustRefuse(t, "MESSAGE_ID_REUSED", "already used in this transaction")
	})
}

// The mock re-sends a stored callback unchanged (X-Pwb-Fd-Replay: 1): it is let through and flagged,
// even though its timestamp is now old and its request's TTL has run out.
func TestFDReplayIsLetThroughAndFlagged(t *testing.T) {
	fx := newFDFixture(t, createReq.entry())
	fx.send(createCb, false).mustPass(t)
	fx.record(createCb.entry())

	fdNow = func() time.Time { return fdTestNow.Add(10 * time.Minute) }
	fx.record(fdMsg{action: "on_deposit_booked", messageID: "m7", timestamp: fdTestNow.Add(9 * time.Minute)}.entry())

	for _, fromNP := range []bool{false, true} {
		r := fx.send(createCb, fromNP)
		r.mustPass(t)
		if r.cookie(cookieFDReplay) != "true" {
			t.Fatalf("fromNP=%t: fd_replay cookie = %q, want true", fromNP, r.cookie(cookieFDReplay))
		}
		if _, err := r.req.Cookie("custom-response-body"); err == nil {
			t.Fatalf("fromNP=%t: a replay must not carry a NACK body", fromNP)
		}
	}
}

func TestFDDuplicateWithDifferentBodyIsRefused(t *testing.T) {
	changed := createCb
	changed.note = "a different body"

	t.Run("after the recorder has the first one", func(t *testing.T) {
		fx := newFDFixture(t, createReq.entry())
		fx.send(createCb, true).mustPass(t)
		fx.record(createCb.entry())
		fx.send(changed, true).mustRefuse(t, "MESSAGE_ID_REUSED",
			"message_id m2 of <on_application_create> is already used in this transaction by <on_application_create>")
	})
	t.Run("before the recorder has the first one", func(t *testing.T) {
		fx := newFDFixture(t, createReq.entry())
		fx.send(createCb, true).mustPass(t)
		fx.send(changed, true).mustRefuse(t, "MESSAGE_ID_REUSED", "message_id m2 of <on_application_create> is already used")
		// The same body still is a replay.
		r := fx.send(createCb, true)
		r.mustPass(t)
		if r.cookie(cookieFDReplay) != "true" {
			t.Fatalf("fd_replay cookie = %q, want true", r.cookie(cookieFDReplay))
		}
	})
	t.Run("a refused message has no digest, so even its exact repeat is a reuse", func(t *testing.T) {
		fx := newFDFixture(t, createReq.entry())
		refused := fdMsg{action: "on_application_create", messageID: "m2", inReplyTo: "m9"}
		fx.send(refused, true).mustRefuse(t, "400", "inReplyTo mismatch")
		fx.record(refused.entry())
		fx.send(refused, true).mustRefuse(t, "MESSAGE_ID_REUSED", "already used in this transaction")
	})
	t.Run("requests too", func(t *testing.T) {
		fx := newFDFixture(t)
		fx.send(createReq, true).mustPass(t)
		fx.record(createReq.entry())
		again := createReq
		again.note = "edited"
		fx.send(again, true).mustRefuse(t, "MESSAGE_ID_REUSED", "message_id m1 of <application_create>")
	})
}

// The TTL check finds the request through inReplyTo (a Beckn-style lookup by the callback's own
// message_id would find nothing and skip the check).
func TestFDCallbackTTLPairsOnInReplyTo(t *testing.T) {
	late := createReq
	late.timestamp = fdTestNow.Add(-60 * time.Second) // ttl is 30 s

	fx := newFDFixture(t, late.entry())
	r := fx.send(createCb, true)
	r.mustPass(t) // forwarded, as in Beckn
	if msg := r.cookie(cookieWorkbenchErrMsg); !strings.Contains(msg, "TTL expired for on_application_create: it answers <application_create> m1") {
		t.Fatalf("workbench_error_message = %q, want the TTL expiry", msg)
	}
	encoded := r.cookie("custom-response-body")
	decoded, _ := base64.StdEncoding.DecodeString(encoded)
	var nack map[string]any
	if err := json.Unmarshal(decoded, &nack); err != nil {
		t.Fatalf("custom-response-body = %q: %v", encoded, err)
	}
	if status := nack["message"].(map[string]any)["ack"].(map[string]any)["status"]; status != "NACK" {
		t.Fatalf("custom-response-body ack = %v, want NACK", status)
	}
	if ctx, _ := nack["context"].(map[string]any); ctx["message_id"] != "m2" {
		t.Fatalf("the FD NACK must echo the context, got %v", nack["context"])
	}
	if fx.svc.LoadFDBodyDigest(context.Background(), fx.key, "m2") == "" {
		t.Fatalf("a forwarded late callback is kept for replay")
	}

	// Beckn skips the TTL check when the request owner is not an NP; so does FD.
	fx = newFDFixture(t, late.entry())
	fx.owner = ""
	r = fx.send(createCb, true)
	r.mustPass(t)
	if r.cookie(cookieWorkbenchErrMsg) != "" {
		t.Fatalf("TTL checked for a non-NP request owner")
	}
}

func TestFDStaleTimestamp(t *testing.T) {
	latest := fdMsg{action: "application_create", messageID: "m1", timestamp: fdTestNow}
	tests := []struct {
		name   string
		offset time.Duration
		fromNP bool
		stale  bool
	}{
		{name: "NP, older by more than the skew", offset: -10 * time.Second, fromNP: true, stale: true},
		{name: "NP, older within the skew", offset: -3 * time.Second, fromNP: true},
		{name: "NP, newer", offset: time.Second, fromNP: true},
		{name: "mock, older", offset: -10 * time.Second, fromNP: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFDFixture(t, latest.entry())
			msg := fdMsg{action: "application_confirm", messageID: "m3", timestamp: fdTestNow.Add(tt.offset)}
			r := fx.send(msg, tt.fromNP)
			if tt.stale {
				r.mustRefuse(t, "400", "stale context timestamp")
				return
			}
			r.mustPass(t)
		})
	}

	t.Run("new transaction", func(t *testing.T) {
		fx := newFDFixture(t)
		fx.send(fdMsg{action: "application_create", messageID: "m1", timestamp: fdTestNow.Add(-time.Hour)}, true).mustPass(t)
	})
}

// Requests and webhooks carry no inReplyTo and follow the action order (Beckn's sync path).
func TestFDRequestsAndWebhooks(t *testing.T) {
	fx := newFDFixture(t)
	fx.send(fdMsg{action: "on_deposit_booked", messageID: "w1"}, true).mustPass(t)

	fx = newFDFixture(t, createReq.entry(), createCb.entry())
	fx.send(fdMsg{action: "on_deposit_booked", messageID: "w1"}, true).mustPass(t)
	fx.send(fdMsg{action: "application_confirm", messageID: "m4"}, true).mustPass(t)

	fx = newFDFixture(t)
	fx.send(fdMsg{action: "on_application_create", messageID: "m2", inReplyTo: "m1"}, true).
		mustRefuse(t, "400", "application_create for on_application_create not found in the flow history")
}

func TestFDMissingMessageID(t *testing.T) {
	fx := newFDFixture(t)
	fx.send(fdMsg{action: "application_create"}, true).mustRefuse(t, "400", "context.message_id is missing")
}

// The Beckn validator is untouched: an FD-shaped callback through ValidateContext still pairs
// on message_id, and inReplyTo means nothing to it.
func TestBecknValidateContextIgnoresInReplyTo(t *testing.T) {
	fx := newFDFixture(t, createReq.entry())
	body := createCb.body()
	var env apiservice.PayloadEnvelope
	var raw apiservice.PayloadRaw
	_ = json.Unmarshal(body, &env)
	_ = json.Unmarshal(body, &raw)
	req := httptest.NewRequest(http.MethodPost, "/on_application_create", nil)
	req.AddCookie(&http.Cookie{Name: cookieSubscriberURL, Value: fdSubscriberURL})
	req.AddCookie(&http.Cookie{Name: cookieRequestOwner, Value: fdNPOwner})
	err := fx.cv.ValidateContext(context.Background(), req, env, raw)
	if err == nil || !strings.Contains(err.Error(), "message_id mismatch between application_create and on_application_create") {
		t.Fatalf("ValidateContext() error = %v, want Beckn's message_id mismatch", err)
	}
	if _, cerr := req.Cookie(cookieFDReplay); cerr == nil {
		t.Fatalf("the Beckn path must never set fd_replay")
	}
}
