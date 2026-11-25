package transactioncache

import (
	"context"
	"errors"
	"testing"
	"time"
)

type memCache struct {
	m    map[string]string
	ttls map[string]time.Duration
}

func newMemCache() *memCache {
	return &memCache{m: map[string]string{}, ttls: map[string]time.Duration{}}
}

func (c *memCache) Get(_ context.Context, key string) (string, error) {
	v, ok := c.m[key]
	if !ok {
		return "", errors.New("redis: nil")
	}
	return v, nil
}

func (c *memCache) Set(_ context.Context, key, value string, ttl time.Duration) error {
	c.m[key] = value
	c.ttls[key] = ttl
	return nil
}

func (c *memCache) Delete(_ context.Context, key string) error { delete(c.m, key); return nil }
func (c *memCache) Clear(_ context.Context) error              { c.m = map[string]string{}; return nil }

func TestFDBodyDigestRoundTrip(t *testing.T) {
	ctx := context.Background()
	cache := newMemCache()
	s := &Service{Cache: cache}
	txnKey := s.CreateTransactionKey("txn-1", "https://np.example/fd/")

	if got := s.LoadFDBodyDigest(ctx, txnKey, "m1"); got != "" {
		t.Fatalf("LoadFDBodyDigest() before save = %q, want \"\"", got)
	}
	if err := s.SaveFDBodyDigest(ctx, txnKey, "m1", "abc"); err != nil {
		t.Fatalf("SaveFDBodyDigest() error = %v", err)
	}
	if got := s.LoadFDBodyDigest(ctx, txnKey, "m1"); got != "abc" {
		t.Fatalf("LoadFDBodyDigest() = %q, want abc", got)
	}
	if got := s.LoadFDBodyDigest(ctx, txnKey, "m2"); got != "" {
		t.Fatalf("LoadFDBodyDigest(other message) = %q, want \"\"", got)
	}
	key := s.FDBodyDigestKey(txnKey, "m1")
	if want := "FD_BODY_SHA256::txn-1::https://np.example/fd::m1"; key != want {
		t.Fatalf("FDBodyDigestKey() = %q, want %q", key, want)
	}
	if ttl := cache.ttls[key]; ttl != 0 {
		t.Fatalf("digest TTL = %v, want 0 (lives as long as the transaction)", ttl)
	}
}
