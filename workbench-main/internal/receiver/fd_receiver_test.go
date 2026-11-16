package receiver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/apiservice"
	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/cache"
	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/cache/sessioncache"
	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/cache/subscribercache"
	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/cache/transactioncache"
	"github.com/beckn-one/beckn-onix/pkg/model"
)

const npURL = "https://np.example/investor-app"

type memCache struct{ m map[string]string }

func (c *memCache) Get(_ context.Context, key string) (string, error) {
	v, ok := c.m[key]
	if !ok {
		return "", errors.New("redis: nil")
	}
	return v, nil
}
func (c *memCache) Set(_ context.Context, key, value string, _ time.Duration) error {
	c.m[key] = value
	return nil
}
func (c *memCache) Delete(_ context.Context, key string) error { delete(c.m, key); return nil }
func (c *memCache) Clear(_ context.Context) error              { c.m = map[string]string{}; return nil }

func expectation(session, action string, expireAt time.Time) cache.Expectation {
	return cache.Expectation{SessionId: session, FlowId: "flow-" + session, ExpectedAction: action, ExpireAt: expireAt.UTC().Format(time.RFC3339)}
}

func newReceiver(t *testing.T, fd bool, expectations ...cache.Expectation) (*WorkbenchRequestReceiver, *memCache) {
	t.Helper()
	mc := &memCache{m: map[string]string{}}
	for _, session := range []string{"s-live", "s-old"} {
		mc.m[session] = `{"transactionIds":[],"flowMap":{},"usecaseId":"U1","sessionDifficulty":{"protocolValidations":true}}`
	}
	if expectations != nil {
		b, _ := json.Marshal(cache.SubscriberCache{ActiveSessions: expectations})
		mc.m[npURL] = string(b)
	}
	r := NewWorkbenchRequestReceiver(
		transactioncache.Service{Cache: mc}, subscribercache.Service{Cache: mc}, sessioncache.Service{Cache: mc})
	if fd {
		r.EnableFD()
	}
	return r, mc
}

func npRequest(action string) *apiservice.WorkbenchRequestData {
	data := &apiservice.WorkbenchRequestData{
		BodyRaw:       apiservice.PayloadRaw{"context": map[string]any{"action": action, "message_id": "m1", "transaction_id": "txn-1"}},
		SubscriberURL: npURL,
		TransactionID: "txn-1",
		RequestOwner:  apiservice.BuyerNP,
	}
	data.BodyEnvelope.Context.Action = action
	data.BodyEnvelope.Context.TransactionID = "txn-1"
	return data
}

func workbenchErr(t *testing.T, err error) *model.WorkbenchErr {
	t.Helper()
	var wbErr *model.WorkbenchErr
	if !errors.As(err, &wbErr) {
		t.Fatalf("error = %v (%T), want a workbench error", err, err)
	}
	return wbErr
}

func TestFDExpiredExpectationIsNamed(t *testing.T) {
	now := time.Now()
	r, mc := newReceiver(t, true,
		expectation("s-old", "application_create", now.Add(-time.Minute)),
		expectation("s-other", "application_confirm", now.Add(5*time.Minute)))
	err := r.ReceiveFromNP(context.Background(), npRequest("application_create"))

	wbErr := workbenchErr(t, err)
	if wbErr.Behavior != "NACK" || wbErr.Err.Code != "EXPECTATION_EXPIRED" {
		t.Fatalf("got %s %s, want NACK EXPECTATION_EXPIRED", wbErr.Behavior, wbErr.Err.Code)
	}
	if !strings.Contains(wbErr.Err.Message, "expected <application_create> from "+npURL+" for flow flow-s-old of session s-old") {
		t.Fatalf("message = %q, want it to name the expired expectation", wbErr.Err.Message)
	}
	if ctx, _ := wbErr.Err.Context.(map[string]any); ctx["message_id"] != "m1" {
		t.Fatalf("NACK context = %v, want the request's", wbErr.Err.Context)
	}
	// Expired expectations are still dropped, as in Beckn; the live one stays.
	var sub cache.SubscriberCache
	_ = json.Unmarshal([]byte(mc.m[npURL]), &sub)
	if len(sub.ActiveSessions) != 1 || sub.ActiveSessions[0].SessionId != "s-other" {
		t.Fatalf("active expectations after = %+v, want only s-other", sub.ActiveSessions)
	}
}

// Beckn keeps its answer: the expired expectation is dropped silently and the request gets the
// generic 412.
func TestBecknExpiredExpectationUnchanged(t *testing.T) {
	r, _ := newReceiver(t, false, expectation("s-old", "search", time.Now().Add(-time.Minute)))
	wbErr := workbenchErr(t, r.ReceiveFromNP(context.Background(), npRequest("search")))
	if wbErr.Behavior != "HTTP" || wbErr.Err.Code != "412" || !strings.Contains(wbErr.Err.Message, "No active expectation found") {
		t.Fatalf("got %s %s %q, want HTTP 412 No active expectation found", wbErr.Behavior, wbErr.Err.Code, wbErr.Err.Message)
	}
}

func TestFDExpectationOtherCases(t *testing.T) {
	now := time.Now()

	t.Run("a live expectation wins over an expired one", func(t *testing.T) {
		r, _ := newReceiver(t, true,
			expectation("s-old", "application_create", now.Add(-time.Minute)),
			expectation("s-live", "application_create", now.Add(5*time.Minute)))
		data := npRequest("application_create")
		if err := r.ReceiveFromNP(context.Background(), data); err != nil {
			t.Fatalf("ReceiveFromNP() error = %v", err)
		}
		if data.SessionID != "s-live" || data.FlowID != "flow-s-live" {
			t.Fatalf("session/flow = %s/%s, want s-live/flow-s-live", data.SessionID, data.FlowID)
		}
	})
	t.Run("an expired expectation for another action is not this request's", func(t *testing.T) {
		r, _ := newReceiver(t, true, expectation("s-old", "application_confirm", now.Add(-time.Minute)))
		wbErr := workbenchErr(t, r.ReceiveFromNP(context.Background(), npRequest("application_create")))
		if wbErr.Err.Code != "412" {
			t.Fatalf("code = %s, want the generic 412", wbErr.Err.Code)
		}
	})
	t.Run("no expectations at all", func(t *testing.T) {
		r, _ := newReceiver(t, true)
		wbErr := workbenchErr(t, r.ReceiveFromNP(context.Background(), npRequest("application_create")))
		if wbErr.Err.Code != "412" {
			t.Fatalf("code = %s, want the generic 412", wbErr.Err.Code)
		}
	})
}
