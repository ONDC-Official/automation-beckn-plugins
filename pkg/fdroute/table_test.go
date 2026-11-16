package fdroute

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The FD-ROUTE-TABLE.md test vector.
const (
	vectorRoutes    = `[{"action":"deposit_events_read","kind":"request","method":"GET","path":"/deposits/{depositId}/events","pathParams":["depositId"],"mode":"SYNC","caller":"INVESTOR_APP","callee":"DEPOSIT_ISSUER","callback":null,"inReplyTo":null,"hasBody":false,"versionGuard":null,"sla":"P95 ≤ 2s","requirements":["FD-LCY-01"],"operationId":"readDepositEvents"}]`
	vectorCanonical = `[{"action":"deposit_events_read","callback":null,"callee":"DEPOSIT_ISSUER","caller":"INVESTOR_APP","hasBody":false,"inReplyTo":null,"kind":"request","method":"GET","mode":"SYNC","operationId":"readDepositEvents","path":"/deposits/{depositId}/events","pathParams":["depositId"],"requirements":["FD-LCY-01"],"sla":"P95 ≤ 2s","versionGuard":null}]`
	vectorHash      = "9584937ce11f12d3bb38a9e7d30ac81d29918aec87b87bce5f8488b7241484d7"
)

func strp(s string) *string { return &s }

func TestCanonicalVector(t *testing.T) {
	var routes any
	if err := json.Unmarshal([]byte(vectorRoutes), &routes); err != nil {
		t.Fatal(err)
	}
	c, err := Canonical(routes)
	if err != nil {
		t.Fatal(err)
	}
	if string(c) != vectorCanonical {
		t.Fatalf("canonical mismatch:\n got %s\nwant %s", c, vectorCanonical)
	}
	h, err := ComputeHash(routes)
	if err != nil {
		t.Fatal(err)
	}
	if h != vectorHash {
		t.Fatalf("hash = %s, want %s", h, vectorHash)
	}
}

