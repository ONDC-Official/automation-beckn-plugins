package fdgate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdroute"
)

const prefix = "/api-service/ONDC:FIS14:FD/0.1.4/issuer"

func strp(s string) *string { return &s }

func writeTable(t *testing.T) string {
	t.Helper()
	req := func(action, method, path string, params []string) fdroute.Route {
		return fdroute.Route{
			Action: action, Kind: fdroute.KindRequest, Method: method, Path: path, PathParams: params,
			Mode: fdroute.ModeSync, Caller: fdroute.RoleInvestorApp, Callee: fdroute.RoleDepositIssuer,
			HasBody: method == "POST", OperationID: action,
		}
	}
	confirm := req("application_confirm", "POST", "/applications/{applicationId}/confirm", []string{"applicationId"})
	confirm.Mode = fdroute.ModeAsyncAckCallback
	confirm.Callback = strp("on_application_confirm")
	data, err := fdroute.Marshal(&fdroute.Table{
		FormatVersion: 1, Domain: "ONDC:FIS14:FD", Version: "0.1.4",
		Routes: []fdroute.Route{
			confirm,
			req("deposit_events_read", "GET", "/deposits/{depositId}/events", []string{"depositId"}),
			req("deposit_read", "GET", "/deposits/{depositId}", []string{"depositId"}),
			req("deposits_query", "POST", "/deposits/query", nil),
		},
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

func newMiddleware(t *testing.T, extra map[string]string) func(http.Handler) http.Handler {
	t.Helper()
	cfg := map[string]string{"routeTable": writeTable(t), "mode": "mock"}
	for k, v := range extra {
		cfg[k] = v
	}
	mw, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return mw
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) ErrorBody {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var b ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if b.Message.Ack.Status != "NACK" {
		t.Fatalf("ack status = %q", b.Message.Ack.Status)
	}
	return b
}

var mustNotRun = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	panic("next must not be called")
})

func TestParseConfig(t *testing.T) {
	c, err := ParseConfig(map[string]string{"routeTable": "/x.yaml", "mode": "np", "pathPrefix": prefix + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if c.PassThrough || c.Mode != ModeNP || c.PathPrefix != prefix {
		t.Fatalf("unexpected config %+v", c)
	}
	c, err = ParseConfig(map[string]string{"routeTable": "/x.yaml", "mode": "standalone", "passThrough": "true"})
	if err != nil || !c.PassThrough || c.Mode != ModeStandalone {
		t.Fatalf("unexpected config %+v, %v", c, err)
	}

	bad := []map[string]string{
		{"mode": "np"},
		{"routeTable": "/x.yaml"},
		{"routeTable": "/x.yaml", "mode": "bpp"},
		{"routeTable": "/x.yaml", "mode": "np", "passThrough": "sometimes"},
		{"routeTable": "/x.yaml", "mode": "np", "pathPrefix": "api-service"},
	}
	for _, cfg := range bad {
		if _, err := ParseConfig(cfg); err == nil {
			t.Fatalf("want error for %v", cfg)
		}
	}
}

func TestNewFailsOnBadTable(t *testing.T) {
	if _, err := New(context.Background(), map[string]string{"routeTable": filepath.Join(t.TempDir(), "absent.yaml"), "mode": "np"}); err == nil {
		t.Fatal("want error for missing table")
	}
	p := writeTable(t)
	data, _ := os.ReadFile(p)
	if err := os.WriteFile(p, []byte(strings.Replace(string(data), "deposits_query", "deposits_search", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), map[string]string{"routeTable": p, "mode": "np"}); err == nil || !strings.Contains(err.Error(), "tableHash") {
		t.Fatalf("want tableHash error, got %v", err)
	}
}

func TestUnknownRoute(t *testing.T) {
	h := newMiddleware(t, nil)(mustNotRun)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/nope", strings.NewReader("{}")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	want := `{"message":{"ack":{"status":"NACK"}},"error":{"code":"FD-PWB-0001","class":"PROTOCOL","message":"no FD 0.1.4 route for POST /nope","obligedParty":"N-01","retryable":false}}`
	if rec.Body.String() != want {
		t.Fatalf("body:\n got %s\nwant %s", rec.Body.String(), want)
	}
}

func TestWrongMethodIsNotFound(t *testing.T) {
	h := newMiddleware(t, nil)(mustNotRun)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/applications/A-1/confirm", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	b := decodeError(t, rec)
	if b.Error.Code != CodeRouteNotFound || !strings.Contains(b.Error.Message, "takes POST") {
		t.Fatalf("error = %+v", b.Error)
	}
}

func TestNotImplementedWithoutPassThrough(t *testing.T) {
	h := newMiddleware(t, nil)(mustNotRun)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/deposits/D-1/events", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d", rec.Code)
	}
	b := decodeError(t, rec)
	if b.Error.Code != CodeNotImplemented || b.Error.Class != "PROTOCOL" || b.Error.ObligedParty != "N-01" || b.Error.Retryable {
		t.Fatalf("error = %+v", b.Error)
	}
	if !strings.Contains(b.Error.Message, "deposit_events_read") {
		t.Fatalf("message does not name the action: %q", b.Error.Message)
	}
}

func TestPassThroughAttachesResolution(t *testing.T) {
	var got *fdroute.Resolution
	var gotHeader http.Header
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = fdroute.FromContext(r.Context())
		gotHeader = r.Header.Clone()
		w.WriteHeader(http.StatusAccepted)
	})
	h := newMiddleware(t, map[string]string{"passThrough": "true", "pathPrefix": prefix})(next)

	tests := []struct {
		name, method, target string
		action               string
		params               map[string]string
		query                url.Values
	}{
		{"literal beats param", "POST", prefix + "/deposits/query", "deposits_query", map[string]string{}, url.Values{}},
		{"GET with query", "GET", prefix + "/deposits/D%2F1/events?since=2026-01-01&limit=5", "deposit_events_read",
			map[string]string{"depositId": "D/1"}, url.Values{"since": {"2026-01-01"}, "limit": {"5"}}},
		{"encoded prefix", "POST", "/api-service/ONDC%3AFIS14%3AFD/0.1.4/issuer/applications/A-1/confirm", "application_confirm",
			map[string]string{"applicationId": "A-1"}, url.Values{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got = nil
			req := httptest.NewRequest(tt.method, tt.target, strings.NewReader(`{"a":1}`))
			req.Header.Set("Cookie", "session=1")
			req.Header.Set(fdroute.HeaderInternalKey, "forged")
			req.Header.Set("X-Pwb-Fd-Route", "forged")
			req.Header.Set("Authorization", "Signature keyId=\"a|b|ed25519\"")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
			}
			if got == nil {
				t.Fatal("no Resolution on the context")
			}
			if got.Route.Action != tt.action || !reflect.DeepEqual(got.PathParams, tt.params) || !reflect.DeepEqual(got.Query, tt.query) {
				t.Fatalf("resolution = %+v (route %s)", got, got.Route.Action)
			}
			if got.OriginalMethod != tt.method || got.OriginalRequestURI != tt.target {
				t.Fatalf("original = %s %s", got.OriginalMethod, got.OriginalRequestURI)
			}
			if gotHeader.Get("Cookie") != "" || gotHeader.Get(fdroute.HeaderInternalKey) != "" || gotHeader.Get("X-Pwb-Fd-Route") != "" {
				t.Fatalf("internal or cookie headers reached next: %v", gotHeader)
			}
			if gotHeader.Get("Authorization") == "" {
				t.Fatal("Authorization was stripped")
			}
		})
	}
}

