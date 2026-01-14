package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/status"
)

var (
	errNotFound = errors.New("transaction not found")
	errAborted  = errors.New("aborted")
)

func createTransactionKey(transactionID, subscriberURL string) string {
	transactionID = strings.TrimSpace(transactionID)
	subscriberURL = strings.TrimSpace(subscriberURL)
	subscriberURL = strings.TrimRight(subscriberURL, "/")
	if transactionID == "" || subscriberURL == "" {
		return ""
	}
	return transactionID + "::" + subscriberURL
}

type cacheAppendInput struct {
	PayloadID     string
	TransactionID string
	MessageID     string
	SubscriberURL string
	Action        string
	Timestamp     string
	TTLSecs       int64
	Response      any
}

func updateTransactionAtomically(ctx context.Context, rdb *redis.Client, key string, in *cacheAppendInput, cacheTTL time.Duration) error {
	const maxAttempts = 8
	fmt.Printf("[CACHE] Updating transaction atomically for key: %s\n", key)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			fmt.Printf("[CACHE] Retry attempt %d/%d for key: %s\n", attempt+1, maxAttempts, key)
		}
		err := rdb.Watch(ctx, func(tx *redis.Tx) error {
			val, err := tx.Get(ctx, key).Result()
			if err != nil {
				if errors.Is(err, redis.Nil) {
					fmt.Printf("[CACHE] ERROR: Transaction not found for key: %s\n", key)
					return errNotFound
				}
				fmt.Printf("[CACHE] ERROR: Failed to get transaction from Redis: %v\n", err)
				return err
			}

			fmt.Printf("[CACHE] Retrieved transaction from Redis, size: %d bytes\n", len(val))
			var txn map[string]any
			if err := json.Unmarshal([]byte(val), &txn); err != nil {
				fmt.Printf("[CACHE] ERROR: Failed to unmarshal transaction: %v\n", err)
				return err
			}
			if txn == nil {
				txn = map[string]any{}
			}

			// IMPORTANT: Keep cache JSON compatible with the shared TS/Go cache types.
			// Key is: transactionId::subscriberUrl
			// Value is a TransactionCache containing apiList entries shaped like ApiData.
			txn["latestAction"] = strings.TrimSpace(in.Action)
			txn["latestTimestamp"] = strings.TrimSpace(in.Timestamp)

			// Maintain messageIds (used for duplicate message_id checks).
			messageID := strings.TrimSpace(in.MessageID)
			if messageID != "" {
				var msgIDs []string
				switch v := txn["messageIds"].(type) {
				case []any:
					for _, it := range v {
						if s, ok := it.(string); ok {
							msgIDs = append(msgIDs, s)
						}
					}
				case []string:
					msgIDs = append(msgIDs, v...)
				}
				seen := false
				for _, s := range msgIDs {
					if s == messageID {
						seen = true
						break
					}
				}
				if !seen {
					msgIDs = append(msgIDs, messageID)
				}
				// Store back as JSON array of strings.
				out := make([]any, 0, len(msgIDs))
				for _, s := range msgIDs {
					out = append(out, s)
				}
				txn["messageIds"] = out
			}

			apiList, ok := txn["apiList"].([]any)
			if !ok || apiList == nil {
				apiList = []any{}
			}

			apiEntry := map[string]any{
				"entryType":     "API",
				"action":        strings.TrimSpace(in.Action),
				"payloadId":     strings.TrimSpace(in.PayloadID),
				"messageId":     messageID,
				"response":      in.Response,
				"timestamp":     strings.TrimSpace(in.Timestamp),
				"realTimestamp": time.Now().UTC().Format(time.RFC3339Nano),
			}
			if in.TTLSecs > 0 {
				apiEntry["ttl"] = in.TTLSecs
			}
			apiList = append(apiList, apiEntry)
			txn["apiList"] = apiList

			updated, err := json.Marshal(txn)
			if err != nil {
				return err
			}

			pipe := tx.TxPipeline()
			if cacheTTL > 0 {
				pipe.Set(ctx, key, string(updated), cacheTTL)
			} else {
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
		// Conflict retry.
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		// If we returned a gRPC status error (e.g. invalid JSON), preserve it.
		st, ok := status.FromError(err)
		if ok {
			return st.Err()
		}
		return err
	}
	return errAborted
}

func createFlowStatusCacheKey(transactionID, subscriberURL string) string {
	transactionID = strings.TrimSpace(transactionID)
	subscriberURL = strings.TrimSpace(subscriberURL)
	if transactionID == "" || subscriberURL == "" {
		return ""
	}
	// Matches TS: `FLOW_STATUS_${transactionId}::${subscriberUrl}` (no trailing-slash trimming)
	return "FLOW_STATUS_" + transactionID + "::" + subscriberURL
}

func setFlowStatusIfExists(ctx context.Context, rdb *redis.Client, transactionID, subscriberURL, statusValue string, ttl time.Duration) error {
	if rdb == nil {
		return nil
	}
	key := createFlowStatusCacheKey(transactionID, subscriberURL)
	if key == "" {
		return nil
	}
	exists, err := rdb.Exists(ctx, key).Result()
	if err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	b, err := json.Marshal(map[string]any{"status": statusValue})
	if err != nil {
		return err
	}
	return rdb.Set(ctx, key, string(b), ttl).Err()
}

func loadTransactionMap(ctx context.Context, rdb *redis.Client, key string) (map[string]any, error) {
	if rdb == nil || strings.TrimSpace(key) == "" {
		return nil, nil
	}
	val, err := rdb.Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(val), &out); err != nil {
		return nil, err
	}
	return out, nil
}
