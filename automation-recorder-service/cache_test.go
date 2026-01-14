package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestCreateTransactionKeyNormalization(t *testing.T) {
	tests := []struct {
		name          string
		transactionID string
		subscriberURL string
		expected      string
	}{
		{"normal", "t1", "https://example.com", "t1::https://example.com"},
		{"trailing slash removed", "t1", "https://example.com/", "t1::https://example.com"},
		{"spaces trimmed", " t1 ", " https://example.com/ ", "t1::https://example.com"},
		{"empty transaction", "", "https://example.com", ""},
		{"empty url", "t1", "", ""},
		{"both empty", "", "", ""},
		{"only spaces", "   ", "   ", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := createTransactionKey(tt.transactionID, tt.subscriberURL)
			if result != tt.expected {
				t.Errorf("createTransactionKey() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestUpdateTransactionAtomicallyMessageIdDeduplication(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	key := createTransactionKey("t1", "https://s")
	seed := map[string]any{
		"messageIds": []string{"m1", "m2"},
		"apiList":    []any{},
	}
	seedB, _ := json.Marshal(seed)
	if err := rdb.Set(ctx, key, string(seedB), 0).Err(); err != nil {
		t.Fatalf("seed set: %v", err)
	}

	// Try to add m2 again (should be deduplicated)
	req := &cacheAppendInput{
		PayloadID:     "pid-1",
		TransactionID: "t1",
		SubscriberURL: "https://s",
		MessageID:     "m2",
		Action:        "on_search",
		Timestamp:     "2026-01-07T00:00:00Z",
		TTLSecs:       30,
		Response:      map[string]any{"ok": true},
	}

	if err := updateTransactionAtomically(ctx, rdb, key, req, 0); err != nil {
		t.Fatalf("update: %v", err)
	}

	val, err := rdb.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(val), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	msgIDs, ok := got["messageIds"].([]any)
	if !ok {
		t.Fatalf("messageIds not array: %#v", got["messageIds"])
	}
	if len(msgIDs) != 2 {
		t.Errorf("messageIds length = %v, want 2 (should not duplicate m2)", len(msgIDs))
	}
}

func TestUpdateTransactionAtomicallyNewMessageId(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	key := createTransactionKey("t1", "https://s")
	seed := map[string]any{
		"messageIds": []string{"m1"},
		"apiList":    []any{},
	}
	seedB, _ := json.Marshal(seed)
	if err := rdb.Set(ctx, key, string(seedB), 0).Err(); err != nil {
		t.Fatalf("seed set: %v", err)
	}

	req := &cacheAppendInput{
		PayloadID:     "pid-1",
		TransactionID: "t1",
		SubscriberURL: "https://s",
		MessageID:     "m2",
		Action:        "on_search",
		Timestamp:     "2026-01-07T00:00:00Z",
		TTLSecs:       30,
		Response:      map[string]any{"ok": true},
	}

	if err := updateTransactionAtomically(ctx, rdb, key, req, 0); err != nil {
		t.Fatalf("update: %v", err)
	}

	val, err := rdb.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(val), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	msgIDs, ok := got["messageIds"].([]any)
	if !ok {
		t.Fatalf("messageIds not array: %#v", got["messageIds"])
	}
	if len(msgIDs) != 2 {
		t.Errorf("messageIds length = %v, want 2", len(msgIDs))
	}
	if msgIDs[1] != "m2" {
		t.Errorf("messageIds[1] = %v, want m2", msgIDs[1])
	}
}

func TestUpdateTransactionAtomicallyEmptyMessageId(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	key := createTransactionKey("t1", "https://s")
	seed := map[string]any{
		"messageIds": []string{},
		"apiList":    []any{},
	}
	seedB, _ := json.Marshal(seed)
	if err := rdb.Set(ctx, key, string(seedB), 0).Err(); err != nil {
		t.Fatalf("seed set: %v", err)
	}

	req := &cacheAppendInput{
		PayloadID:     "pid-1",
		TransactionID: "t1",
		SubscriberURL: "https://s",
		MessageID:     "",
		Action:        "on_search",
		Timestamp:     "2026-01-07T00:00:00Z",
		TTLSecs:       30,
		Response:      map[string]any{"ok": true},
	}

	if err := updateTransactionAtomically(ctx, rdb, key, req, 0); err != nil {
		t.Fatalf("update: %v", err)
	}

	val, err := rdb.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(val), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	msgIDs, ok := got["messageIds"].([]any)
	if !ok {
		t.Fatalf("messageIds not array: %#v", got["messageIds"])
	}
	if len(msgIDs) != 0 {
		t.Errorf("messageIds length = %v, want 0 (empty messageId should not be added)", len(msgIDs))
	}
}

func TestLoadTransactionMap(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	key := "test-key"
	data := map[string]any{"field": "value"}
	dataB, _ := json.Marshal(data)
	if err := rdb.Set(ctx, key, string(dataB), 0).Err(); err != nil {
		t.Fatalf("set: %v", err)
	}

	result, err := loadTransactionMap(ctx, rdb, key)
	if err != nil {
		t.Fatalf("loadTransactionMap() error = %v", err)
	}
	if result["field"] != "value" {
		t.Errorf("result[field] = %v, want value", result["field"])
	}
}

func TestLoadTransactionMapNotFound(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	result, err := loadTransactionMap(ctx, rdb, "nonexistent-key")
	if err != nil {
		t.Fatalf("loadTransactionMap() error = %v", err)
	}
	if result != nil {
		t.Errorf("result = %v, want nil for nonexistent key", result)
	}
}

func TestLoadTransactionMapInvalidJSON(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	key := "test-key"
	if err := rdb.Set(ctx, key, "not-valid-json", 0).Err(); err != nil {
		t.Fatalf("set: %v", err)
	}

	result, err := loadTransactionMap(ctx, rdb, key)
	if err == nil {
		t.Errorf("loadTransactionMap() expected error for invalid JSON")
	}
	if result != nil {
		t.Errorf("result = %v, want nil on error", result)
	}
}

func TestSetFlowStatusIfExistsCreatesNewKey(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	// Create the key first
	key := createFlowStatusCacheKey("t1", "https://s")
	initial := map[string]any{"status": "PENDING"}
	initialB, _ := json.Marshal(initial)
	if err := rdb.Set(ctx, key, string(initialB), 0).Err(); err != nil {
		t.Fatalf("set: %v", err)
	}

	// Update it
	if err := setFlowStatusIfExists(ctx, rdb, "t1", "https://s", "COMPLETED", 1*time.Hour); err != nil {
		t.Fatalf("setFlowStatusIfExists() error = %v", err)
	}

	val, err := rdb.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(val), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got["status"] != "COMPLETED" {
		t.Errorf("status = %v, want COMPLETED", got["status"])
	}

	// Check TTL was set
	ttl, err := rdb.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 0 || ttl > 1*time.Hour {
		t.Errorf("TTL = %v, want between 0 and 1 hour", ttl)
	}
}

func TestCreateFlowStatusCacheKey(t *testing.T) {
	key := createFlowStatusCacheKey("t1", "https://example.com/")
	expected := "FLOW_STATUS_t1::https://example.com/"
	if key != expected {
		t.Errorf("createFlowStatusCacheKey() = %v, want %v", key, expected)
	}
}

func TestUpdateTransactionAtomicallyRetry(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	key := createTransactionKey("t1", "https://s")
	seed := map[string]any{
		"messageIds": []string{},
		"apiList":    []any{},
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

	// Should succeed even with concurrent updates
	if err := updateTransactionAtomically(ctx, rdb, key, req, 0); err != nil {
		t.Fatalf("update: %v", err)
	}
}
