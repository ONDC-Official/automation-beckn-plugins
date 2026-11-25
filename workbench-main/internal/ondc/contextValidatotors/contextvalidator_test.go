package contextvalidatotors

import (
	"context"
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
)

func strPtr(s string) *string { return &s }

func testProperties() apiservice.TransactionProperties {
	return apiservice.TransactionProperties{
		SupportedActions: map[string][]string{
			"null":       {"search"},
			"search":     {"on_search"},
			"on_search":  {"search", "select"},
			"select":     {"on_select"},
			"on_select":  {"select", "init"},
			"init":       {"on_init"},
			"on_init":    {"confirm"},
			"confirm":    {"on_confirm"},
			"on_confirm": {"on_status", "issue"},
			"on_status":  {"issue"},
			// IGM: issue may follow issue (a care relay, or a later update to the issue).
			"issue":           {"on_issue", "issue", "issue_status"},
			"on_issue":        {"issue", "issue_status"},
			"issue_status":    {"on_issue_status", "issue_status", "issue"},
			"on_issue_status": {"issue", "issue_status"},
		},
		APIProperties: map[string]apiservice.ActionProperties{
			"on_search":  {AsyncPredecessor: strPtr("search")},
			"on_select":  {AsyncPredecessor: strPtr("select")},
			"on_init":    {AsyncPredecessor: strPtr("init")},
			"on_confirm": {AsyncPredecessor: strPtr("confirm")},
			// on_status has no async predecessor (unsolicited), so it takes the sync path.
		},
	}
}

func apiEntry(action, messageID, timestamp string) cache.ApiData {
	return cache.ApiData{EntryType: "API", Action: action, MessageId: messageID, Timestamp: timestamp}
}

func transactionWith(entries ...cache.ApiData) *cache.TransactionCache {
	txn := &cache.TransactionCache{ApiList: []any{}}
	for _, e := range entries {
		txn.ApiList = append(txn.ApiList, e)
		txn.LatestAction = e.Action
		txn.LatestTimestamp = e.Timestamp
	}
	return txn
}

func payloadFor(action, messageID string) apiservice.PayloadEnvelope {
	var p apiservice.PayloadEnvelope
	p.Context.Action = action
	p.Context.MessageID = messageID
	p.Context.TransactionID = "txn-1"
	return p
}

func TestValidateAsyncContextDuplicateMessageId(t *testing.T) {
	searchFlow := []cache.ApiData{
		apiEntry("search", "m1", "2026-01-01T00:00:01Z"),
		apiEntry("on_search", "m1", "2026-01-01T00:00:02Z"),
	}
	selectFlow := append(append([]cache.ApiData{}, searchFlow...),
		apiEntry("select", "m2", "2026-01-01T00:00:03Z"),
		apiEntry("on_select", "m2", "2026-01-01T00:00:04Z"),
	)
	confirmFlow := append(append([]cache.ApiData{}, selectFlow...),
		apiEntry("init", "m3", "2026-01-01T00:00:05Z"),
		apiEntry("on_init", "m3", "2026-01-01T00:00:06Z"),
		apiEntry("confirm", "m4", "2026-01-01T00:00:07Z"),
		apiEntry("on_confirm", "m4", "2026-01-01T00:00:08Z"),
	)

	tests := []struct {
		name      string
		history   []cache.ApiData
		action    string
		messageID string
		wantValid bool
		wantErr   string
	}{
		{name: "first search", history: nil, action: "search", messageID: "m1", wantValid: true},
		{name: "repeat search with fresh message_id", history: searchFlow, action: "search", messageID: "m9", wantValid: true},
		{name: "repeat search reusing message_id", history: searchFlow, action: "search", messageID: "m1", wantErr: "Duplicate message_id"},
		{name: "select with fresh message_id", history: searchFlow, action: "select", messageID: "m2", wantValid: true},
		{name: "select reusing search message_id", history: searchFlow, action: "select", messageID: "m1", wantErr: "Duplicate message_id"},
		{name: "init with fresh message_id", history: selectFlow, action: "init", messageID: "m3", wantValid: true},
		{name: "init reusing select message_id", history: selectFlow, action: "init", messageID: "m2", wantErr: "Duplicate message_id"},
		{name: "init reusing search message_id", history: selectFlow, action: "init", messageID: "m1", wantErr: "Duplicate message_id"},
		{name: "unsupported action still reports flow error first", history: searchFlow, action: "init", messageID: "m1", wantErr: "not supported after"},
		{name: "async on_select matching select", history: selectFlow[:3], action: "on_select", messageID: "m2", wantValid: true},
		{name: "async second on_select", history: selectFlow, action: "on_select", messageID: "m2", wantErr: "Duplicate message_id"},
		{name: "sync-path callback sharing message_id is not flagged", history: confirmFlow, action: "on_status", messageID: "m4", wantValid: true},
	}

	cv := NewContextValidator(nil, testProperties(), apiservice.Receiver)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cv.validateAsyncContext(context.Background(), payloadFor(tt.action, tt.messageID), transactionWith(tt.history...), "", false)
			if got.Valid != tt.wantValid {
				t.Fatalf("Valid = %t, want %t (error: %q)", got.Valid, tt.wantValid, got.Error)
			}
			if tt.wantErr != "" && !strings.Contains(got.Error, tt.wantErr) {
				t.Fatalf("Error = %q, want it to contain %q", got.Error, tt.wantErr)
			}
			if !got.Valid && got.ForwardRequest {
				t.Fatalf("ForwardRequest = true for invalid result %q, want false", got.Error)
			}
		})
	}
}

