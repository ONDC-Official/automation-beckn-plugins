package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestDeriveFieldsValid(t *testing.T) {
	payload := auditPayload{
		RequestBody: map[string]any{
			"context": map[string]any{
				"message_id": "m1",
			},
		},
		ResponseBody: map[string]any{"ok": true},
		AdditionalData: map[string]any{
			"payload_id":        "pid-1",
			"transaction_id":    "t1",
			"subscriber_url":    "https://example.com",
			"action":            "on_search",
			"timestamp":         "2026-01-07T00:00:00Z",
			"api_name":          "search",
			"status_code":       float64(200),
			"ttl_seconds":       float64(30),
			"cache_ttl_seconds": float64(60),
			"is_mock":           true,
			"session_id":        "session-1",
		},
	}

	derived, err := deriveFields(payload)
	if err != nil {
		t.Fatalf("deriveFields() error = %v", err)
	}

	if derived.PayloadID != "pid-1" {
		t.Errorf("PayloadID = %v, want pid-1", derived.PayloadID)
	}
	if derived.TransactionID != "t1" {
		t.Errorf("TransactionID = %v, want t1", derived.TransactionID)
	}
	if derived.MessageID != "m1" {
		t.Errorf("MessageID = %v, want m1", derived.MessageID)
	}
	if derived.SubscriberURL != "https://example.com" {
		t.Errorf("SubscriberURL = %v, want https://example.com", derived.SubscriberURL)
	}
	if derived.Action != "on_search" {
		t.Errorf("Action = %v, want on_search", derived.Action)
	}
	if derived.Timestamp != "2026-01-07T00:00:00Z" {
		t.Errorf("Timestamp = %v, want 2026-01-07T00:00:00Z", derived.Timestamp)
	}
	if derived.APIName != "search" {
		t.Errorf("APIName = %v, want search", derived.APIName)
	}
	if derived.StatusCode != 200 {
		t.Errorf("StatusCode = %v, want 200", derived.StatusCode)
	}
	if derived.TTLSecs != 30 {
		t.Errorf("TTLSecs = %v, want 30", derived.TTLSecs)
	}
	if derived.CacheTTLSecs != 60 {
		t.Errorf("CacheTTLSecs = %v, want 60", derived.CacheTTLSecs)
	}
	if !derived.IsMock {
		t.Errorf("IsMock = %v, want true", derived.IsMock)
	}
	if derived.SessionID != "session-1" {
		t.Errorf("SessionID = %v, want session-1", derived.SessionID)
	}
}

func TestDeriveFieldsMissingTransactionID(t *testing.T) {
	payload := auditPayload{
		RequestBody:  map[string]any{},
		ResponseBody: map[string]any{"ok": true},
		AdditionalData: map[string]any{
			"subscriber_url": "https://example.com",
		},
	}

	_, err := deriveFields(payload)
	if err == nil {
		t.Error("deriveFields() expected error for missing transaction_id")
	}
}

func TestDeriveFieldsMissingSubscriberURL(t *testing.T) {
	payload := auditPayload{
		RequestBody:  map[string]any{},
		ResponseBody: map[string]any{"ok": true},
		AdditionalData: map[string]any{
			"transaction_id": "t1",
		},
	}

	_, err := deriveFields(payload)
	if err == nil {
		t.Error("deriveFields() expected error for missing subscriber_url")
	}
}

func TestDeriveFieldsDefaults(t *testing.T) {
	payload := auditPayload{
		RequestBody:  map[string]any{},
		ResponseBody: map[string]any{"ok": true},
		AdditionalData: map[string]any{
			"transaction_id": "t1",
			"subscriber_url": "https://example.com",
		},
	}

	derived, err := deriveFields(payload)
	if err != nil {
		t.Fatalf("deriveFields() error = %v", err)
	}

	if derived.Action != "unknown_action" {
		t.Errorf("Action default = %v, want unknown_action", derived.Action)
	}
	if derived.APIName != "unknown_api" {
		t.Errorf("APIName default = %v, want unknown_api", derived.APIName)
	}
	if derived.Timestamp == "" {
		t.Error("Timestamp should be auto-generated")
	}
	// Verify timestamp is valid RFC3339Nano
	if _, err := time.Parse(time.RFC3339Nano, derived.Timestamp); err != nil {
		t.Errorf("Timestamp format invalid: %v", err)
	}
}

func TestDeriveFieldsMessageIDBackfill(t *testing.T) {
	payload := auditPayload{
		RequestBody: map[string]any{
			"context": map[string]any{
				"message_id": "m-from-context",
			},
		},
		ResponseBody: map[string]any{"ok": true},
		AdditionalData: map[string]any{
			"transaction_id": "t1",
			"subscriber_url": "https://example.com",
			// message_id not provided in additionalData
		},
	}

	derived, err := deriveFields(payload)
	if err != nil {
		t.Fatalf("deriveFields() error = %v", err)
	}

	if derived.MessageID != "m-from-context" {
		t.Errorf("MessageID = %v, want m-from-context (from context)", derived.MessageID)
	}
}