func TestPathOutsidePrefix(t *testing.T) {
	h := newMiddleware(t, map[string]string{"passThrough": "true", "pathPrefix": prefix})(mustNotRun)
	for _, target := range []string{"/deposits/D-1/events", "/api-service/ONDC:FIS14:FD/0.1.4/investor/deposits/D-1/events", prefix} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d", target, rec.Code)
		}
		if b := decodeError(t, rec); b.Error.Code != CodeRouteNotFound {
			t.Fatalf("%s: code = %s", target, b.Error.Code)
		}
	}
}

func TestPanicRecovery(t *testing.T) {
	t.Run("before response", func(t *testing.T) {
		h := newMiddleware(t, map[string]string{"passThrough": "true"})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("boom")
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/deposits/D-1", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d", rec.Code)
		}
		if b := decodeError(t, rec); b.Error.Code != CodeInternal || b.Error.Class != "TECHNICAL" {
			t.Fatalf("error = %+v", b.Error)
		}
	})
	t.Run("after response started", func(t *testing.T) {
		h := newMiddleware(t, map[string]string{"passThrough": "true"})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("partial"))
			panic("boom")
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/deposits/D-1", nil))
		if rec.Code != http.StatusOK || rec.Body.String() != "partial" {
			t.Fatalf("response rewritten after it started: %d %q", rec.Code, rec.Body.String())
		}
	})
	t.Run("abort handler re-panics", func(t *testing.T) {
		h := newMiddleware(t, map[string]string{"passThrough": "true"})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic(http.ErrAbortHandler)
		}))
		defer func() {
			if p := recover(); p != http.ErrAbortHandler {
				t.Fatalf("recovered %v, want http.ErrAbortHandler", p)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/deposits/D-1", nil))
	})
}

func TestTrackingWriterUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	tw := &trackingWriter{ResponseWriter: rec}
	if err := http.NewResponseController(tw).Flush(); err != nil {
		t.Fatalf("Flush through trackingWriter: %v", err)
	}
	if !rec.Flushed {
		t.Fatal("underlying writer not flushed")
	}
}