func TestHashRoutesVector(t *testing.T) {
	h, err := HashRoutes([]Route{{
		Action: "deposit_events_read", Kind: KindRequest, Method: "GET",
		Path: "/deposits/{depositId}/events", PathParams: []string{"depositId"},
		Mode: ModeSync, Caller: RoleInvestorApp, Callee: RoleDepositIssuer,
		HasBody: false, SLA: strp("P95 ≤ 2s"), Requirements: []string{"FD-LCY-01"},
		OperationID: "readDepositEvents",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if h != vectorHash {
		t.Fatalf("hash = %s, want %s", h, vectorHash)
	}
}

func TestLoadVectorFiles(t *testing.T) {
	for _, f := range []string{"testdata/vector.yaml", "testdata/vector_wrapped.yaml"} {
		t.Run(f, func(t *testing.T) {
			tbl, err := Load(f)
			if err != nil {
				t.Fatal(err)
			}
			if tbl.Domain != "ONDC:FIS14:FD" || tbl.Version != "0.1.4" || tbl.FormatVersion != 1 {
				t.Fatalf("unexpected header %+v", tbl)
			}
			r, ok := tbl.ByAction("deposit_events_read")
			if !ok {
				t.Fatal("ByAction: not found")
			}
			if r.SLA == nil || *r.SLA != "P95 ≤ 2s" || r.Callback != nil || r.HasBody {
				t.Fatalf("route decoded wrongly: %+v", r)
			}
			if _, ok := tbl.ByAction("nope"); ok {
				t.Fatal("ByAction found an unknown action")
			}
		})
	}
}

func TestLoadHashMismatch(t *testing.T) {
	data, err := os.ReadFile("testdata/vector.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Same declared hash, different routes.
	tampered := strings.Replace(string(data), "P95 ≤ 2s", "P95 ≤ 3s", 1)
	_, err = Parse([]byte(tampered))
	if err == nil || !strings.Contains(err.Error(), "tableHash mismatch") {
		t.Fatalf("want tableHash mismatch, got %v", err)
	}
	// Missing hash.
	missing := strings.Replace(string(data), "tableHash: "+vectorHash, "", 1)
	if _, err := Parse([]byte(missing)); err == nil {
		t.Fatal("want error for missing tableHash")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("want error")
	}
}

// fixtureRoutes is a slice of FD's table that exercises every matching rule.
func fixtureRoutes() []Route {
	req := func(action, method, path string, params []string, hasBody bool) Route {
		return Route{
			Action: action, Kind: KindRequest, Method: method, Path: path, PathParams: params,
			Mode: ModeSync, Caller: RoleInvestorApp, Callee: RoleDepositIssuer, HasBody: hasBody,
			Requirements: []string{}, OperationID: action,
		}
	}
	confirm := req("application_confirm", "POST", "/applications/{applicationId}/confirm", []string{"applicationId"}, true)
	confirm.Mode = ModeAsyncAckCallback
	confirm.Callback = strp("on_application_confirm")
	confirm.VersionGuard = strp("application")
	return []Route{
		confirm,
		req("deposit_events_read", "GET", "/deposits/{depositId}/events", []string{"depositId"}, false),
		req("deposit_read", "GET", "/deposits/{depositId}", []string{"depositId"}, false),
		req("deposit_update", "POST", "/deposits/{depositId}", []string{"depositId"}, true),
		req("deposit_withdraw", "POST", "/deposits/{depositId}/withdraw", []string{"depositId"}, true),
		req("deposits_query", "POST", "/deposits/query", nil, true),
		{
			Action: "on_application_confirm", Kind: KindCallback, Method: "POST", Path: "/on_application_confirm",
			Mode: ModeAsyncAckCallback, Caller: RoleDepositIssuer, Callee: RoleInvestorApp,
			InReplyTo: strp("application_confirm"), HasBody: true, OperationID: "onApplicationConfirm",
		},
		{
			Action: "on_deposit_booked", Kind: KindWebhook, Method: "POST", Path: "/on_deposit_booked",
			Mode: ModeWebhook, Caller: RoleDepositIssuer, Callee: RoleInvestorApp, HasBody: true,
			OperationID: "onDepositBooked",
		},
		req("payment_instruction_read", "GET", "/applications/{applicationId}/payment-instruction", []string{"applicationId"}, false),
		req("payment_instruction_submit", "POST", "/applications/{applicationId}/payment-instruction", []string{"applicationId"}, true),
	}
}

func writeTable(t *testing.T, routes []Route) string {
	t.Helper()
	data, err := Marshal(&Table{FormatVersion: 1, Domain: "ONDC:FIS14:FD", Version: "0.1.4", Routes: routes})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "fd_routes.yaml")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func loadFixture(t *testing.T) *Table {
	t.Helper()
	tbl, err := Load(writeTable(t, fixtureRoutes()))
	if err != nil {
		t.Fatal(err)
	}
	return tbl
}

func TestMatch(t *testing.T) {
	tbl := loadFixture(t)
	tests := []struct {
		method, path string
		action       string
		params       map[string]string
	}{
		// Literal beats {param}.
		{"POST", "/deposits/query", "deposits_query", map[string]string{}},
		{"GET", "/deposits/query", "deposit_read", map[string]string{"depositId": "query"}},
		{"POST", "/deposits/D-1", "deposit_update", map[string]string{"depositId": "D-1"}},
		{"GET", "/deposits/D-1", "deposit_read", map[string]string{"depositId": "D-1"}},
		// The literal branch fails deeper down; the {param} branch is tried next.
		{"POST", "/deposits/query/withdraw", "deposit_withdraw", map[string]string{"depositId": "query"}},
		{"GET", "/deposits/D-1/events", "deposit_events_read", map[string]string{"depositId": "D-1"}},
		// Same template, different methods.
		{"GET", "/applications/A-9/payment-instruction", "payment_instruction_read", map[string]string{"applicationId": "A-9"}},
		{"POST", "/applications/A-9/payment-instruction", "payment_instruction_submit", map[string]string{"applicationId": "A-9"}},
		{"POST", "/applications/A-9/confirm", "application_confirm", map[string]string{"applicationId": "A-9"}},
		// Values are percent-decoded; an encoded slash stays inside its segment.
		{"GET", "/deposits/a%2Fb%20c", "deposit_read", map[string]string{"depositId": "a/b c"}},
		// Method is case-insensitive; a query string is ignored.
		{"get", "/deposits/D-1/events?since=2026-01-01", "deposit_events_read", map[string]string{"depositId": "D-1"}},
		{"POST", "/on_application_confirm", "on_application_confirm", map[string]string{}},
		{"POST", "/on_deposit_booked", "on_deposit_booked", map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			r, params, err := tbl.Match(tt.method, tt.path)
			if err != nil {
				t.Fatal(err)
			}
			if r.Action != tt.action {
				t.Fatalf("action = %s, want %s", r.Action, tt.action)
			}
			if !reflect.DeepEqual(params, tt.params) {
				t.Fatalf("params = %v, want %v", params, tt.params)
			}
		})
	}
}

func TestMatchNoRoute(t *testing.T) {
	tbl := loadFixture(t)
	tests := []struct {
		method, path string
		allowed      []string
	}{
		{"GET", "/unknown", nil},
		{"POST", "/deposits", nil},
		{"GET", "/deposits/", nil},            // {param} needs a non-empty segment
		{"GET", "/deposits//events", nil},     // ditto
		{"GET", "/deposits/D-1/events/", nil}, // trailing slash is a different path
		{"GET", "/deposits/%zz", nil},         // invalid escape
		{"GET", "deposits/D-1", nil},          // not absolute
		{"GET", "/", nil},
		{"DELETE", "/deposits/D-1", []string{"GET", "POST"}},
		{"GET", "/applications/A-9/confirm", []string{"POST"}},
		{"POST", "/deposits/D-1/events", []string{"GET"}},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			r, _, err := tbl.Match(tt.method, tt.path)
			if err == nil {
				t.Fatalf("matched %s", r.Action)
			}
			if !errors.Is(err, ErrNoRoute) {
				t.Fatalf("error %v is not ErrNoRoute", err)
			}
			var nre *NoRouteError
			if !errors.As(err, &nre) {
				t.Fatalf("error %T is not *NoRouteError", err)
			}
			if !reflect.DeepEqual(nre.Allowed, tt.allowed) {
				t.Fatalf("allowed = %v, want %v", nre.Allowed, tt.allowed)
			}
		})
	}
}

func TestLoadAmbiguous(t *testing.T) {
	routes := fixtureRoutes()
	dup := routes[2] // GET /deposits/{depositId}
	dup.Action = "deposit_read_again"
	dup.Path = "/deposits/{id}"
	dup.PathParams = []string{"id"}
	_, err := Load(writeTable(t, append(routes, dup)))
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("want ambiguity error, got %v", err)
	}

	// Same template under a different method is fine.
	ok := routes[2]
	ok.Action = "deposit_read_post"
	ok.Method = "POST"
	ok.Path = "/deposits/{id}/read"
	ok.PathParams = []string{"id"}
	if _, err := Load(writeTable(t, append(fixtureRoutes(), ok))); err != nil {
		t.Fatal(err)
	}
}

