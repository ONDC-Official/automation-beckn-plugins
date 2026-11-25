package receiver

import (
	"strings"
	"testing"
	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/apiservice"
	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/cache"
)

func TestNoExpectationMessage(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	live := now.Add(time.Minute).Format(time.RFC3339)
	gone := now.Add(-time.Minute).Format(time.RFC3339)

	request := func(owner apiservice.RequestOwner, action string) *apiservice.WorkbenchRequestData {
		data := &apiservice.WorkbenchRequestData{
			RequestOwner:  owner,
			SubscriberURL: "https://np.example/buyer",
			TransactionID: "txn-1",
		}
		data.BodyEnvelope.Context.Action = action
		return data
	}

	tests := []struct {
		name         string
		owner        apiservice.RequestOwner
		action       string
		hasSession   bool
		expectations []cache.Expectation
		want         []string
	}{
		{
			name:   "no session for the subscriber",
			owner:  apiservice.BuyerNP,
			action: "search",
			want: []string{
				"No active expectation found for transaction ID: txn-1 and Subscriber URL: https://np.example/buyer as a buyer_np.",
				"No workbench session exists for this subscriber URL.",
				"create a session on the workbench with this subscriber URL as a BAP (buyer app), start a flow there, then send search within 5 minutes.",
			},
		},
		{
			name:       "session but no flow started",
			owner:      apiservice.BuyerNP,
			action:     "search",
			hasSession: true,
			want:       []string{"Your session has no flow waiting for a request."},
		},
		{
			name:         "flow waiting for another action",
			owner:        apiservice.BuyerNP,
			action:       "select",
			hasSession:   true,
			expectations: []cache.Expectation{{ExpectedAction: "search", ExpireAt: live}},
			want:         []string{"Your session is waiting for search, not select.", "then send select within 5 minutes."},
		},
		{
			name:         "flow expired",
			owner:        apiservice.BuyerNP,
			action:       "search",
			hasSession:   true,
			expectations: []cache.Expectation{{ExpectedAction: "search", ExpireAt: gone}},
			want:         []string{"The flow you started expired before this request arrived."},
		},
		{
			name:   "seller app request",
			owner:  apiservice.SellerNP,
			action: "on_search",
			want:   []string{"as a seller_np.", "as a BPP (seller app)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := noExpectationMessage(request(tt.owner, tt.action), tt.hasSession, tt.expectations, now)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("message %q\ndoes not contain %q", got, want)
				}
			}
		})
	}
}
