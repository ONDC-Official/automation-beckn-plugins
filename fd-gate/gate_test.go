package fdgate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdroute"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestParseConfigDefaults(t *testing.T) {
	c, err := parseConfig(map[string]string{"routeTable": "/x.yaml", "mode": "np", "pathPrefix": prefix + "/", "addr": "redis:6379"}, envOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{
		RouteTable: "/x.yaml", Mode: ModeNP, PathPrefix: prefix, Addr: "redis:6379",
		RegistryURL: DefaultRegistryURL, Signature: SignatureRequired, RequireHTTPS: true,
		ClockSkew: 5 * time.Second, ReplayTTL: 24 * time.Hour, ObjectTTL: 7 * 24 * time.Hour, MaxBodyBytes: 8 << 20,
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("config\n got %+v\nwant %+v", c, want)
	}
}

func TestParseConfigValues(t *testing.T) {
	cfg := map[string]string{
		"routeTable": "/x.yaml", "mode": "mock", "addr": "r:1", "passThrough": "true",
		"mockServiceURL": "http://mock:3000/mock", "registryURL": "https://reg.example/v2.0/",
		"signature": "OFF", "requireHTTPS": "false", "clockSkew": "PT10S", "replayTTL": "2h",
		"objectTTL": "P30D", "maxBodyBytes": "1MiB",
	}
	env := map[string]string{
		"PWB_INTERNAL_KEY": "k", "SUBSCRIBER_ID": "wb.example", "UNIQUE_KEY_ID": "u1", "SIGNING_PRIVATE": "cHJpdg==",
		"REDIS_USERNAME": "ru", "REDIS_PASSWORD": "rp", "IN_HOUSE_REGISTRY": "https://ignored/",
	}
	c, err := parseConfig(cfg, envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	if !c.PassThrough || c.MockServiceURL != "http://mock:3000/mock" || c.RegistryURL != "https://reg.example/v2.0/" ||
		c.Signature != SignatureOff || c.RequireHTTPS || c.ClockSkew != 10*time.Second || c.ReplayTTL != 2*time.Hour ||
		c.ObjectTTL != 30*24*time.Hour || c.MaxBodyBytes != 1<<20 {
		t.Fatalf("config %+v", c)
	}
	if c.InternalKey != "k" || c.SubscriberID != "wb.example" || c.UniqueKeyID != "u1" || c.SigningPrivateKey != "cHJpdg==" ||
		c.RedisUsername != "ru" || c.RedisPassword != "rp" {
		t.Fatalf("env values %+v", c)
	}
	for in, want := range map[string]int64{"1024": 1024, "2KiB": 2048, "3 MB": 3_000_000, "5KB": 5000} {
		c, err := parseConfig(map[string]string{"routeTable": "/x", "mode": "standalone", "maxBodyBytes": in}, envOf(nil))
		if err != nil || c.MaxBodyBytes != want {
			t.Fatalf("maxBodyBytes %q: %v %v", in, c, err)
		}
	}
}

func TestParseConfigEnvOverrides(t *testing.T) {
	base := map[string]string{
		"routeTable": "/x.yaml", "mode": "np", "addr": "r:1", "requireHTTPS": "true", "signature": "required",
		"registryURL": "https://config-registry/", "mockServiceURL": "http://build-time/mock",
	}
	c, err := parseConfig(base, envOf(map[string]string{
		"FD_REQUIRE_HTTPS": "false", "FD_SIGNATURE": "off", "FD_REGISTRY_URL": "https://env-registry/",
		"MOCK_SERVER_URL": "http://runtime/mock", "IN_HOUSE_REGISTRY": "https://in-house/",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.RequireHTTPS || c.Signature != SignatureOff || c.RegistryURL != "https://env-registry/" || c.MockServiceURL != "http://runtime/mock" {
		t.Fatalf("env did not override: %+v", c)
	}

	// Registry fallbacks when the config key is empty.
	noReg := map[string]string{"routeTable": "/x.yaml", "mode": "np", "addr": "r:1"}
	for _, tt := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"IN_HOUSE_REGISTRY": "https://in-house/", "REGISTRY_URL": "https://alt/"}, "https://in-house/"},
		{map[string]string{"REGISTRY_URL": "https://alt/"}, "https://alt/"},
		{nil, DefaultRegistryURL},
	} {
		c, err := parseConfig(noReg, envOf(tt.env))
		if err != nil || c.RegistryURL != tt.want {
			t.Fatalf("env %v: registry %q, %v; want %q", tt.env, c.RegistryURL, err, tt.want)
		}
	}
	// Config beats the IN_HOUSE_REGISTRY/REGISTRY_URL fallbacks.
	c, _ = parseConfig(base, envOf(map[string]string{"IN_HOUSE_REGISTRY": "https://in-house/"}))
	if c.RegistryURL != "https://config-registry/" || c.MockServiceURL != "http://build-time/mock" {
		t.Fatalf("config not used: %+v", c)
	}

	// The real environment, through ParseConfig.
	t.Setenv("FD_REQUIRE_HTTPS", "false")
	t.Setenv("MOCK_SERVER_URL", "http://env-mock/mock")
	c, err = ParseConfig(base)
	if err != nil || c.RequireHTTPS || c.MockServiceURL != "http://env-mock/mock" {
		t.Fatalf("ParseConfig env: %+v %v", c, err)
	}
	t.Setenv("FD_REQUIRE_HTTPS", "nope")
	if _, err := ParseConfig(base); err == nil {
		t.Fatal("want error for invalid FD_REQUIRE_HTTPS")
	}
}

func TestParseConfigErrors(t *testing.T) {
	bad := []map[string]string{
		{"mode": "np", "addr": "r"},
		{"routeTable": "/x.yaml", "addr": "r"},
		{"routeTable": "/x.yaml", "mode": "bpp", "addr": "r"},
		{"routeTable": "/x.yaml", "mode": "np", "addr": "r", "passThrough": "sometimes"},
		{"routeTable": "/x.yaml", "mode": "np", "addr": "r", "pathPrefix": "api-service"},
		{"routeTable": "/x.yaml", "mode": "np"},   // no Redis
		{"routeTable": "/x.yaml", "mode": "mock"}, // no Redis
		{"routeTable": "/x.yaml", "mode": "np", "addr": "r", "signature": "maybe"},
		{"routeTable": "/x.yaml", "mode": "np", "addr": "r", "requireHTTPS": "yes please"},
		{"routeTable": "/x.yaml", "mode": "np", "addr": "r", "clockSkew": "soon"},
		{"routeTable": "/x.yaml", "mode": "np", "addr": "r", "replayTTL": "0s"},
		{"routeTable": "/x.yaml", "mode": "np", "addr": "r", "maxBodyBytes": "-1"},
		{"routeTable": "/x.yaml", "mode": "np", "addr": "r", "maxBodyBytes": "big"},
	}
	for _, cfg := range bad {
		if _, err := parseConfig(cfg, envOf(nil)); err == nil {
			t.Fatalf("want error for %v", cfg)
		}
	}
	if _, err := parseConfig(map[string]string{"routeTable": "/x.yaml", "mode": "np", "addr": "r"}, envOf(map[string]string{"FD_SIGNATURE": "sometimes"})); err == nil {
		t.Fatal("want error for invalid FD_SIGNATURE")
	}
	if c, err := parseConfig(map[string]string{"routeTable": "/x.yaml", "mode": "standalone"}, envOf(nil)); err != nil || c.Addr != "" {
		t.Fatalf("standalone needs no Redis: %v", err)
	}
}

func TestNewFailsOnBadTable(t *testing.T) {
	if _, err := New(context.Background(), map[string]string{"routeTable": filepath.Join(t.TempDir(), "absent.yaml"), "mode": "np", "addr": "r:1"}); err == nil {
		t.Fatal("want error for missing table")
	}
	p := writeTable(t)
	data, _ := os.ReadFile(p)
	if err := os.WriteFile(p, []byte(strings.Replace(string(data), "portfolio_query", "portfolio_search", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), map[string]string{"routeTable": p, "mode": "np", "addr": "r:1"}); err == nil || !strings.Contains(err.Error(), "tableHash") {
		t.Fatalf("want tableHash error, got %v", err)
	}
}

func TestNewBuildsEveryMode(t *testing.T) {
	p := writeTable(t)
	for _, mode := range []string{"np", "mock", "standalone"} {
		if _, err := New(context.Background(), map[string]string{"routeTable": p, "mode": mode, "addr": "127.0.0.1:1", "signature": "off"}); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
	}
	if _, err := New(context.Background(), map[string]string{"routeTable": p, "mode": "np", "addr": "127.0.0.1:1"}); err != nil {
		t.Fatalf("np with signatures: %v", err)
	}
}

func TestNewGateNeedsDeps(t *testing.T) {
	if _, err := NewGate(baseConfig(t, ModeNP, prefix), Deps{Keys: newFakeKeys()}); err == nil {
		t.Fatal("np without a store must fail")
	}
	if _, err := NewGate(baseConfig(t, ModeNP, prefix), Deps{Store: newMemStore()}); err == nil {
		t.Fatal("np with signatures but no key lookup must fail")
	}
	c := baseConfig(t, ModeNP, prefix)
	c.Signature = SignatureOff
	if _, err := NewGate(c, Deps{Store: newMemStore()}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewGate(baseConfig(t, ModeStandalone, ""), Deps{}); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownRoute(t *testing.T) {
	tg := newNPGate(t)
	rec := tg.serve(mustNotRun, httptest.NewRequest("POST", prefix+"/nope", strings.NewReader("{}")))
	want := `{"message":{"ack":{"status":"NACK"}},"error":{"code":"FD-PWB-0001","class":"PROTOCOL","reason":"FIELD_CONSTRAINT_VIOLATED","message":"ROUTE_NOT_FOUND: no FD 0.1.4 route for POST /nope","obligedParty":"N-01 INVESTOR_APP","retryable":false,"fieldPath":""}}`
	if rec.Code != http.StatusNotFound || rec.Body.String() != want {
		t.Fatalf("got %d %s\nwant 404 %s", rec.Code, rec.Body.String(), want)
	}
	// On the investor-app module the caller is an issuer.
	c := baseConfig(t, ModeNP, appPrefix)
	rec = newTestGate(t, c).serve(mustNotRun, httptest.NewRequest("POST", appPrefix+"/nope", nil))
	if b := expectNack(t, rec, http.StatusNotFound, CodeRouteNotFound); b.Error.ObligedParty != "N-02 DEPOSIT_ISSUER" {
		t.Fatalf("obligedParty = %s", b.Error.ObligedParty)
	}
}

func TestWrongMethodIsNotFound(t *testing.T) {
	rec := newNPGate(t).serve(mustNotRun, httptest.NewRequest("GET", prefix+"/applications/A-1/confirm", nil))
	b := expectNack(t, rec, http.StatusNotFound, CodeRouteNotFound)
	if !strings.Contains(b.Error.Message, "takes POST") {
		t.Fatalf("error = %+v", b.Error)
	}
}

func TestPathOutsidePrefix(t *testing.T) {
	for _, mode := range []Mode{ModeNP, ModeStandalone} {
		tg := newTestGate(t, baseConfig(t, mode, prefix))
		for _, target := range []string{"/deposits/D-1/events", "/api-service/ONDC:FIS14:FD/0.1.4/investor/deposits/D-1/events", prefix} {
			rec := tg.serve(mustNotRun, httptest.NewRequest("GET", target, nil))
			expectNack(t, rec, http.StatusNotFound, CodeRouteNotFound)
		}
	}
}

func TestStandaloneAttachesResolution(t *testing.T) {
	var got *fdroute.Resolution
	var gotHeader http.Header
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = fdroute.FromContext(r.Context())
		gotHeader = r.Header.Clone()
		w.WriteHeader(http.StatusAccepted)
	})
	tg := newTestGate(t, baseConfig(t, ModeStandalone, prefix))

	tests := []struct {
		name, method, target string
		action               string
		params               map[string]string
		query                url.Values
	}{
		{"literal beats param", "POST", prefix + "/deposits/query", "portfolio_query", map[string]string{}, url.Values{}},
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
			rec := tg.serve(next, req)
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

func TestPanicRecovery(t *testing.T) {
	boom := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	t.Run("np: inner panic is a 200 INTERNAL NACK and frees the replay key", func(t *testing.T) {
		tg := newNPGate(t)
		body := confirmMsg("m-panic").bytes(t)
		rec := tg.serve(boom, npRequest(t, "POST", prefix+"/applications/APP-1/confirm", body, signOpts{}))
		expectNack(t, rec, http.StatusOK, CodeInternal)
		if _, ok := tg.store.get(ReplayKey(domain, appID, "application_confirm", "m-panic")); ok {
			t.Fatal("replay key not released after the panic")
		}
		// A retry is handled afresh.
		in := ackInner()
		rec = tg.serve(in, npRequest(t, "POST", prefix+"/applications/APP-1/confirm", body, signOpts{}))
		if rec.Code != http.StatusAccepted || in.calls.Load() != 1 {
			t.Fatalf("retry: %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("mock: inner panic before writing", func(t *testing.T) {
		tg := newTestGate(t, baseConfig(t, ModeMock, mockPrefix))
		req := httptest.NewRequest("POST", mockPrefix+"/application_confirm", strings.NewReader(string(confirmMsg("m-1").bytes(t))))
		req.Header.Set(fdroute.HeaderFdPathParams, `{"applicationId":"APP-1"}`)
		expectNack(t, tg.serve(boom, req), http.StatusOK, CodeInternal)
	})
	t.Run("after response started", func(t *testing.T) {
		tg := newTestGate(t, baseConfig(t, ModeStandalone, ""))
		rec := tg.serve(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("partial"))
			panic("boom")
		}), httptest.NewRequest("GET", "/deposits/D-1", nil))
		if rec.Code != http.StatusOK || rec.Body.String() != "partial" {
			t.Fatalf("response rewritten after it started: %d %q", rec.Code, rec.Body.String())
		}
	})
	t.Run("abort handler re-panics", func(t *testing.T) {
		tg := newTestGate(t, baseConfig(t, ModeStandalone, ""))
		defer func() {
			if p := recover(); p != http.ErrAbortHandler {
				t.Fatalf("recovered %v, want http.ErrAbortHandler", p)
			}
		}()
		tg.serve(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }), httptest.NewRequest("GET", "/deposits/D-1", nil))
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

func TestPathToPointer(t *testing.T) {
	for in, want := range map[string]string{
		"":                                "",
		"/context/action":                 "/context/action",
		"$.message.confirm":               "/message/confirm",
		"message.items[0].id":             "/message/items/0/id",
		"$['context']['bap_id']":          "/context/bap_id",
		"$.context.action, $.context.ttl": "/context/action",
		"$.message.a/b~c":                 "/message/a~1b~0c",
	} {
		if got := pathToPointer(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
}
