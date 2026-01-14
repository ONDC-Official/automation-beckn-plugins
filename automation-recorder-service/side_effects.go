package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

func sendLogsToNO(ctx context.Context, cfg config, client *http.Client, d derivedFields, requestBody, responseBody map[string]any) error {
	fmt.Printf("[NO] Sending logs to Network Observability for transaction: %s\n", d.TransactionID)
	if strings.TrimSpace(cfg.NOURL) == "" {
		fmt.Printf("[NO] Skipping: NO URL not configured\n")
		return nil
	}
	if len(cfg.NOEnabledIn) > 0 && !cfg.NOEnabledIn[cfg.Env] {
		fmt.Printf("[NO] Skipping: Not enabled for environment '%s'\n", cfg.Env)
		return nil
	}
	if client == nil {
		client = http.DefaultClient
	}
	client.Timeout = cfg.NOTimeout

	endpoint, err := url.JoinPath(cfg.NOURL, "/v1/api/push-txn-logs")
	if err != nil {
		return err
	}

	common := map[string]any{
		"payloadId":     d.PayloadID,
		"transactionId": d.TransactionID,
		"subscriberUrl": strings.TrimRight(d.SubscriberURL, "/"),
		"action":        d.Action,
		"timestamp":     d.Timestamp,
		"apiName":       d.APIName,
	}

	// Send request log.
	fmt.Printf("[NO] Posting request log to %s\n", endpoint)
	if err := postJSON(ctx, client, endpoint, cfg.NOToken, mergeMaps(common, map[string]any{"type": "request", "request": requestBody})); err != nil {
		fmt.Printf("[NO] ERROR: Failed to post request log: %v\n", err)
		return err
	}
	fmt.Printf("[NO] Request log posted successfully\n")
	
	// Send response log.
	fmt.Printf("[NO] Posting response log to %s\n", endpoint)
	if err := postJSON(ctx, client, endpoint, cfg.NOToken, mergeMaps(common, map[string]any{"type": "response", "response": responseBody, "statusCode": d.StatusCode})); err != nil {
		fmt.Printf("[NO] ERROR: Failed to post response log: %v\n", err)
		return err
	}
	fmt.Printf("[NO] Response log posted successfully\n")
	return nil
}

