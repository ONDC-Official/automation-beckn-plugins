package ondcworkbench

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

const (
	testSubscriberURL = "https://np.example/issuer"
	testTxnID         = "txn-1"
	fdPropertiesYAML  = `
supportedActions:
  "null": [application_create]
  application_create: [on_application_create, application_create]
  on_application_create: [application_create]
apiProperties:
  application_create: {async_predecessor: null, transaction_partner: []}
  on_application_create: {async_predecessor: application_create, transaction_partner: [application_create]}
`
	becknPropertiesYAML = `
supportedActions:
  "null": [search]
  search: [on_search]
apiProperties:
  search: {async_predecessor: null, transaction_partner: []}
  on_search: {async_predecessor: search, transaction_partner: [search]}
`
)

// newTestWorkbench builds the plugin through New, as ONIX does.
func newTestWorkbench(t *testing.T, domain, version, moduleType, propertiesYAML string) (*ondcWorkbench, *memCache) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transaction_properties.yaml")
	if err := os.WriteFile(path, []byte(propertiesYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := &memCache{m: map[string]string{}}
	wb, _, err := New(context.Background(), cache, &Config{
		ProtocolVersion:           version,
		ProtocolDomain:            domain,
		ModuleRole:                "BAP",
		ModuleType:                moduleType,
		MockServiceURL:            "http://mock.example/mock",
		TransactionPropertiesPath: path,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return wb.(*ondcWorkbench), cache
}

func contextBody(t *testing.T, ctxFields map[string]any, extra map[string]any) []byte {
	t.Helper()
	ctx := map[string]any{
		"transaction_id": testTxnID, "message_id": "m1", "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"bap_id": "app.example", "bap_uri": "https://wb.example/investor-app",
		"bpp_id": "issuer.example", "bpp_uri": testSubscriberURL, "ttl": "PT30S",
	}
	for k, v := range ctxFields {
		ctx[k] = v
	}
	body := map[string]any{"context": ctx, "message": map[string]any{}}
	for k, v := range extra {
		body[k] = v
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func validateContextCookies(t *testing.T, wb *ondcWorkbench, cache *memCache, body []byte) *http.Request {
	t.Helper()
	cache.m[wb.TransactionCache.CreateTransactionKey(testTxnID, testSubscriberURL)] =
		`{"latestAction":"","latestTimestamp":"1970-01-01T00:00:00Z","apiList":[],"messageIds":[]}`
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	req.AddCookie(&http.Cookie{Name: "subscriber_url", Value: testSubscriberURL})
	req.AddCookie(&http.Cookie{Name: "request_owner", Value: "seller_np"})
	if err := wb.WorkbenchValidateContext(context.Background(), req, body); err != nil {
		t.Fatalf("WorkbenchValidateContext() error = %v", err)
	}
	return req
}

func ackBody(t *testing.T, req *http.Request) map[string]any {
	t.Helper()
	c, err := req.Cookie("custom-response-body")
	if err != nil {
		t.Fatalf("no custom-response-body cookie: %v", err)
	}
	decoded, _ := base64.StdEncoding.DecodeString(c.Value)
	var ack map[string]any
	if err := json.Unmarshal(decoded, &ack); err != nil {
		t.Fatalf("custom-response-body %q: %v", decoded, err)
	}
	if status := ack["message"].(map[string]any)["ack"].(map[string]any)["status"]; status != "ACK" {
		t.Fatalf("ack status = %v, want ACK", status)
	}
	return ack
}

func TestIsFD(t *testing.T) {
	for domain, want := range map[string]bool{"ONDC:FIS14:FD": true, "ONDC:FIS14": false, "ONDC:RET10": false, "": false} {
		if got := isFD(&Config{ProtocolDomain: domain}); got != want {
			t.Errorf("isFD(%q) = %t, want %t", domain, got, want)
		}
	}
	if isFD(nil) {
		t.Errorf("isFD(nil) = true")
	}
}

// FD's version is 0.1.4, which Beckn's rule (echo only for "1…") would answer without context.
func TestFDAckEchoesContext(t *testing.T) {
	wb, cache := newTestWorkbench(t, "ONDC:FIS14:FD", "0.1.4", "receiver", fdPropertiesYAML)
	body := contextBody(t,
		map[string]any{"domain": "ONDC:FIS14:FD", "version": "0.1.4", "action": "application_create"},
		map[string]any{"fdEnvelope": map[string]any{"envelopeVersion": "1.0"}})
	ack := ackBody(t, validateContextCookies(t, wb, cache, body))
	ctx, ok := ack["context"].(map[string]any)
	if !ok {
		t.Fatalf("FD ACK = %v, want the request context echoed", ack)
	}
	if ctx["version"] != "0.1.4" || ctx["message_id"] != "m1" || ctx["action"] != "application_create" {
		t.Fatalf("echoed context = %v, want the request's", ctx)
	}
}

// Beckn keeps its rule: context echoed for version 1.x only.
func TestBecknAckUnchanged(t *testing.T) {
	tests := []struct {
		name        string
		ctxFields   map[string]any
		wantContext bool
	}{
		{name: "version 2.0.0", ctxFields: map[string]any{"version": "2.0.0"}, wantContext: false},
		{name: "version 0.1.4 outside FD", ctxFields: map[string]any{"version": "0.1.4"}, wantContext: false},
		{name: "core_version 1.2.5", ctxFields: map[string]any{"core_version": "1.2.5"}, wantContext: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wb, cache := newTestWorkbench(t, "ONDC:RET10", "1.2.5", "receiver", becknPropertiesYAML)
			fields := map[string]any{"domain": "ONDC:RET10", "action": "search"}
			for k, v := range tt.ctxFields {
				fields[k] = v
			}
			ack := ackBody(t, validateContextCookies(t, wb, cache, contextBody(t, fields, nil)))
			if _, has := ack["context"]; has != tt.wantContext {
				t.Fatalf("ACK has context = %t, want %t: %v", has, tt.wantContext, ack)
			}
		})
	}
}

// A session whose difficulty switches header and protocol validation off.
const lenientSession = `{"transactionIds":[],"flowMap":{},"usecaseId":"U1",` +
	`"sessionDifficulty":{"protocolValidations":false,"headerValidaton":false,"useGzip":false}}`

func receiveCookies(t *testing.T, wb *ondcWorkbench, req *http.Request, body []byte) map[string]string {
	t.Helper()
	if err := wb.WorkbenchReceiver(context.Background(), req, body); err != nil {
		t.Fatalf("WorkbenchReceiver() error = %v", err)
	}
	first := map[string]string{}
	for _, c := range req.Cookies() {
		if _, seen := first[c.Name]; !seen {
			first[c.Name] = c.Value // what http.Request.Cookie, and so ONIX, reads
		}
	}
	return first
}

func TestFDForcesValidationCookies(t *testing.T) {
	fdBody := func(t *testing.T) []byte {
		return contextBody(t,
			map[string]any{"domain": "ONDC:FIS14:FD", "version": "0.1.4", "action": "application_create"},
			map[string]any{"fdEnvelope": map[string]any{"envelopeVersion": "1.0"}})
	}

	t.Run("mock module, new transaction", func(t *testing.T) {
		wb, cache := newTestWorkbench(t, "ONDC:FIS14:FD", "0.1.4", "caller", fdPropertiesYAML)
		cache.m["s1"] = lenientSession
		req := httptest.NewRequest(http.MethodPost, "/mock/application_create?flow_id=f1&session_id=s1", nil)
		req.AddCookie(&http.Cookie{Name: "header_validation", Value: "false"}) // sent by the caller
		got := receiveCookies(t, wb, req, fdBody(t))
		if got["header_validation"] != "true" || got["protocol_validation"] != "true" {
			t.Fatalf("header_validation=%q protocol_validation=%q, want both true", got["header_validation"], got["protocol_validation"])
		}
		if got["session_id"] != "s1" || got["flow_id"] != "f1" || got["subscriber_url"] != testSubscriberURL || got["use_gzip"] != "false" {
			t.Fatalf("the other cookies must survive unchanged, got %v", got)
		}
		for _, c := range req.Cookies() {
			if (c.Name == "header_validation" || c.Name == "protocol_validation") && c.Value != "true" {
				t.Fatalf("a %s=%s cookie is left on the request", c.Name, c.Value)
			}
		}
	})

	t.Run("NP module, existing transaction", func(t *testing.T) {
		wb, cache := newTestWorkbench(t, "ONDC:FIS14:FD", "0.1.4", "receiver", fdPropertiesYAML)
		cache.m["s1"] = lenientSession
		// moduleRole BAP receives at bap_uri.
		cache.m[wb.TransactionCache.CreateTransactionKey(testTxnID, "https://wb.example/investor-app")] =
			`{"sessionId":"s1","flowId":"f1","latestAction":"","latestTimestamp":"1970-01-01T00:00:00Z","apiList":[]}`
		req := httptest.NewRequest(http.MethodPost, "/investor-app/application_create", nil)
		got := receiveCookies(t, wb, req, fdBody(t))
		if got["header_validation"] != "true" || got["protocol_validation"] != "true" {
			t.Fatalf("header_validation=%q protocol_validation=%q, want both true", got["header_validation"], got["protocol_validation"])
		}
	})
}

// Beckn sessions keep their difficulty.
func TestBecknValidationCookiesUnchanged(t *testing.T) {
	wb, cache := newTestWorkbench(t, "ONDC:RET10", "1.2.5", "caller", becknPropertiesYAML)
	cache.m["s1"] = lenientSession
	req := httptest.NewRequest(http.MethodPost, "/mock/search?flow_id=f1&session_id=s1", nil)
	got := receiveCookies(t, wb, req, contextBody(t, map[string]any{"domain": "ONDC:RET10", "core_version": "1.2.5", "action": "search"}, nil))
	if got["header_validation"] != "false" || got["protocol_validation"] != "false" {
		t.Fatalf("header_validation=%q protocol_validation=%q, want the session's false", got["header_validation"], got["protocol_validation"])
	}
}
