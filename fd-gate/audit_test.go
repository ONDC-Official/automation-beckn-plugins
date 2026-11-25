package fdgate

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdroute"
)

type fakeAuditor struct {
	mu     sync.Mutex
	events []EdgeEvent
}

func (f *fakeAuditor) Audit(_ context.Context, ev EdgeEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}

func (f *fakeAuditor) all() []EdgeEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]EdgeEvent(nil), f.events...)
}

func auditedGate(t *testing.T) (*testGate, *fakeAuditor) {
	t.Helper()
	tg := newNPGate(t)
	a := &fakeAuditor{}
	tg.g.auditor = a
	return tg, a
}

func TestEdgeRefusalIsAudited(t *testing.T) {
	tg, a := auditedGate(t)
	body := confirmMsg("m-edge").bytes(t)
	// Signed over other bytes: a valid header on a tampered body.
	rec := tg.serve(ackInner(), npRequest(t, "POST", confirmTarget, body, signOpts{body: []byte(`{"other":1}`)}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	evs := a.all()
	if len(evs) != 1 {
		t.Fatalf("events %d", len(evs))
	}
	ev := evs[0]
	if ev.Kind != EdgeRefused || ev.Code != CodeSignatureInvalid || ev.Status != http.StatusOK ||
		ev.Route.Action != "application_confirm" || ev.Method != "POST" || string(ev.Body) != string(body) {
		t.Fatalf("event %+v", ev)
	}
	if string(ev.Response) != rec.Body.String() {
		t.Fatalf("audited answer differs from the one sent:\n%s\n%s", ev.Response, rec.Body.String())
	}
}

func TestEdgeReplayAndReuseAreAudited(t *testing.T) {
	tg, a := auditedGate(t)
	in := ackInner()
	body := confirmMsg("m-twice").bytes(t)
	tg.serve(in, npRequest(t, "POST", confirmTarget, body, signOpts{}))
	if n := len(a.all()); n != 0 {
		t.Fatalf("an accepted message is recorded by network-observability, not the edge: %d events", n)
	}
	tg.serve(in, npRequest(t, "POST", confirmTarget, body, signOpts{}))
	other := confirmMsg("m-twice")
	other["message"] = map[string]any{"changed": true}
	tg.serve(in, npRequest(t, "POST", confirmTarget, other.bytes(t), signOpts{}))
	evs := a.all()
	if len(evs) != 2 || evs[0].Kind != EdgeReplay || evs[0].Status != http.StatusAccepted ||
		evs[1].Kind != EdgeRefused || evs[1].Code != CodeMessageIDReused {
		t.Fatalf("events %+v", evs)
	}
}

func TestEdgeAuditPayload(t *testing.T) {
	tg := newNPGate(t)
	route, ok := tg.g.table.ByAction("application_confirm")
	if !ok {
		t.Fatal("no route")
	}
	ev := EdgeEvent{Kind: EdgeRefused, Code: CodeTTLExpired, Route: route, Method: "POST",
		Path: "/applications/APP-1/confirm", Body: confirmMsg("m-p").bytes(t), Status: 200, Response: []byte(`{"message":{"ack":{"status":"NACK"}}}`)}
	raw, ok := edgeAuditPayload(ev, "pid-1")
	if !ok {
		t.Fatal("no payload")
	}
	var p struct {
		RequestBody    map[string]any `json:"requestBody"`
		ResponseBody   map[string]any `json:"responseBody"`
		AdditionalData map[string]any `json:"additionalData"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	ad := p.AdditionalData
	if ad["payload_id"] != "pid-1" || ad["transaction_id"] != transactionID || ad["subscriber_url"] != appURI ||
		ad["action"] != "application_confirm" || ad["message_id"] != "m-p" || ad["fd_edge"] != CodeTTLExpired ||
		ad["fd_replay"] != false || ad["status_code"] != float64(200) {
		t.Fatalf("additionalData %v", ad)
	}
	h := ad["request_headers"].(map[string]any)
	if origin := h["x-pwb-fd-origin"].([]any); origin[0] != fdroute.OriginNP {
		t.Fatalf("headers %v", h)
	}
	if p.RequestBody["context"] == nil || p.ResponseBody["message"] == nil {
		t.Fatalf("bodies %v %v", p.RequestBody, p.ResponseBody)
	}
	// A body that is not JSON names no transaction: nothing to file.
	ev.Body = []byte("not json")
	if _, ok := edgeAuditPayload(ev, "pid-2"); ok {
		t.Fatal("payload for a non-JSON body")
	}
}

func TestRecorderTargetConfig(t *testing.T) {
	base := map[string]string{"routeTable": writeTable(t), "mode": "np", "addr": "redis:6379"}
	c, err := parseConfig(base, envOf(map[string]string{EnvRecorderGRPC: "recorder-service:8089"}))
	if err != nil || c.RecorderTarget != "recorder-service:8089" {
		t.Fatalf("env fallback: %v %q", err, c.RecorderTarget)
	}
	withKey := map[string]string{"recorderTarget": "rec:1"}
	for k, v := range base {
		withKey[k] = v
	}
	if c, _ = parseConfig(withKey, envOf(map[string]string{EnvRecorderGRPC: "recorder-service:8089"})); c.RecorderTarget != "rec:1" {
		t.Fatalf("config key: %q", c.RecorderTarget)
	}
	if c, _ = parseConfig(base, envOf(nil)); c.RecorderTarget != "" {
		t.Fatalf("default: %q", c.RecorderTarget)
	}
}