func TestDeriveFieldsMessageIDPriority(t *testing.T) {
	payload := auditPayload{
		RequestBody: map[string]any{
			"context": map[string]any{
				"message_id": "m-from-context",
			},
		},
		ResponseBody: map[string]any{"ok": true},
		AdditionalData: map[string]any{
			"transaction_id": "t1",
			"subscriber_url": "https://example.com",
			"message_id":     "m-from-additional",
		},
	}

	derived, err := deriveFields(payload)
	if err != nil {
		t.Fatalf("deriveFields() error = %v", err)
	}

	if derived.MessageID != "m-from-additional" {
		t.Errorf("MessageID = %v, want m-from-additional (additionalData takes priority)", derived.MessageID)
	}
}

func TestAppendFormEntryAtomicallyInvalidKey(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	err := appendFormEntryAtomically(ctx, rdb, "", "https://s", "f1", "type", "sub", nil)
	if err == nil {
		t.Error("appendFormEntryAtomically() expected error for empty transaction_id")
	}

	err = appendFormEntryAtomically(ctx, rdb, "t1", "", "f1", "type", "sub", nil)
	if err == nil {
		t.Error("appendFormEntryAtomically() expected error for empty subscriber_url")
	}
}

func TestAppendFormEntryAtomicallyPreservesTTL(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	key := createTransactionKey("t1", "https://s")
	seed := map[string]any{"apiList": []any{}}
	seedB, _ := json.Marshal(seed)
	
	// Set with 1 hour TTL
	if err := rdb.Set(ctx, key, string(seedB), 1*time.Hour).Err(); err != nil {
		t.Fatalf("seed set: %v", err)
	}

	err := appendFormEntryAtomically(ctx, rdb, "t1", "https://s", "f1", "HTML", "sub", nil)
	if err != nil {
		t.Fatalf("appendFormEntryAtomically() error = %v", err)
	}

	// Check TTL is still set
	ttl, err := rdb.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 0 {
		t.Error("TTL should be preserved")
	}
	if ttl > 1*time.Hour {
		t.Error("TTL should not exceed original value")
	}
}

func TestRecoveryUnaryInterceptor(t *testing.T) {
	ctx := context.Background()

	// Test normal execution
	called := false
	_, err := recoveryUnaryInterceptor(ctx, nil, nil, func(ctx context.Context, req any) (any, error) {
		called = true
		return "ok", nil
	})

	if err != nil {
		t.Errorf("recoveryUnaryInterceptor() error = %v", err)
	}
	if !called {
		t.Error("handler was not called")
	}
}

func TestRecoveryUnaryInterceptorPanic(t *testing.T) {
	ctx := context.Background()

	// Test panic recovery
	_, err := recoveryUnaryInterceptor(ctx, nil, nil, func(ctx context.Context, req any) (any, error) {
		panic("test panic")
	})

	if err == nil {
		t.Error("recoveryUnaryInterceptor() should return error on panic")
	}
}

func TestTsISOStringNow(t *testing.T) {
	result := tsISOStringNow()

	// Should match format: 2006-01-02T15:04:05.000Z
	if len(result) != 24 {
		t.Errorf("timestamp length = %v, want 24", len(result))
	}
	if result[len(result)-1] != 'Z' {
		t.Error("timestamp should end with Z")
	}

	// Should be parseable
	_, err := time.Parse("2006-01-02T15:04:05.000Z", result)
	if err != nil {
		t.Errorf("timestamp format invalid: %v", err)
	}
}

func TestCreateFlowStatusCacheKeyFormat(t *testing.T) {
	key := createFlowStatusCacheKey("txn-123", "https://example.com/")
	expected := "FLOW_STATUS_txn-123::https://example.com/"
	
	if key != expected {
		t.Errorf("createFlowStatusCacheKey() = %v, want %v", key, expected)
	}
}

func TestSetFlowStatusIfExistsNotCreateNew(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	// Key does not exist
	err := setFlowStatusIfExists(ctx, rdb, "t1", "https://s", "COMPLETED", 1*time.Hour)
	if err != nil {
		t.Fatalf("setFlowStatusIfExists() error = %v", err)
	}

	// Verify key was NOT created
	key := createFlowStatusCacheKey("t1", "https://s")
	exists := rdb.Exists(ctx, key).Val()
	if exists != 0 {
		t.Error("key should not be created if it doesn't exist")
	}
}
