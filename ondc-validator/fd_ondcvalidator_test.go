package ondcvalidator

import (
	"context"
	"net/url"
	"sync"
	"testing"
	"time"
)

// ttlCache is a memoryCache that also records the TTL of every Set.
type ttlCache struct {
	*memoryCache
	mu   sync.Mutex
	ttls map[string]time.Duration
}

func newTTLCache() *ttlCache {
	return &ttlCache{memoryCache: newMemoryCache(), ttls: map[string]time.Duration{}}
}

func (c *ttlCache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	c.mu.Lock()
	c.ttls[key] = ttl
	c.mu.Unlock()
	return c.memoryCache.Set(ctx, key, value, ttl)
}

func searchPayloadFor(domain string) []byte {
	return []byte(`{"context":{"domain":"` + domain + `","action":"search","core_version":"1.2.5",` +
		`"transaction_id":"txn-1","message_id":"msg-1","timestamp":"2026-01-01T00:00:00.000Z"},` +
		`"message":{"intent":{}}}`)
}

// The stateful store's TTL, chosen from the payload as SaveValidationData and ValidatePayload
// parse it: a real duration for FD payloads, Beckn's historical 60*5 (a time.Duration of 300 ns)
// for everything else. (The vendored example validationpkg saves no keys of its own, so the
// store is driven directly.)
func TestStatefulStoreTTLByDomain(t *testing.T) {
	tests := []struct {
		domain string
		want   time.Duration
	}{
		{domain: "ONDC:FIS14:FD", want: 24 * time.Hour},
		{domain: "ONDC:RET10", want: time.Duration(60 * 5)},
		{domain: "ONDC:FIS14", want: time.Duration(60 * 5)},
		{domain: "", want: time.Duration(60 * 5)},
	}
	for _, tt := range tests {
		t.Run(tt.domain, func(t *testing.T) {
			cache := newTTLCache()
			validator, _, err := New(context.Background(), cache, &Config{StateFullValidations: true})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			_, uniqueKey, env, err := convertToPayload(searchPayloadFor(tt.domain))
			if err != nil {
				t.Fatalf("convertToPayload() error = %v", err)
			}
			if err := validator.(*ondcValidator).storageFor(env).SaveKey(uniqueKey, "k", "v"); err != nil {
				t.Fatalf("SaveKey() error = %v", err)
			}
			if got := cache.ttls["txn-1:k"]; got != tt.want {
				t.Fatalf("TTL = %v, want %v", got, tt.want)
			}
		})
	}
}

// SaveValidationData still works end to end for both kinds of payload.
func TestSaveValidationDataByDomain(t *testing.T) {
	for _, domain := range []string{"ONDC:FIS14:FD", "ONDC:RET10"} {
		validator, _, _ := New(context.Background(), newTTLCache(), &Config{StateFullValidations: true})
		u, _ := url.Parse("https://example.com/search")
		if err := validator.SaveValidationData(context.Background(), u, searchPayloadFor(domain)); err != nil {
			t.Fatalf("%s: SaveValidationData() error = %v", domain, err)
		}
	}
}

// An FD store reads back what it saved, through cacheWrapper's own read methods.
func TestFDStorageReadsBack(t *testing.T) {
	cache := newTTLCache()
	v := &ondcValidator{config: &Config{}, cache: cache, storage: &cacheWrapper{cache: cache}}
	var env payloadEnvelope
	env.Context.Domain = fdProtocolDomain
	store := v.storageFor(env)
	if store == v.storage {
		t.Fatalf("storageFor(FD) returned the Beckn store")
	}
	if err := store.SaveKey("txn-1", "k", "v"); err != nil {
		t.Fatalf("SaveKey() error = %v", err)
	}
	if got, err := store.GetKey("txn-1", "k"); err != nil || got != "v" {
		t.Fatalf("GetKey() = %q, %v; want \"v\", nil", got, err)
	}
	if ok, err := store.KeyExists("txn-1", "k"); err != nil || !ok {
		t.Fatalf("KeyExists() = %t, %v; want true, nil", ok, err)
	}

	env.Context.Domain = "ONDC:RET10"
	if v.storageFor(env) != v.storage {
		t.Fatalf("storageFor(non-FD) must return the existing store unchanged")
	}
}
