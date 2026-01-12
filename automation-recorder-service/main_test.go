package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestCreateTransactionKey(t *testing.T) {
	k := createTransactionKey(" t1 ", " https://example.com/ ")
	if k != "t1::https://example.com" {
		t.Fatalf("unexpected key: %q", k)
	}
}

func TestUpdateTransactionAtomicallyNotFound(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	req := &cacheAppendInput{
		PayloadID:     "pid-1",
		TransactionID: "t1",
		MessageID:     "m1",
		SubscriberURL: "https://s",
		Action:        "on_search",
		Timestamp:     "2026-01-07T00:00:00Z",
		TTLSecs:       30,
		Response:      map[string]any{"ok": true},
	}

	err := updateTransactionAtomically(context.Background(), rdb, createTransactionKey(req.TransactionID, req.SubscriberURL), req, 0)
	if err == nil {
		t.Fatalf("expected error")
	}
	if err != errNotFound {
		t.Fatalf("expected errNotFound, got %v", err)
	}
}

func TestUpdateTransactionAtomicallyAppendsApi(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	key := createTransactionKey("t1", "https://s/")

	seed := map[string]any{
		"latestAction":    "init",
		"latestTimestamp": "old",
		"type":            "",
		"subscriberType":  "BPP",
		"messageIds":      []string{},
		"apiList":         []any{},
		"referenceData":   map[string]any{},
	}
	seedB, _ := json.Marshal(seed)
	if err := rdb.Set(context.Background(), key, string(seedB), 0).Err(); err != nil {
		t.Fatalf("seed set: %v", err)
	}

	req := &cacheAppendInput{
		PayloadID:     "pid-1",
		TransactionID: "t1",
		SubscriberURL: "https://s/",
		MessageID:     "m1",
		Action:        "on_search",
		Timestamp:     "2026-01-07T00:00:00Z",
		TTLSecs:       30,
		Response:      map[string]any{"ok": true},
	}

	if err := updateTransactionAtomically(context.Background(), rdb, createTransactionKey(req.TransactionID, req.SubscriberURL), req, 0); err != nil {
		t.Fatalf("update: %v", err)
	}

	val, err := rdb.Get(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(val), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["latestAction"] != "on_search" {
		t.Fatalf("latestAction: %#v", got["latestAction"])
	}
	if got["latestTimestamp"] != "2026-01-07T00:00:00Z" {
		t.Fatalf("latestTimestamp: %#v", got["latestTimestamp"])
	}
	apiList, ok := got["apiList"].([]any)
	if !ok || len(apiList) != 1 {
		t.Fatalf("apiList: %#v", got["apiList"])
	}
	entry, ok := apiList[0].(map[string]any)
	if !ok {
		t.Fatalf("apiList[0] not object: %#v", apiList[0])
	}
	if entry["entryType"] != "API" {
		t.Fatalf("entryType: %#v", entry["entryType"])
	}
	if entry["action"] != "on_search" {
		t.Fatalf("action: %#v", entry["action"])
	}
	if entry["payloadId"] != "pid-1" {
		t.Fatalf("payloadId: %#v", entry["payloadId"])
	}
	if entry["messageId"] != "m1" {
		t.Fatalf("messageId: %#v", entry["messageId"])
	}
	if entry["timestamp"] != "2026-01-07T00:00:00Z" {
		t.Fatalf("timestamp: %#v", entry["timestamp"])
	}
	if rt, _ := entry["realTimestamp"].(string); rt == "" {
		t.Fatalf("realTimestamp: %#v", entry["realTimestamp"])
	}
	if entry["ttl"].(float64) != 30 {
		t.Fatalf("ttl: %#v", entry["ttl"])
	}
	msgIDs, ok := got["messageIds"].([]any)
	if !ok || len(msgIDs) != 1 || msgIDs[0] != "m1" {
		t.Fatalf("messageIds: %#v", got["messageIds"])
	}
}

func TestGrpcLogEventHappyPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	key := createTransactionKey("t1", "https://s")
	seed := map[string]any{
		"latestAction":    "init",
		"latestTimestamp": "old",
		"type":            "",
		"subscriberType":  "BPP",
		"messageIds":      []string{},
		"apiList":         []any{},
		"referenceData":   map[string]any{},
	}
	seedB, _ := json.Marshal(seed)
	if err := rdb.Set(ctx, key, string(seedB), 0).Err(); err != nil {
		t.Fatalf("seed set: %v", err)
	}

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	registerAuditService(gs, &recorderServer{rdb: rdb, cfg: config{SkipNOPush: true, SkipDBSave: true, AsyncQueueSize: 10, AsyncWorkerCount: 1, DropOnQueueFull: true, Env: "test"}, httpClient: http.DefaultClient, async: newAsyncDispatcher(ctx, 10, 1, true)})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.DialContext(ctx, "bufnet", grpc.WithContextDialer(dialer), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	payload := map[string]any{
		"requestBody":  map[string]any{"context": map[string]any{"transaction_id": "t1"}},
		"responseBody": map[string]any{"ok": true},
		"additionalData": map[string]any{
			"payload_id":        "pid-1",
			"transaction_id":    "t1",
			"subscriber_url":    "https://s",
			"action":            "on_search",
			"timestamp":         "2026-01-07T00:00:00Z",
			"api_name":          "search",
			"ttl_seconds":       30,
			"cache_ttl_seconds": 0,
			"status_code":       200,
		},
	}
	b, _ := json.Marshal(payload)

	req := wrapperspb.Bytes(b)
	res := &emptypb.Empty{}
	if err := conn.Invoke(ctx, grpcFullMethod, req, res); err != nil {
		t.Fatalf("invoke: %v", err)
	}

	val, err := rdb.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(val), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["latestAction"] != "on_search" {
		t.Fatalf("latestAction: %#v", got["latestAction"])
	}
}

func TestGrpcLogEventNotFound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	registerAuditService(gs, &recorderServer{rdb: rdb, cfg: config{SkipNOPush: true, SkipDBSave: true, AsyncQueueSize: 10, AsyncWorkerCount: 1, DropOnQueueFull: true, Env: "test"}, httpClient: http.DefaultClient, async: newAsyncDispatcher(ctx, 10, 1, true)})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.DialContext(ctx, "bufnet", grpc.WithContextDialer(dialer), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	payload := map[string]any{
		"requestBody":  map[string]any{},
		"responseBody": map[string]any{"ok": true},
		"additionalData": map[string]any{
			"payload_id":     "pid-1",
			"transaction_id": "t-missing",
			"subscriber_url": "https://s",
			"action":         "on_search",
			"timestamp":      "2026-01-07T00:00:00Z",
			"api_name":       "search",
			"ttl_seconds":    30,
		},
	}
	b, _ := json.Marshal(payload)

	req := wrapperspb.Bytes(b)
	res := &emptypb.Empty{}
	err = conn.Invoke(ctx, grpcFullMethod, req, res)
	if err == nil {
		t.Fatalf("expected error")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.NotFound {
		t.Fatalf("expected NOT_FOUND, got %v", st.Code())
	}
}

func TestHTMLFormEndpointAppendsFormEntry(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	key := createTransactionKey("t1", "https://s")
	seed := map[string]any{
		"latestAction":    "init",
		"latestTimestamp": "old",
		"type":            "",
		"subscriberType":  "BPP",
		"messageIds":      []string{},
		"apiList":         []any{},
		"referenceData":   map[string]any{},
	}
	seedB, _ := json.Marshal(seed)
	if err := rdb.Set(ctx, key, string(seedB), 0).Err(); err != nil {
		t.Fatalf("seed set: %v", err)
	}

	srv := httptest.NewServer(newHTTPMux(rdb))
	t.Cleanup(srv.Close)

	body := map[string]any{
		"transaction_id": "t1",
		"subscriber_url": "https://s",
		"form_action_id": "form-123",
		"form_type":      "HTML_FORM",
		"submissionId":   "sub-1",
		"error":          map[string]any{"msg": "bad"},
	}
	b, _ := json.Marshal(body)

	resp, err := http.Post(srv.URL+"/html-form", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}

	val, err := rdb.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(val), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["latestAction"] != "init" {
		t.Fatalf("latestAction changed: %#v", got["latestAction"])
	}

	apiList, ok := got["apiList"].([]any)
	if !ok || len(apiList) != 1 {
		t.Fatalf("apiList: %#v", got["apiList"])
	}
	entry, ok := apiList[0].(map[string]any)
	if !ok {
		t.Fatalf("apiList[0] not object: %#v", apiList[0])
	}
	if entry["entryType"] != "FORM" {
		t.Fatalf("entryType: %#v", entry["entryType"])
	}
	if entry["formId"] != "form-123" {
		t.Fatalf("formId: %#v", entry["formId"])
	}
	if entry["formType"] != "HTML_FORM" {
		t.Fatalf("formType: %#v", entry["formType"])
	}
	if entry["submissionId"] != "sub-1" {
		t.Fatalf("submissionId: %#v", entry["submissionId"])
	}
	if ts, _ := entry["timestamp"].(string); ts == "" || !strings.HasSuffix(ts, "Z") {
		t.Fatalf("timestamp: %#v", entry["timestamp"])
	}
}

func TestGrpcLogEventBadJSON(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	registerAuditService(gs, &recorderServer{rdb: rdb, cfg: config{SkipNOPush: true, SkipDBSave: true, AsyncQueueSize: 10, AsyncWorkerCount: 1, DropOnQueueFull: true, Env: "test"}, httpClient: http.DefaultClient, async: newAsyncDispatcher(ctx, 10, 1, true)})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.DialContext(ctx, "bufnet", grpc.WithContextDialer(dialer), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := wrapperspb.Bytes([]byte("not-json"))
	res := &emptypb.Empty{}
	err = conn.Invoke(ctx, grpcFullMethod, req, res)
	if err == nil {
		t.Fatalf("expected error")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("expected INVALID_ARGUMENT, got %v", st.Code())
	}
}

func TestCacheTTLApplied(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	key := createTransactionKey("t1", "https://s")
	seed := map[string]any{
		"latestAction":    "init",
		"latestTimestamp": "old",
		"type":            "",
		"subscriberType":  "BPP",
		"messageIds":      []string{},
		"apiList":         []any{},
		"referenceData":   map[string]any{},
	}
	seedB, _ := json.Marshal(seed)
	if err := rdb.Set(ctx, key, string(seedB), 0).Err(); err != nil {
		t.Fatalf("seed set: %v", err)
	}

	req := &cacheAppendInput{
		PayloadID:     "pid-1",
		TransactionID: "t1",
		SubscriberURL: "https://s",
		MessageID:     "m1",
		Action:        "on_search",
		Timestamp:     "2026-01-07T00:00:00Z",
		TTLSecs:       30,
		Response:      map[string]any{"ok": true},
	}
	if err := updateTransactionAtomically(ctx, rdb, key, req, 1*time.Second); err != nil {
		t.Fatalf("update: %v", err)
	}

	mr.FastForward(2 * time.Second)
	if rdb.Exists(ctx, key).Val() != 0 {
		t.Fatalf("expected key to expire")
	}
}

func TestFlowStatusKeyUpdatedOnlyIfExists(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	// When key doesn't exist, it must not be created.
	missingKey := createFlowStatusCacheKey("t1", "https://s")
	if err := setFlowStatusIfExists(ctx, rdb, "t1", "https://s", "AVAILABLE", 5*time.Hour); err != nil {
		t.Fatalf("setFlowStatusIfExists missing: %v", err)
	}
	if mr.Exists(missingKey) {
		t.Fatalf("expected flow status key to not be created")
	}

	// When key exists, it must be updated with ttl.
	existingKey := createFlowStatusCacheKey("t2", "https://s")
	mr.Set(existingKey, "{}")
	if err := setFlowStatusIfExists(ctx, rdb, "t2", "https://s", "AVAILABLE", 5*time.Hour); err != nil {
		t.Fatalf("setFlowStatusIfExists existing: %v", err)
	}
	val, err := rdb.Get(ctx, existingKey).Result()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(val), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["status"] != "AVAILABLE" {
		t.Fatalf("status: %#v", got["status"])
	}
}

func TestSavePayloadToDB_MatchesTSDataService(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	// Seed a transaction with flow/session/subscriberType data.
	// NOTE: sessionId intentionally omitted to force sha256 fallback.
	key := createTransactionKey("t1", "https://subscriber.example.com")
	seed := map[string]any{
		"flowId":         "flow-1",
		"subscriberType": "BPP",
	}
	seedB, _ := json.Marshal(seed)
	if err := rdb.Set(ctx, key, string(seedB), 0).Err(); err != nil {
		t.Fatalf("seed set: %v", err)
	}

	var gotCheckPath string
	var createdSession map[string]any
	var savedPayload map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/sessions/check/"):
			gotCheckPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("false"))
			return
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions":
			_ = json.NewDecoder(r.Body).Decode(&createdSession)
			w.WriteHeader(200)
			return
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions/payload":
			_ = json.NewDecoder(r.Body).Decode(&savedPayload)
			w.WriteHeader(200)
			return
		default:
			w.WriteHeader(404)
			return
		}
	}))
	defer srv.Close()

	cfg := config{
		Env:           "test",
		DBBaseURL:     srv.URL,
		DBAPIKey:      "k",
		DBTimeout:     2 * time.Second,
		DBEnabledIn:   map[string]bool{"test": true},
		DBSessionPath: "/api/sessions",
		DBPayloadPath: "/api/sessions/payload",
	}

	requestBody := map[string]any{
		"context": map[string]any{
			"transaction_id": "t1",
			"message_id":     "m1",
			"action":         "on_search",
			"domain":         "retail",
			"version":        "2.0.0",
			"bpp_id":         "bpp",
			"bap_id":         "bap",
		},
	}
	responseBody := map[string]any{"ok": true}
	additionalData := map[string]any{
		"req_header": map[string]any{"x-test": "abc"},
	}

	d := derivedFields{
		PayloadID:     "pid-1",
		TransactionID: "t1",
		MessageID:     "m1",
		SubscriberURL: "https://subscriber.example.com",
		Action:        "on_search",
		StatusCode:    201,
	}

	if err := savePayloadToDB(ctx, cfg, srv.Client(), rdb, d, requestBody, responseBody, additionalData); err != nil {
		t.Fatalf("savePayloadToDB: %v", err)
	}

	if gotCheckPath == "" {
		t.Fatalf("expected session check call")
	}
	if createdSession["sessionType"] != "AUTOMATION" {
		t.Fatalf("sessionType: %#v", createdSession["sessionType"])
	}
	if createdSession["npType"] != "BPP" {
		t.Fatalf("npType: %#v", createdSession["npType"])
	}
	if createdSession["npId"] != "https://subscriber.example.com" {
		t.Fatalf("npId: %#v", createdSession["npId"])
	}

	if savedPayload["transactionId"] != "t1" {
		t.Fatalf("transactionId: %#v", savedPayload["transactionId"])
	}
	if savedPayload["payloadId"] != "pid-1" {
		t.Fatalf("payloadId: %#v", savedPayload["payloadId"])
	}
	if savedPayload["action"] != "ON_SEARCH" {
		t.Fatalf("action: %#v", savedPayload["action"])
	}
	if savedPayload["httpStatus"].(float64) != 201 {
		t.Fatalf("httpStatus: %#v", savedPayload["httpStatus"])
	}
}
