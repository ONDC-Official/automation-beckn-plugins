package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdroute"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// The plugin manager looks Provider up and type-asserts the symbol (a pointer to the
// variable) to definition.MiddlewareProvider.
var (
	_ definition.MiddlewareProvider = Provider
	_ definition.MiddlewareProvider = &Provider
)

func TestProviderNew(t *testing.T) {
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

	// The plugin manager passes adapter.yaml's values as strings. Redis is connected lazily,
	// so no server is needed to build the middleware.
	mw, err := Provider.New(context.Background(), map[string]string{"routeTable": p, "mode": "np", "addr": "127.0.0.1:1", "requireHTTPS": "true"})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	mw(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest("GET", "/deposits/D-1", nil))
	// An unsigned participant call is refused with an FD NACK at HTTP 200.
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"code":"FD-PWB-0010"`) {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	mw(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest("GET", "/nowhere", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path: status = %d", rec.Code)
	}

	if _, err := Provider.New(context.Background(), map[string]string{"mode": "np"}); err == nil {
		t.Fatal("want error without routeTable")
	}
	if _, err := Provider.New(context.Background(), map[string]string{"routeTable": p, "mode": "np"}); err == nil {
		t.Fatal("want error without addr in np mode")
	}
	if _, err := Provider.New(nil, map[string]string{"routeTable": p, "mode": "np"}); err == nil {
		t.Fatal("want error for nil context")
	}
}
