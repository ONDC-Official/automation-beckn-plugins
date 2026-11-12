package payloadutils

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/apiservice"
	"github.com/beckn-one/beckn-onix/pkg/model"
)

func TestWithWorkbenchPrefix(t *testing.T) {
	got := WithWorkbenchPrefix("action is missing in context")
	if want := "[FROM ONIX WORKBENCH] action is missing in context"; got != want {
		t.Fatalf("WithWorkbenchPrefix() = %q, want %q", got, want)
	}
	if again := WithWorkbenchPrefix(got); again != got {
		t.Fatalf("WithWorkbenchPrefix() on prefixed message = %q, want it unchanged", again)
	}
}

func TestErrorConstructorsArePrefixed(t *testing.T) {
	errs := map[string]error{
		"BadRequestNack":     NewBadRequestNackError("bad", nil),
		"InternalServerNack": NewInternalServerNackError("internal", nil),
		"BadRequestHTTP":     NewBadRequestHTTPError("bad"),
		"PreconditionHTTP":   NewPreconditionFailedHTTPError("precondition"),
		"InternalServerHTTP": NewInternalServerHTTPError("internal"),
		// Re-wrapping a workbench error's message must not double the prefix.
		"Rewrapped": NewInternalServerNackError(NewBadRequestHTTPError("bad").Error(), nil),
	}
	for name, err := range errs {
		msg := err.Error()
		if !strings.HasPrefix(msg, WorkbenchErrorPrefix+" ") || strings.Count(msg, WorkbenchErrorPrefix) != 1 {
			t.Errorf("%s: message %q, want exactly one %q prefix", name, msg, WorkbenchErrorPrefix)
		}
	}
}

func TestAsWorkbenchError(t *testing.T) {
	if AsWorkbenchError(nil) != nil {
		t.Fatalf("AsWorkbenchError(nil) should be nil")
	}

	nack := NewBadRequestNackError("bad", nil)
	if got := AsWorkbenchError(nack); got != nack {
		t.Fatalf("AsWorkbenchError() changed an existing workbench error")
	}

	got := AsWorkbenchError(errors.New("cache is nil"))
	var workbenchErr *model.WorkbenchErr
	if !errors.As(got, &workbenchErr) {
		t.Fatalf("AsWorkbenchError() = %T, want *model.WorkbenchErr", got)
	}
	if workbenchErr.Behavior != "HTTP" || workbenchErr.Err.Code != "500" {
		t.Fatalf("behavior/code = %s/%s, want HTTP/500", workbenchErr.Behavior, workbenchErr.Err.Code)
	}
	if want := "[FROM ONIX WORKBENCH] cache is nil"; workbenchErr.Err.Message != want {
		t.Fatalf("message = %q, want %q", workbenchErr.Err.Message, want)
	}

	// onix wraps step errors with %w; the message it sends must still be the prefixed one.
	wrapped := fmt.Errorf("ondc workbench receive step failed: %w", got)
	if !errors.As(wrapped, &workbenchErr) || !strings.HasPrefix(workbenchErr.BecknError().Message, WorkbenchErrorPrefix) {
		t.Fatalf("wrapped error lost the workbench prefix")
	}
}

