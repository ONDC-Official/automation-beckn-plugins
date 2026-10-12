package contextvalidatotors

import (
	"context"
	"strings"
	"testing"

	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/apiservice"
	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/cache"
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
			"on_confirm": {"on_status"},
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

	cv := NewContextValidator(nil, testProperties())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cv.validateAsyncContext(context.Background(), payloadFor(tt.action, tt.messageID), transactionWith(tt.history...), "")
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
