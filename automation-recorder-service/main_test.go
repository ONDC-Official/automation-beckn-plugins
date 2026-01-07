package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
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
		TransactionID: "t1",
		SubscriberURL: "https://s",
		Action:        "on_search",
		Timestamp:     "2026-01-07T00:00:00Z",
		APIName:       "search",
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
		"transactionId":   "t1",
		"subscriberUrl":   "https://s",
		"latestAction":    "init",
		"latestTimestamp": "old",
		"apiList":         []any{},
	}
	seedB, _ := json.Marshal(seed)
	if err := rdb.Set(context.Background(), key, string(seedB), 0).Err(); err != nil {
		t.Fatalf("seed set: %v", err)
	}

	req := &cacheAppendInput{
		TransactionID: "t1",
		SubscriberURL: "https://s/",
		Action:        "on_search",
		Timestamp:     "2026-01-07T00:00:00Z",
		APIName:       "search",
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
}

func TestGrpcLogEventHappyPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	key := createTransactionKey("t1", "https://s")
	seed := map[string]any{
		"transactionId":   "t1",
		"subscriberUrl":   "https://s",
		"latestAction":    "init",
		"latestTimestamp": "old",
		"apiList":         []any{},
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
		"transactionId": "t1",
		"subscriberUrl": "https://s",
		"apiList":       []any{},
	}
	seedB, _ := json.Marshal(seed)
	if err := rdb.Set(ctx, key, string(seedB), 0).Err(); err != nil {
		t.Fatalf("seed set: %v", err)
	}

	req := &cacheAppendInput{
		TransactionID: "t1",
		SubscriberURL: "https://s",
		Action:        "on_search",
		Timestamp:     "2026-01-07T00:00:00Z",
		APIName:       "search",
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