func TestGetRequestDataCallerRoutesToParticipant(t *testing.T) {
	const (
		workbenchBuyer  = "https://workbench.example/api-service/ONDC:NTS10/2.1.0/buyer"
		workbenchSeller = "https://workbench.example/api-service/ONDC:NTS10/2.1.0/seller"
		npBuyer         = "https://buyer-np.example/ondc"
		npSeller        = "https://seller-np.example/ondc"
	)
	cases := []struct {
		name           string
		action         string
		bapURI, bppURI string
		subscriberURL  string
		wantRole       apiservice.ModuleRole
		wantTarget     string
		wantOwner      apiservice.RequestOwner
		wantID         string
	}{
		// The usual direction: requests from the BAP, callbacks from the BPP.
		{"buyer mock sends search to the seller", "search", workbenchBuyer, "", npSeller, apiservice.BAP, npSeller, apiservice.BuyerNP, "bap-id"},
		{"buyer mock sends select to the seller", "select", workbenchBuyer, npSeller, npSeller, apiservice.BAP, npSeller, apiservice.BuyerNP, "bap-id"},
		{"seller mock sends on_search to the buyer", "on_search", npBuyer, workbenchSeller, npBuyer, apiservice.BPP, npBuyer, apiservice.SellerNP, "bpp-id"},
		// RSF RECON_seller_initiated: the seller sends recon, the buyer answers on_recon.
		{"seller mock sends recon to the buyer", "recon", npBuyer, workbenchSeller, npBuyer, apiservice.BPP, npBuyer, apiservice.SellerNP, "bpp-id"},
		{"buyer mock sends on_recon to the seller", "on_recon", workbenchBuyer, npSeller, npSeller, apiservice.BAP, npSeller, apiservice.BuyerNP, "bap-id"},
		// RSF RECON_buyer_initiated keeps the usual direction.
		{"buyer mock sends recon to the seller", "recon", workbenchBuyer, npSeller, npSeller, apiservice.BAP, npSeller, apiservice.BuyerNP, "bap-id"},
		{"seller mock sends on_recon to the buyer", "on_recon", npBuyer, workbenchSeller, npBuyer, apiservice.BPP, npBuyer, apiservice.SellerNP, "bpp-id"},
		// Workbench to workbench: both uris are the workbench's own.
		{"loopback seller mock sends recon", "recon", workbenchBuyer, workbenchSeller, workbenchBuyer, apiservice.BPP, workbenchBuyer, apiservice.SellerNP, "bpp-id"},
		{"loopback buyer mock sends on_recon", "on_recon", workbenchBuyer, workbenchSeller, workbenchSeller, apiservice.BAP, workbenchSeller, apiservice.BuyerNP, "bap-id"},
		{"trailing slash still matches", "recon", npBuyer + "/", workbenchSeller, npBuyer, apiservice.BPP, npBuyer + "/", apiservice.SellerNP, "bpp-id"},
		// No match: the on_ prefix decides, as before.
		{"no subscriber_url falls back to the prefix", "recon", npBuyer, npSeller, "", apiservice.BAP, npSeller, apiservice.BuyerNP, "bap-id"},
		{"unknown subscriber_url falls back to the prefix", "on_recon", npBuyer, npSeller, "https://other.example", apiservice.BPP, npBuyer, apiservice.SellerNP, "bpp-id"},
		{"same uri on both sides falls back to the prefix", "recon", npBuyer, npBuyer, npBuyer, apiservice.BAP, npBuyer, apiservice.BuyerNP, "bap-id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var payload apiservice.PayloadEnvelope
			payload.Context.Action = c.action
			payload.Context.BapID, payload.Context.BapURI = "bap-id", c.bapURI
			payload.Context.BppID, payload.Context.BppURI = "bpp-id", c.bppURI
			u := url.URL{Path: "/api-service/ONDC:NTS10/2.1.0/mock/" + c.action}
			if c.subscriberURL != "" {
				u.RawQuery = url.Values{"subscriber_url": {c.subscriberURL}}.Encode()
			}

			owner, target, id, role, err := GetRequestData(payload, apiservice.Caller, apiservice.BAP, u)
			if err != nil {
				t.Fatalf("GetRequestData() error = %v", err)
			}
			if role != string(c.wantRole) || target != c.wantTarget || owner != c.wantOwner || id != c.wantID {
				t.Fatalf("GetRequestData() = role %s, target %s, owner %s, id %s; want %s, %s, %s, %s",
					role, target, owner, id, c.wantRole, c.wantTarget, c.wantOwner, c.wantID)
			}
		})
	}
}

func TestGetRequestDataReceiverIgnoresDirection(t *testing.T) {
	// Receivers are chosen by the path the NP called, whatever the action: a recon the
	// seller sends to the workbench's buyer endpoint is still keyed on its bpp_uri.
	var payload apiservice.PayloadEnvelope
	payload.Context.Action = "recon"
	payload.Context.BapURI = "https://workbench.example/buyer"
	payload.Context.BppURI = "https://seller-np.example/ondc"
	u := url.URL{RawQuery: url.Values{"subscriber_url": {"https://workbench.example/buyer"}}.Encode()}

	_, target, _, role, err := GetRequestData(payload, apiservice.Receiver, apiservice.BPP, u)
	if err != nil {
		t.Fatalf("GetRequestData() error = %v", err)
	}
	if role != string(apiservice.BPP) || target != payload.Context.BppURI {
		t.Fatalf("GetRequestData() = role %s, target %s; want BPP, %s", role, target, payload.Context.BppURI)
	}
}
