package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type formHandler struct {
	rdb *redis.Client
}

func newHTTPMux(rdb *redis.Client) *http.ServeMux {
	mux := http.NewServeMux()
	fh := &formHandler{rdb: rdb}
	mux.HandleFunc("/html-form", loggingMiddleware(fh.htmlForm))
	return mux
}

// loggingMiddleware logs HTTP requests
func loggingMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx := r.Context()
		
		fmt.Printf("[HTTP] --> %s %s from %s\n", r.Method, r.URL.Path, r.RemoteAddr)
		fmt.Printf("[HTTP] User-Agent: %s\n", r.UserAgent())
		fmt.Printf("[HTTP] Content-Type: %s\n", r.Header.Get("Content-Type"))
		
		// Wrap response writer to capture status code
		lrw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		
		next(lrw, r)
		
		duration := time.Since(start)
		fmt.Printf("[HTTP] <-- %d %s (took %v)\n", lrw.statusCode, http.StatusText(lrw.statusCode), duration)
		
		if lrw.statusCode >= 400 {
			fmt.Printf("[HTTP] ERROR: Request failed with status %d\n", lrw.statusCode)
		}
		_ = ctx
	}
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

func (h *formHandler) htmlForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	fmt.Printf("[FORM] Processing form submission request\n")
	
	// Mirror Express route: POST only.
	if r.Method != http.MethodPost {
		fmt.Printf("[FORM] ERROR: Invalid method %s, only POST allowed\n", r.Method)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var formData map[string]any
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if err := dec.Decode(&formData); err != nil || formData == nil {
		fmt.Printf("[FORM] ERROR: Failed to decode form data: %v\n", err)
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}
	
	fmt.Printf("[FORM] Received form data with %d fields\n", len(formData))
	_ = ctx

	transactionID, ok1 := formData["transaction_id"].(string)
	subscriberURL, ok2 := formData["subscriber_url"].(string)
	formActionID, ok3 := formData["form_action_id"].(string)
	if !ok1 || !ok2 || !ok3 {
		fmt.Printf("[FORM] ERROR: Missing required fields - transaction_id: %v, subscriber_url: %v, form_action_id: %v\n", ok1, ok2, ok3)
		http.Error(w, "Missing required form fields: transaction_id, subscriber_url, or form_action_id\n                should be strings", http.StatusBadRequest)
		return
	}

	fmt.Printf("[FORM] Transaction ID: %s, Subscriber URL: %s, Form Action ID: %s\n", transactionID, subscriberURL, formActionID)

	formType, _ := formData["form_type"].(string)

	// TS controller passes formData.submissionId (camelCase)
	submissionID, _ := formData["submissionId"].(string)
	if strings.TrimSpace(submissionID) == "" {
		submissionID, _ = formData["submission_id"].(string)
	}

	errVal := formData["error"]

	fmt.Printf("[FORM] Appending form entry to Redis...\n")
	if err := appendFormEntryAtomically(r.Context(), h.rdb, transactionID, subscriberURL, formActionID, formType, submissionID, errVal); err != nil {
		// TS controller catches and returns 500.
		fmt.Printf("[FORM] ERROR: Failed to append form entry: %v\n", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	fmt.Printf("[FORM] Form submitted successfully\n")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Form submitted successfully"))
}

func appendFormEntryAtomically(ctx context.Context, rdb *redis.Client, transactionID, subscriberURL, formID, formType, submissionID string, errVal any) error {
	key := createTransactionKey(transactionID, subscriberURL)
	if key == "" {
		return fmt.Errorf("invalid key")
	}
	if rdb == nil {
		return fmt.Errorf("redis not configured")
	}

	const maxAttempts = 8
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err := rdb.Watch(ctx, func(tx *redis.Tx) error {
			val, err := tx.Get(ctx, key).Result()
			if err != nil {
				if errors.Is(err, redis.Nil) {
					return errNotFound
				}
				return err
			}

			ttl, _ := tx.TTL(ctx, key).Result()

			var txn map[string]any
			if err := json.Unmarshal([]byte(val), &txn); err != nil {
				return err
			}
			if txn == nil {
				txn = map[string]any{}
			}

			apiList, ok := txn["apiList"].([]any)
			if !ok || apiList == nil {
				apiList = []any{}
			}

			entry := map[string]any{
				"entryType": "FORM",
				"formId":    strings.TrimSpace(formID),
				"timestamp": tsISOStringNow(),
				"formType":  strings.TrimSpace(formType),
			}
			if strings.TrimSpace(submissionID) != "" {
				entry["submissionId"] = strings.TrimSpace(submissionID)
			}
			if errVal != nil {
				entry["error"] = errVal
			}

			apiList = append(apiList, entry)
			txn["apiList"] = apiList

			updated, err := json.Marshal(txn)
			if err != nil {
				return err
			}

			pipe := tx.TxPipeline()
			if ttl > 0 {
				pipe.Set(ctx, key, string(updated), ttl)
			} else {
				// ttl == -1 means persistent key; ttl == -2 shouldn't happen because GET succeeded.
				pipe.Set(ctx, key, string(updated), 0)
			}
			_, err = pipe.Exec(ctx)
			return err
		}, key)

		if err == nil {
			return nil
		}
		if errors.Is(err, errNotFound) {
			return err
		}
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		return err
	}
	return errAborted
}

// JS Date().toISOString() shape: 2006-01-02T15:04:05.000Z
func tsISOStringNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}
