package networkobservability

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type auditServiceServer interface {
	LogEvent(context.Context, *wrapperspb.BytesValue) (*emptypb.Empty, error)
}

type auditSvc struct {
	got chan []byte
}

func (s *auditSvc) LogEvent(_ context.Context, in *wrapperspb.BytesValue) (*emptypb.Empty, error) {
	s.got <- append([]byte(nil), in.Value...)
	return &emptypb.Empty{}, nil
}

func registerAuditService(s *grpc.Server, impl auditServiceServer) {
	s.RegisterService(&grpc.ServiceDesc{
		ServiceName: "beckn.audit.v1.AuditService",
		HandlerType: (*auditServiceServer)(nil),
		Methods: []grpc.MethodDesc{
			{
				MethodName: "LogEvent",
				Handler: func(srv interface{}, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
					in := new(wrapperspb.BytesValue)
					if err := dec(in); err != nil {
						return nil, err
					}
					if interceptor == nil {
						return srv.(auditServiceServer).LogEvent(ctx, in)
					}
					info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/beckn.audit.v1.AuditService/LogEvent"}
					handler := func(ctx context.Context, req any) (any, error) {
						return srv.(auditServiceServer).LogEvent(ctx, req.(*wrapperspb.BytesValue))
					}
					return interceptor(ctx, in, info, handler)
				},
			},
		},
		Streams:  []grpc.StreamDesc{},
		Metadata: "proto/audit.proto",
	}, impl)
}

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

func TestParseConfigGrpcDoesNotValidateHTTPMethod(t *testing.T) {
	path := writeTempYAML(t, "transport: grpc\ngrpc_target: 127.0.0.1:12345\naudit_method: PATCH\n")
	_, err := parseConfigFile(path)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
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
		"audit_url: "+server.URL+"\n"+
		"async: false\n"+
		"audit_bearer_token: TOKEN\n"+
		"remap:\n"+
		"  method: \"$.ctx.method\"\n"+
		"  sid: \"$.ctx.sid\"\n"+
		"  sid2: \"$.ctx.cookies.sid\"\n"+
		"  foo_cookie: \"$.ctx.cookies.foo\"\n"+
		"  x_test: \"$.ctx.headers.x-test\"\n"+
		"  status: \"$.ctx.status\"\n"+
		"  id: \"uuid()\"\n"+
		"  id2: \"$.ctx.uuid\"\n",
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
	req.AddCookie(&http.Cookie{Name: "foo", Value: "bar"})
	req.Header.Set("X-Test", "abc")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	select {
	case payload := <-received:
		requestBody, ok := payload["requestBody"].(map[string]any)
		if !ok {
			t.Fatalf("expected requestBody object")
		}
		if requestBody["context"] == nil {
			t.Fatalf("expected requestBody.context present")
		}

		responseBody, ok := payload["responseBody"].(map[string]any)
		if !ok {
			t.Fatalf("expected responseBody object")
		}
		if responseBody["ok"] != true {
			t.Fatalf("expected responseBody.ok true, got %#v", responseBody["ok"])
		}

		additional, ok := payload["additionalData"].(map[string]any)
		if !ok {
			t.Fatalf("expected additionalData object")
		}
		if additional["method"] != "GET" {
			t.Fatalf("expected method GET, got %#v", additional["method"])
		}
		if additional["sid"] != "123" {
			t.Fatalf("expected sid 123, got %#v", additional["sid"])
		}
		if additional["sid2"] != "123" {
			t.Fatalf("expected sid2 123, got %#v", additional["sid2"])
		}
		if additional["foo_cookie"] != "bar" {
			t.Fatalf("expected foo_cookie bar, got %#v", additional["foo_cookie"])
		}
		if additional["x_test"] != "abc" {
			t.Fatalf("expected x_test abc, got %#v", additional["x_test"])
		}
		status, ok := additional["status"].(float64)
		if !ok {
			t.Fatalf("expected numeric status, got %#v", additional["status"])
		}
		if int(status) != 201 {
			t.Fatalf("expected status 201, got %#v", additional["status"])
		}
		id, _ := additional["id"].(string)
		id2, _ := additional["id2"].(string)
		if id == "" || id2 == "" {
			t.Fatalf("expected non-empty ids")
		}
		if id != id2 {
			t.Fatalf("expected uuid() == $.ctx.uuid")
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
		"audit_url: "+server.URL+"\n"+
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
		"audit_url: "+server.URL+"\n"+
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

func TestAuditSyncGrpcDispatch(t *testing.T) {
	got := make(chan []byte, 1)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	grpcServer := grpc.NewServer()
	registerAuditService(grpcServer, &auditSvc{got: got})
	go func() { _ = grpcServer.Serve(listener) }()
	defer grpcServer.Stop()

	configPath := writeTempYAML(t, ""+
		"transport: grpc\n"+
		"grpc_target: "+listener.Addr().String()+"\n"+
		"grpc_insecure: true\n"+
		"async: false\n"+
		"remap:\n"+
		"  method: \"$.ctx.method\"\n"+
		"  status: \"$.ctx.status\"\n"+
		"  id: \"uuid()\"\n",
	)

	mw, err := NewNetworkObservabilityMiddleware(context.Background(), configPath)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/", nil))

	select {
	case b := <-got:
		var payload map[string]any
		if err := json.Unmarshal(b, &payload); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		additional, ok := payload["additionalData"].(map[string]any)
		if !ok {
			t.Fatalf("expected additionalData object")
		}
		if additional["method"] != "GET" {
			t.Fatalf("expected method GET, got %#v", additional["method"])
		}
		status, ok := additional["status"].(float64)
		if !ok {
			t.Fatalf("expected numeric status, got %#v", additional["status"])
		}
		if int(status) != 201 {
			t.Fatalf("expected status 201, got %#v", additional["status"])
		}
		id, _ := additional["id"].(string)
		if id == "" {
			t.Fatalf("expected non-empty id")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for grpc audit")
	}
}

func readAllAndClose(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()
	return io.ReadAll(rc)
}
