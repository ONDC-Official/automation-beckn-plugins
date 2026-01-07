package networkobservability

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "network-observability.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return p
}

func TestParseConfigDefaults(t *testing.T) {
	path := writeTempYAML(t, "audit_url: http://example.com\n")
	cfg, err := parseConfigFile(path)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if cfg.AuditMethod != http.MethodPost {
		t.Fatalf("expected POST, got %q", cfg.AuditMethod)
	}
	if !cfg.Async {
		t.Fatalf("expected async default true")
	}
	if cfg.Timeout != 5*time.Second {
		t.Fatalf("expected 5s timeout, got %v", cfg.Timeout)
	}
	if cfg.QueueSize != 1000 {
		t.Fatalf("expected queue_size 1000, got %d", cfg.QueueSize)
	}
	if cfg.WorkerCount != 2 {
		t.Fatalf("expected worker_count 2, got %d", cfg.WorkerCount)
	}
	if cfg.MaxBodyBytes != 1024*1024 {
		t.Fatalf("expected max_body_bytes 1048576, got %d", cfg.MaxBodyBytes)
	}
	if cfg.IncludeRawReq || cfg.IncludeRawRes {
		t.Fatalf("expected include_raw_* defaults false")
	}
	if !cfg.DropOnQueueFull {
		t.Fatalf("expected drop_on_queue_full default true")
	}
}

func TestParseConfigInvalidMethod(t *testing.T) {
	path := writeTempYAML(t, "audit_url: http://example.com\naudit_method: PATCH\n")
	_, err := parseConfigFile(path)
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestCaptureRequestBodyTruncatesAndRestores(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://example.com/", bytes.NewBufferString("abcdef"))
	_, b, truncated := captureRequestBody(r, 3)
	if !truncated {
		t.Fatalf("expected truncated")
	}
	if string(b) != "abc" {
		t.Fatalf("expected captured 'abc', got %q", string(b))
	}
	restored, err := readAllAndClose(r.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if string(restored) != "abc" {
		t.Fatalf("expected restored body 'abc', got %q", string(restored))
	}
}

func TestCaptureResponseWriterTruncates(t *testing.T) {
	rr := httptest.NewRecorder()
	crw := newCaptureResponseWriter(rr, 3)
	crw.WriteHeader(201)
	_, _ = crw.Write([]byte("abcdef"))

	b, truncated := crw.bodyBytes()
	if !truncated {
		t.Fatalf("expected truncated")
	}
	if string(b) != "abc" {
		t.Fatalf("expected captured 'abc', got %q", string(b))
	}
	if crw.StatusCode() != 201 {
		t.Fatalf("expected status 201, got %d", crw.StatusCode())
	}
}

func TestAuditSyncWithBearerTokenAndRemap(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer TOKEN" {
			w.WriteHeader(401)
			return
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		received <- payload
		w.WriteHeader(204)
	}))
	defer server.Close()

	configPath := writeTempYAML(t, ""+
		"audit_url: " + server.URL + "\n"+
		"async: false\n"+
		"audit_bearer_token: TOKEN\n"+
		"include_raw_req: true\n"+
		"include_raw_res: true\n"+
		"remap:\n"+
		"  method: \"$.req.method\"\n"+
		"  sid: \"$.req.cookies.sid\"\n"+
		"  status: \"$.res.status\"\n"+
		"  id: \"uuid()\"\n"+
		"  id2: \"$.ctx.gen.uuid\"\n",
	)

	mw, err := NewNetworkObservabilityMiddleware(context.Background(), configPath)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "http://example.com/test?x=1", bytes.NewBufferString(`{"context":{"transaction_id":"t1"}}`))
	req.AddCookie(&http.Cookie{Name: "sid", Value: "123"})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	select {
	case payload := <-received:
		mapped, ok := payload["mapped"].(map[string]any)
		if !ok {
			t.Fatalf("expected mapped object")
		}
		if mapped["method"] != "GET" {
			t.Fatalf("expected method GET, got %#v", mapped["method"])
		}
		if mapped["sid"] != "123" {
			t.Fatalf("expected sid 123, got %#v", mapped["sid"])
		}
		status, ok := mapped["status"].(float64)
		if !ok {
			t.Fatalf("expected numeric status, got %#v", mapped["status"])
		}
		if int(status) != 201 {
			t.Fatalf("expected status 201, got %#v", mapped["status"])
		}
		id, _ := mapped["id"].(string)
		id2, _ := mapped["id2"].(string)
		if id == "" || id2 == "" {
			t.Fatalf("expected non-empty ids")
		}
		if id != id2 {
			t.Fatalf("expected uuid() == $.ctx.gen.uuid")
		}
		if _, ok := payload["req"]; !ok {
			t.Fatalf("expected req included")
		}
		if _, ok := payload["res"]; !ok {
			t.Fatalf("expected res included")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for audit")
	}
}

func TestAuditHeadersJsonAuthorizationNotOverridden(t *testing.T) {
	receivedAuth := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth <- r.Header.Get("Authorization")
		w.WriteHeader(204)
	}))
	defer server.Close()

	configPath := writeTempYAML(t, ""+
		"audit_url: " + server.URL + "\n"+
		"async: false\n"+
		"audit_bearer_token: TOKEN\n"+
		"audit_headers:\n"+
		"  Authorization: Custom\n"+
		"remap: {}\n",
	)

	mw, err := NewNetworkObservabilityMiddleware(context.Background(), configPath)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/", nil))

	select {
	case auth := <-receivedAuth:
		if auth != "Custom" {
			t.Fatalf("expected Custom authorization, got %q", auth)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for audit")
	}
}

func TestAuditAsyncDispatch(t *testing.T) {
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		w.WriteHeader(204)
	}))
	defer server.Close()

	configPath := writeTempYAML(t, ""+
		"audit_url: " + server.URL + "\n"+
		"async: true\n"+
		"queue_size: 10\n"+
		"worker_count: 1\n"+
		"remap: {}\n",
	)

	mw, err := NewNetworkObservabilityMiddleware(context.Background(), configPath)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/", nil))

	select {
	case <-received:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for async audit")
	}
}

func readAllAndClose(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()
	return io.ReadAll(rc)
}
