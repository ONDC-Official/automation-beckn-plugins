package ondcvalidator

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryCache struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemoryCache() *memoryCache {
	return &memoryCache{m: make(map[string]string)}
}

func (c *memoryCache) Get(ctx context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.m[key]
	if !ok {
		return "", errors.New("key not found")
	}
	return value, nil
}

func (c *memoryCache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = value
	return nil
}

func (c *memoryCache) Delete(ctx context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
	return nil
}

func (c *memoryCache) Clear(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = make(map[string]string)
	return nil
}

func TestValidatePayload_WithProvidedSearchPayload_ConfigFalseFalse(t *testing.T) {
	ctx := context.Background()
	cache := newMemoryCache()

	validator, _, err := New(ctx, cache, &Config{
		StateFullValidations: false,
		DebugMode:            false,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	parsedURL, err := url.Parse("https://example.com/search")
	if err != nil {
		t.Fatalf("url.Parse error = %v", err)
	}

	payload := []byte(`{
		"context": {
			"domain": "ONDC:RET10",
			"action": "search",
			"country": "IND",
			"city": "std:080",
			"core_version": "1.2.5",
			"bap_id": "dev-automation.ondc.org",
			"bap_uri": "https://197c2e5e5beb.ngrok-free.app",
			"transaction_id": "txn_g1766987315",
			"message_id": "msg_f1766987315",
			"timestamp": "2025-12-29T05:48:34.896Z",
			"ttl": "PT30S"
		},
		"message": {
			"intent": {
				"category": {
					"id": "Food and Beverages"
				},
				"fulfillment": {
					"type": "Delivery",
					"end": {
						"location": {
							"gps": "12.9715987,77.5945627"
						}
					}
				},
				"payment": {
					"@ondc/org/buyer_app_finder_fee_type": "percent",
					"@ondc/org/buyer_app_finder_fee_amount": "2"
				}
			}
		}
	}`)

	validateErr := validator.ValidatePayload(ctx, parsedURL, payload)
	// Print the result (shows up when running `go test -v`).
	t.Logf("ValidatePayload() error: %v", validateErr)

	if validateErr == nil {
		// With the current implementation, only `transaction_id` is unmarshaled into `payloadType`,
		// so most spec validations typically fail. If this starts passing, it likely means the
		// validator (or validationpkg rules) changed and the expectation should be revisited.
		t.Fatalf("ValidatePayload() expected error, got nil")
	}

	// Ensure we did not fail at JSON parsing / missing transaction_id.
	if strings.Contains(validateErr.Error(), "invalid payload") {
		t.Fatalf("ValidatePayload() returned unexpected parsing error: %v", validateErr)
	}

	// ValidatePayload can fail in two ways: returning an error from validationpkg,
	// or returning a formatted L1 validation failure message.
	if !strings.Contains(validateErr.Error(), "validation error") && !strings.Contains(validateErr.Error(), "L1 validation failed") {
		t.Fatalf("ValidatePayload() error did not match expected patterns. got: %v", validateErr)
	}
}


