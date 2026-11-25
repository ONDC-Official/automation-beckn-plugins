package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdroute"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// The plugin manager looks Provider up and type-asserts the symbol (a pointer to the
// variable) to definition.TransportWrapperProvider.
var (
	_ definition.TransportWrapperProvider = Provider
	_ definition.TransportWrapperProvider = &Provider
)

type recordRT struct{ got *http.Request }

func (r *recordRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.got = req
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody, Request: req}, nil
}

func writeTable(t *testing.T) string {
	t.Helper()
	data, err := fdroute.Marshal(&fdroute.Table{
		FormatVersion: 1, Domain: "ONDC:FIS14:FD", Version: "0.1.4",
		Routes: []fdroute.Route{{
			Action: "deposit_read", Kind: fdroute.KindRequest, Method: "GET", Path: "/deposits/{depositId}",
			PathParams: []string{"depositId"}, Mode: fdroute.ModeSync, Caller: fdroute.RoleInvestorApp,
			Callee: fdroute.RoleDepositIssuer, OperationID: "readDeposit",
		}},
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

func TestProviderNew(t *testing.T) {
	for _, k := range []string{"SUBSCRIBER_ID", "UNIQUE_KEY_ID", "SIGNING_PRIVATE", "FD_REQUIRE_HTTPS"} {
		t.Setenv(k, "") // independent of the shell's environment
	}
	seed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	// The manager passes the YAML config's string values in a map[string]any.
	w, closer, err := Provider.New(context.Background(), map[string]any{
		"routeTable": writeTable(t), "internalHosts": "mock:3000",
		"subscriberId": "wb.example", "uniqueKeyId": "k1", "signingPrivateKey": seed, "requireHTTPS": "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if closer != nil {
		t.Fatal("unexpected closer")
	}
	base := &recordRT{}
	rt := w.Wrap(base)

	req := httptest.NewRequest("POST", "https://participant.example.com/fd/deposit_read", bytes.NewReader([]byte(`{}`)))
	req.RequestURI = ""
	req.Header.Set("X-Pwb-Internal-Key", "k")
	req.Header.Set(fdroute.HeaderFdRoute, "deposit_read")
	req.Header.Set(fdroute.HeaderFdPathParams, `{"depositId":"D-1"}`)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	got := base.got
	if got.Header.Get("X-Pwb-Internal-Key") != "" {
		t.Fatal("internal header leaked to participant")
	}
	if got.Method != "GET" || got.URL.String() != "https://participant.example.com/fd/deposits/D-1" || got.Header.Get("Authorization") == "" {
		t.Fatalf("sent %s %s %v", got.Method, got.URL, got.Header)
	}

	// Internal destinations pass unchanged.
	in := httptest.NewRequest("POST", "http://mock:3000/mock/x", nil)
	in.RequestURI = ""
	in.Header.Set("X-Pwb-Internal-Key", "k")
	if _, err := rt.RoundTrip(in); err != nil || base.got != in {
		t.Fatalf("internal request changed: %v", err)
	}

	for _, bad := range []map[string]any{
		{"internalHosts": 3, "routeTable": writeTable(t)},
		{"internalHosts": "mock:3000"}, // no routeTable
		{"routeTable": writeTable(t), "subscriberId": "only-this"},
	} {
		if _, _, err := Provider.New(context.Background(), bad); err == nil {
			t.Fatalf("want config error for %v", bad)
		}
	}
	if _, _, err := Provider.New(nil, map[string]any{}); err == nil {
		t.Fatal("want error for nil context")
	}
}
