package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestHTTPFormMethodNotAllowed(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	srv := httptest.NewServer(newHTTPMux(rdb))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/html-form")
	if err != nil {
		t.Fatalf("GET request error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %v, want %v", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

func TestHTTPFormInvalidJSON(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	srv := httptest.NewServer(newHTTPMux(rdb))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/html-form", "application/json", bytes.NewReader([]byte("not-json")))
	if err != nil {
		t.Fatalf("POST request error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %v, want %v", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestHTTPFormMissingRequiredFields(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	srv := httptest.NewServer(newHTTPMux(rdb))
	defer srv.Close()

	tests := []struct {
		name string
		body map[string]any
	}{
		{"missing transaction_id", map[string]any{"subscriber_url": "https://s", "form_action_id": "f1"}},
		{"missing subscriber_url", map[string]any{"transaction_id": "t1", "form_action_id": "f1"}},
		{"missing form_action_id", map[string]any{"transaction_id": "t1", "subscriber_url": "https://s"}},
		{"wrong type transaction_id", map[string]any{"transaction_id": 123, "subscriber_url": "https://s", "form_action_id": "f1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := json.Marshal(tt.body)
			resp, err := http.Post(srv.URL+"/html-form", "application/json", bytes.NewReader(b))
			if err != nil {
				t.Fatalf("POST request error: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %v, want %v", resp.StatusCode, http.StatusBadRequest)
			}
		})
	}
}

func TestHTTPFormTransactionNotFound(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	srv := httptest.NewServer(newHTTPMux(rdb))
	defer srv.Close()

	body := map[string]any{
		"transaction_id": "nonexistent",
		"subscriber_url": "https://s",
		"form_action_id": "f1",
	}
	b, _ := json.Marshal(body)

	resp, err := http.Post(srv.URL+"/html-form", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST request error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %v, want %v (transaction not found)", resp.StatusCode, http.StatusInternalServerError)
	}
}

func TestHTTPFormSuccessWithAllFields(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	key := createTransactionKey("t1", "https://s")
	seed := map[string]any{
		"apiList": []any{},
	}
	seedB, _ := json.Marshal(seed)
	if err := rdb.Set(ctx, key, string(seedB), 0).Err(); err != nil {
		t.Fatalf("seed set: %v", err)
	}

	srv := httptest.NewServer(newHTTPMux(rdb))
	defer srv.Close()

	body := map[string]any{
		"transaction_id": "t1",
		"subscriber_url": "https://s",
		"form_action_id": "form-123",
		"form_type":      "HTML_FORM",
		"submissionId":   "sub-1",
		"error":          map[string]any{"code": "ERR001"},
	}
	b, _ := json.Marshal(body)

	resp, err := http.Post(srv.URL+"/html-form", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST request error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %v, want %v", resp.StatusCode, http.StatusOK)
	}

	// Verify data was stored
	val, err := rdb.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(val), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	apiList, ok := got["apiList"].([]any)
	if !ok || len(apiList) != 1 {
		t.Fatalf("apiList: %#v", got["apiList"])
	}

	entry, ok := apiList[0].(map[string]any)
	if !ok {
		t.Fatalf("entry not a map: %#v", apiList[0])
	}

	if entry["entryType"] != "FORM" {
		t.Errorf("entryType = %v, want FORM", entry["entryType"])
	}
	if entry["formId"] != "form-123" {
		t.Errorf("formId = %v, want form-123", entry["formId"])
	}
	if entry["submissionId"] != "sub-1" {
		t.Errorf("submissionId = %v, want sub-1", entry["submissionId"])
	}
}

func TestHTTPFormSubmissionIdVariants(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	tests := []struct {
		name     string
		body     map[string]any
		expected string
	}{
		{"camelCase submissionId", map[string]any{"submissionId": "sub-camel"}, "sub-camel"},
		{"snake_case submission_id", map[string]any{"submission_id": "sub-snake"}, "sub-snake"},
		{"both prefers camelCase", map[string]any{"submissionId": "sub-camel", "submission_id": "sub-snake"}, "sub-camel"},
		{"empty submissionId uses snake_case", map[string]any{"submissionId": "", "submission_id": "sub-snake"}, "sub-snake"},
		{"spaces-only submissionId uses snake_case", map[string]any{"submissionId": "   ", "submission_id": "sub-snake"}, "sub-snake"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := createTransactionKey("t1", "https://s")
			seed := map[string]any{"apiList": []any{}}
			seedB, _ := json.Marshal(seed)
			if err := rdb.Set(ctx, key, string(seedB), 0).Err(); err != nil {
				t.Fatalf("seed set: %v", err)
			}

			srv := httptest.NewServer(newHTTPMux(rdb))
			defer srv.Close()

			body := map[string]any{
				"transaction_id": "t1",
				"subscriber_url": "https://s",
				"form_action_id": "f1",
			}
			for k, v := range tt.body {
				body[k] = v
			}
			b, _ := json.Marshal(body)

			resp, err := http.Post(srv.URL+"/html-form", "application/json", bytes.NewReader(b))
			if err != nil {
				t.Fatalf("POST request error: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %v, want %v", resp.StatusCode, http.StatusOK)
			}

			val, err := rdb.Get(ctx, key).Result()
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(val), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			apiList := got["apiList"].([]any)
			entry := apiList[0].(map[string]any)
			
			if tt.expected != "" {
				if entry["submissionId"] != tt.expected {
					t.Errorf("submissionId = %v, want %v", entry["submissionId"], tt.expected)
				}
			} else {
				if _, exists := entry["submissionId"]; exists {
					t.Errorf("submissionId should not exist for empty value")
				}
			}
		})
	}
}

func TestHTTPFormOptionalFields(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	key := createTransactionKey("t1", "https://s")
	seed := map[string]any{"apiList": []any{}}
	seedB, _ := json.Marshal(seed)
	if err := rdb.Set(ctx, key, string(seedB), 0).Err(); err != nil {
		t.Fatalf("seed set: %v", err)
	}

	srv := httptest.NewServer(newHTTPMux(rdb))
	defer srv.Close()

	// Minimal required fields only
	body := map[string]any{
		"transaction_id": "t1",
		"subscriber_url": "https://s",
		"form_action_id": "f1",
	}
	b, _ := json.Marshal(body)

	resp, err := http.Post(srv.URL+"/html-form", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST request error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %v, want %v", resp.StatusCode, http.StatusOK)
	}

	val, err := rdb.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(val), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	apiList := got["apiList"].([]any)
	entry := apiList[0].(map[string]any)

	if entry["entryType"] != "FORM" {
		t.Errorf("entryType = %v, want FORM", entry["entryType"])
	}
	if entry["formId"] != "f1" {
		t.Errorf("formId = %v, want f1", entry["formId"])
	}
}

func TestLoggingMiddleware(t *testing.T) {
	// Test that logging middleware doesn't break the handler
	called := false
	handler := loggingMiddleware(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	})

	req := httptest.NewRequest("POST", "/test", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if !called {
		t.Error("handler was not called")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %v, want %v", w.Code, http.StatusOK)
	}
}

func TestLoggingResponseWriter(t *testing.T) {
	w := httptest.NewRecorder()
	lrw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}

	lrw.WriteHeader(http.StatusCreated)
	if lrw.statusCode != http.StatusCreated {
		t.Errorf("statusCode = %v, want %v", lrw.statusCode, http.StatusCreated)
	}
	if w.Code != http.StatusCreated {
		t.Errorf("underlying ResponseWriter Code = %v, want %v", w.Code, http.StatusCreated)
	}
}