func TestValidateAsyncContextCareRelay(t *testing.T) {
	confirmed := []cache.ApiData{
		apiEntry("confirm", "m4", "2026-01-01T00:00:07Z"),
		apiEntry("on_confirm", "m4", "2026-01-01T00:00:08Z"),
	}
	with := func(extra ...cache.ApiData) []cache.ApiData {
		return append(append([]cache.ApiData{}, confirmed...), extra...)
	}
	outboundIssue := apiEntry("issue", "i1", "2026-01-01T00:00:09Z")
	relayedIssue := apiEntry("issue", "i1", "2026-01-01T00:00:10Z")
	outboundIssueStatus := apiEntry("issue_status", "s1", "2026-01-01T00:00:11Z")

	tests := []struct {
		name       string
		moduleType apiservice.ModuleType
		useCare    bool
		history    []cache.ApiData
		action     string
		messageID  string
		wantValid  bool
	}{
		{name: "care relays our own issue back", moduleType: apiservice.Receiver, useCare: true, history: with(outboundIssue), action: "issue", messageID: "i1", wantValid: true},
		{name: "care relays our own issue_status back", moduleType: apiservice.Receiver, useCare: true, history: with(outboundIssue, outboundIssueStatus), action: "issue_status", messageID: "s1", wantValid: true},
		{name: "care issue with a fresh message_id", moduleType: apiservice.Receiver, useCare: true, history: with(outboundIssue), action: "issue", messageID: "i2", wantValid: true},
		{name: "care relays the same issue twice", moduleType: apiservice.Receiver, useCare: true, history: with(outboundIssue, relayedIssue), action: "issue", messageID: "i1"},
		{name: "care issue reusing another action's message_id", moduleType: apiservice.Receiver, useCare: true, history: with(outboundIssue), action: "issue", messageID: "m4"},
		{name: "same message_id on a different care action", moduleType: apiservice.Receiver, useCare: true, history: with(outboundIssue), action: "issue_status", messageID: "i1"},
		{name: "care off: repeated issue is a duplicate", moduleType: apiservice.Receiver, useCare: false, history: with(outboundIssue), action: "issue", messageID: "i1"},
		{name: "mock caller: repeated issue is a duplicate", moduleType: apiservice.Caller, useCare: true, history: with(outboundIssue), action: "issue", messageID: "i1"},
		{name: "non-care action keeps the strict check", moduleType: apiservice.Receiver, useCare: true, history: with(apiEntry("on_status", "u1", "2026-01-01T00:00:09Z")), action: "issue", messageID: "m4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cv := NewContextValidator(nil, testProperties(), tt.moduleType)
			got := cv.validateAsyncContext(context.Background(), payloadFor(tt.action, tt.messageID), transactionWith(tt.history...), "", tt.useCare)
			if got.Valid != tt.wantValid {
				t.Fatalf("Valid = %t, want %t (error: %q)", got.Valid, tt.wantValid, got.Error)
			}
			if !tt.wantValid && !strings.Contains(got.Error, "Duplicate message_id") {
				t.Fatalf("Error = %q, want a duplicate message_id error", got.Error)
			}
		})
	}
}

// mapCache is an in-memory definition.Cache.
type mapCache map[string]string

func (m mapCache) Get(_ context.Context, key string) (string, error) {
	v, ok := m[key]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}
func (m mapCache) Set(_ context.Context, key, value string, _ time.Duration) error {
	m[key] = value
	return nil
}
func (m mapCache) Delete(_ context.Context, key string) error { delete(m, key); return nil }
func (m mapCache) Clear(_ context.Context) error              { return nil }

// ValidateContext reads use_care from the request cookies; without the cookie, care is off.
func TestValidateContextUseCareCookie(t *testing.T) {
	const subscriber = "https://np.example.com/seller"
	history := transactionWith(
		apiEntry("confirm", "m4", "2026-01-01T00:00:07Z"),
		apiEntry("on_confirm", "m4", "2026-01-01T00:00:08Z"),
		apiEntry("issue", "i1", "2026-01-01T00:00:09Z"),
	)
	raw, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	txnCache := &transactioncache.Service{Cache: mapCache{}}
	if err := txnCache.Cache.Set(context.Background(), txnCache.CreateTransactionKey("txn-1", subscriber), string(raw), 0); err != nil {
		t.Fatal(err)
	}
	cv := NewContextValidator(txnCache, testProperties(), apiservice.Receiver)

	for _, tt := range []struct {
		name    string
		cookies map[string]string
		wantErr bool
	}{
		{name: "use_care=true accepts the relay", cookies: map[string]string{cookieUseCare: "true"}},
		{name: "use_care=false rejects it", cookies: map[string]string{cookieUseCare: "false"}, wantErr: true},
		{name: "no use_care cookie rejects it", cookies: map[string]string{}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/buyer/issue", nil)
			req.AddCookie(&http.Cookie{Name: cookieSubscriberURL, Value: subscriber})
			req.AddCookie(&http.Cookie{Name: cookieRequestOwner, Value: string(apiservice.BuyerNP)})
			for k, v := range tt.cookies {
				req.AddCookie(&http.Cookie{Name: k, Value: v})
			}
			payload := payloadFor("issue", "i1")
			payload.Context.Timestamp = "2026-01-01T00:00:10Z"
			err := cv.ValidateContext(context.Background(), req, payload, apiservice.PayloadRaw{"context": map[string]any{}})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %t", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "Duplicate message_id") {
				t.Fatalf("err = %v, want a duplicate message_id error", err)
			}
		})
	}
}