func savePayloadToDB(ctx context.Context, cfg config, client *http.Client, rdb *redis.Client, d derivedFields, requestBody, responseBody map[string]any, additionalData map[string]any) error {
	fmt.Printf("[DB] Saving payload to database for transaction: %s\n", d.TransactionID)
	if strings.TrimSpace(cfg.DBBaseURL) == "" {
		fmt.Printf("[DB] Skipping: DB URL not configured\n")
		return nil
	}
	if len(cfg.DBEnabledIn) > 0 && !cfg.DBEnabledIn[cfg.Env] {
		fmt.Printf("[DB] Skipping: Not enabled for environment '%s'\n", cfg.Env)
		return nil
	}
	if client == nil {
		client = http.DefaultClient
	}
	client.Timeout = cfg.DBTimeout

	// Load transaction from Redis; if it doesn't exist, match TS behavior and skip DB save.
	fmt.Printf("[DB] Loading transaction from Redis...\n")
	txn, err := loadTransactionMap(ctx, rdb, createTransactionKey(d.TransactionID, d.SubscriberURL))
	if err != nil {
		fmt.Printf("[DB] ERROR: Failed to load transaction: %v\n", err)
		return err
	}
	if txn == nil {
		fmt.Printf("[DB] Transaction not found in Redis, skipping DB save\n")
		return nil
	}
	fmt.Printf("[DB] Transaction loaded successfully\n")

	sessionId := strings.TrimSpace(getString(txn, "sessionId"))
	flowId := strings.TrimSpace(getString(txn, "flowId"))
	npType := strings.TrimSpace(getString(txn, "subscriberType"))

	if sessionId == "" {
		// Matches TS: key = sha256(transactionKey)
		sessionId = sha256Hex(createTransactionKey(d.TransactionID, d.SubscriberURL))
	}

	// Check/Create session in DB
	checkURL, err := url.JoinPath(cfg.DBBaseURL, cfg.DBSessionPath, "check", sessionId)
	if err != nil {
		return err
	}
	exists, err := getBoolJSON(ctx, client, checkURL, cfg.DBAPIKey)
	if err != nil {
		return err
	}
	if !exists {
		createURL, err := url.JoinPath(cfg.DBBaseURL, cfg.DBSessionPath)
		if err != nil {
			return err
		}
		domain := getContextString(requestBody, "domain")
		version := getContextString(requestBody, "version")
		if strings.TrimSpace(version) == "" {
			version = getContextString(requestBody, "core_version")
		}
		sessionPayload := map[string]any{
			"sessionId":     sessionId,
			"npType":        npType,
			"npId":          strings.TrimSpace(d.SubscriberURL),
			"domain":        domain,
			"version":       version,
			"sessionType":   "AUTOMATION",
			"sessionActive": true,
		}
		if err := postJSONWithAPIKey(ctx, client, createURL, cfg.DBAPIKey, sessionPayload); err != nil {
			fmt.Printf("[DB] ERROR: Failed to create session in DB: %v\n", err)
			return err
		}
	}

	// Save payload
	payloadURL, err := url.JoinPath(cfg.DBBaseURL, cfg.DBPayloadPath)
	if err != nil {
		return err
	}

	action := strings.ToUpper(strings.TrimSpace(d.Action))
	messageID := strings.TrimSpace(d.MessageID)
	if messageID == "" {
		messageID = getContextString(requestBody, "message_id")
	}

	 // Extract request headers from additionalData and convert to JSON string
    var reqHeaderStr string
    if additionalData != nil {
        var headerData any
        if v, ok := additionalData["reqHeader"]; ok {
            headerData = v
        } else if v, ok := additionalData["req_header"]; ok {
            headerData = v
        } else if v, ok := additionalData["request_headers"]; ok {
            headerData = v
        }
        
        if headerData != nil {
            // Convert to JSON string
            if headerBytes, err := json.Marshal(headerData); err == nil {
                reqHeaderStr = string(headerBytes)
            } else {
                fmt.Printf("[DB] WARNING: Failed to marshal request headers: %v\n", err)
                reqHeaderStr = "{}"
            }
        } else {
            reqHeaderStr = "{}"
        }
    } else {
        reqHeaderStr = "{}"
    }
	fmt.Printf("[DB] Request headers for DB payload: %+v\n", reqHeaderStr)

	requestPayload := map[string]any{
		"messageId":     messageID,
		"transactionId": strings.TrimSpace(d.TransactionID),
		"payloadId":     strings.TrimSpace(d.PayloadID),
		"action":        action,
		"bppId":         getContextString(requestBody, "bpp_id"),
		"bapId":         getContextString(requestBody, "bap_id"),
		"reqHeader":     reqHeaderStr,
		"jsonRequest":   requestBody,
		"jsonResponse":  map[string]any{"response": responseBody},
		"httpStatus":    d.StatusCode,
		"flowId":        flowId,
		"sessionDetails": map[string]any{
			"sessionId": sessionId,
		},
	}

	return postJSONWithAPIKey(ctx, client, payloadURL, cfg.DBAPIKey, requestPayload)
}

func getContextString(requestBody map[string]any, key string) string {
	ctxObj, _ := requestBody["context"].(map[string]any)
	if ctxObj == nil {
		return ""
	}
	return getString(ctxObj, key)
}

func getBoolJSON(ctx context.Context, client *http.Client, endpoint string, apiKey string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("x-api-key", apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("http %s returned %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var v any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return false, err
	}
	switch t := v.(type) {
	case bool:
		return t, nil
	case map[string]any:
		if inner, ok := t["data"].(bool); ok {
			return inner, nil
		}
	}
	return false, nil
}

func postJSON(ctx context.Context, client *http.Client, endpoint string, bearerToken string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(bearerToken) != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http %s returned %d", endpoint, resp.StatusCode)
	}
	return nil
}

func postJSONWithAPIKey(ctx context.Context, client *http.Client, endpoint string, apiKey string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("x-api-key", apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("http %s returned %d and message %s", endpoint, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func getStatus(ctx context.Context, client *http.Client, endpoint string, apiKey string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("x-api-key", apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// Preserve the old dependency behavior: callers might pass nil client.
func ensureHTTPClient(c *http.Client) *http.Client {
	if c == nil {
		return http.DefaultClient
	}
	return c
}

// Helpers so lints don't complain about unused imports in some builds.
var _ = errors.Is
var _ = time.Second