func TestLoadValidation(t *testing.T) {
	tests := map[string]func(r *Route){
		"pathParams mismatch":   func(r *Route) { r.PathParams = []string{"other"} },
		"pathParams missing":    func(r *Route) { r.PathParams = nil },
		"invalid action":        func(r *Route) { r.Action = "Bad-Action" },
		"invalid method":        func(r *Route) { r.Method = "PUT" },
		"invalid kind":          func(r *Route) { r.Kind = "event" },
		"invalid mode":          func(r *Route) { r.Mode = "ASYNC" },
		"prefixed caller":       func(r *Route) { r.Caller = "N-01 INVESTOR_APP" },
		"invalid callee":        func(r *Route) { r.Callee = "BANK" },
		"partial param segment": func(r *Route) { r.Path = "/deposits/x{depositId}" },
		"empty segment":         func(r *Route) { r.Path = "/deposits//{depositId}" },
		"relative path":         func(r *Route) { r.Path = "deposits/{depositId}" },
		"duplicate param": func(r *Route) {
			r.Path = "/deposits/{depositId}/{depositId}"
			r.PathParams = []string{"depositId", "depositId"}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			routes := fixtureRoutes()
			mutate(&routes[2])
			if _, err := Load(writeTable(t, routes)); err == nil {
				t.Fatal("want error")
			}
		})
	}
	t.Run("duplicate action", func(t *testing.T) {
		routes := fixtureRoutes()
		routes[3].Action = routes[2].Action
		if _, err := Load(writeTable(t, routes)); err == nil || !strings.Contains(err.Error(), "duplicate action") {
			t.Fatalf("want duplicate action error, got %v", err)
		}
	})
	t.Run("no routes", func(t *testing.T) {
		if _, err := Load(writeTable(t, []Route{})); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("format version", func(t *testing.T) {
		data, _ := os.ReadFile("testdata/vector.yaml")
		if _, err := Parse([]byte(strings.Replace(string(data), "formatVersion: 1", "formatVersion: 2", 1))); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestMarshalRoundTrip(t *testing.T) {
	tbl := loadFixture(t)
	want := fixtureRoutes()
	if len(tbl.Routes) != len(want) {
		t.Fatalf("got %d routes", len(tbl.Routes))
	}
	r, _ := tbl.ByAction("application_confirm")
	if r.Callback == nil || *r.Callback != "on_application_confirm" || r.VersionGuard == nil || r.Mode != ModeAsyncAckCallback {
		t.Fatalf("round trip lost fields: %+v", r)
	}
	h, err := HashRoutes(tbl.Routes)
	if err != nil || h != tbl.TableHash {
		t.Fatalf("HashRoutes(loaded) = %s, %v; table says %s", h, err, tbl.TableHash)
	}
}

func TestCanonicalLineSeparators(t *testing.T) {
	c, err := Canonical(map[string]any{"b": "x\u2028y\u2029z", "a": `\u2028`, "c": "<&>"})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"a\":\"\\\\u2028\",\"b\":\"x\u2028y\u2029z\",\"c\":\"<&>\"}"
	if string(c) != want {
		t.Fatalf("got %s, want %s", c, want)
	}
}

func TestStripInternal(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderFdRoute, "x")
	h.Set(HeaderInternalKey, "secret")
	h.Set("X-Pwb-Anything-Else", "y")
	h["x-pwb-lowercase"] = []string{"z"} // non-canonical key
	h.Set("X-Pwbnot-Internal", "keep")
	h.Set("Authorization", "keep")
	StripInternal(h)
	if len(h) != 2 || h.Get("Authorization") != "keep" || h.Get("X-Pwbnot-Internal") != "keep" {
		t.Fatalf("unexpected headers after strip: %v", h)
	}
	for _, name := range []string{HeaderFdRoute, HeaderFdPathParams, HeaderFdEnvelope, HeaderUpstreamMs, HeaderInternalKey} {
		if !IsInternalHeader(name) {
			t.Fatalf("%s should be internal", name)
		}
	}
}

func TestResolutionContext(t *testing.T) {
	if FromContext(context.Background()) != nil {
		t.Fatal("want nil")
	}
	res := &Resolution{PathParams: map[string]string{"depositId": "D-1"}, OriginalMethod: "GET"}
	ctx := WithResolution(context.Background(), res)
	if FromContext(ctx) != res {
		t.Fatal("resolution not carried")
	}
	// A value under an equal-looking key from another package must not collide.
	type resolutionKey struct{}
	if FromContext(context.WithValue(context.Background(), resolutionKey{}, res)) != nil {
		t.Fatal("foreign key collided")
	}
}

func TestRenderPath(t *testing.T) {
	tests := []struct {
		route  Route
		params map[string]string
		want   string
	}{
		{Route{Action: "deposit_events_read", Path: "/deposits/{depositId}/events"}, map[string]string{"depositId": "D-1"}, "/deposits/D-1/events"},
		{Route{Action: "deposit_events_read", Path: "/deposits/{depositId}/events"}, map[string]string{"depositId": "D/1 x"}, "/deposits/D%2F1%20x/events"},
		{Route{Action: "on_application_confirm", Path: "/on_application_confirm"}, nil, "/on_application_confirm"},
		{Route{Action: "a", Path: "/x/{a}/{b}"}, map[string]string{"a": "1", "b": "2", "extra": "3"}, "/x/1/2"},
	}
	for _, tt := range tests {
		got, err := tt.route.RenderPath(tt.params)
		if err != nil {
			t.Fatalf("%s: %v", tt.route.Path, err)
		}
		if got != tt.want {
			t.Fatalf("%s: got %s want %s", tt.route.Path, got, tt.want)
		}
	}
	for _, params := range []map[string]string{nil, {"depositId": ""}} {
		r := Route{Action: "deposit_read", Path: "/deposits/{depositId}"}
		if _, err := r.RenderPath(params); err == nil || !strings.Contains(err.Error(), "depositId") {
			t.Fatalf("want missing-parameter error for %v, got %v", params, err)
		}
	}
}
