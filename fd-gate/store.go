package fdgate

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/redis/go-redis/v9"
)

// Store is the shared key-value store fd-gate keeps its replay records and object index in
// (Redis in production).
type Store interface {
	// SetNX stores value under key only if key is absent, and reports whether it did.
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	// Get returns the value under key; ErrNotFound when there is none.
	Get(ctx context.Context, key string) ([]byte, error)
	// Set stores value under key, replacing any value.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// Del removes key.
	Del(ctx context.Context, key string) error
}

// ErrNotFound is returned by Store.Get for an absent key.
var ErrNotFound = errors.New("fd-gate: key not found")

// RedisStore is a Store over go-redis.
type RedisStore struct{ Client redis.UniversalClient }

// NewRedisStore connects lazily to the Redis at addr.
func NewRedisStore(addr, username, password string) *RedisStore {
	return &RedisStore{Client: redis.NewClient(&redis.Options{
		Addr:            addr,
		Username:        username,
		Password:        password,
		MaxRetries:      3,
		MinRetryBackoff: 50 * time.Millisecond,
		MaxRetryBackoff: 500 * time.Millisecond,
		DialTimeout:     3 * time.Second,
		ReadTimeout:     2 * time.Second,
		WriteTimeout:    2 * time.Second,
	})}
}

// SetNX implements Store.
func (s *RedisStore) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	return s.Client.SetNX(ctx, key, value, ttl).Result()
}

// Get implements Store.
func (s *RedisStore) Get(ctx context.Context, key string) ([]byte, error) {
	b, err := s.Client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNotFound
	}
	return b, err
}

// Set implements Store.
func (s *RedisStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return s.Client.Set(ctx, key, value, ttl).Err()
}

// Del implements Store.
func (s *RedisStore) Del(ctx context.Context, key string) error {
	return s.Client.Del(ctx, key).Err()
}

// ObjectRef is what the object index records for an FD identifier: the transaction it
// belongs to and the endpoints of that transaction.
type ObjectRef struct {
	TransactionID string `json:"transaction_id"`
	BapID         string `json:"bap_id"`
	BapURI        string `json:"bap_uri"`
	BppID         string `json:"bpp_id"`
	BppURI        string `json:"bpp_uri"`
}

// ObjectKey is the index key of an identifier: FD_OBJ::<domain>::<idType>::<id>.
func ObjectKey(domain, idType, id string) string {
	return "FD_OBJ::" + domain + "::" + idType + "::" + id
}

// ReplayKey is the replay record key of a POST:
// FD_REPLAY::<domain>::<signerSubscriberId>::<action>::<message_id>.
func ReplayKey(domain, signer, action, messageID string) string {
	return "FD_REPLAY::" + domain + "::" + signer + "::" + action + "::" + messageID
}

// PathParamsKey is where np mode leaves a participant's request's path parameters for the
// playground: FD_PATH_PARAMS::<domain>::<transaction_id>::<message_id>.
func PathParamsKey(domain, transactionID, messageID string) string {
	return "FD_PATH_PARAMS::" + domain + "::" + transactionID + "::" + messageID
}

// recordPathParams leaves the path parameters of a participant's POST where the playground
// finds them. ONIX forwards an asynchronous request to the mock with its body alone, so
// X-Pwb-Fd-Path-Params never reaches it, and a path parameter that is not a Correlation
// member — consent_withdraw's consentId, the only way that request names its consent — would
// be lost. Kept as long as the replay record. Failures are logged, never fatal.
func (g *Gate) recordPathParams(ctx context.Context, params map[string]string, b *fdBody) {
	if g.store == nil || b == nil || len(params) == 0 {
		return
	}
	transactionID, messageID := b.str("transaction_id"), b.str("message_id")
	if transactionID == "" || messageID == "" {
		return
	}
	value, err := json.Marshal(params)
	if err != nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if err := g.store.Set(ctx, PathParamsKey(g.table.Domain, transactionID, messageID), value, g.cfg.ReplayTTL); err != nil {
		log.Errorf(ctx, err, "fd-gate: cannot record the path parameters of %s", messageID)
	}
}

// indexBody records every string identifier in the body's fdEnvelope.correlation against
// the body's context. Failures are logged, never fatal: the index only serves later GETs.
func (g *Gate) indexBody(ctx context.Context, b *fdBody) {
	if g.store == nil || b == nil || len(b.corr) == 0 {
		return
	}
	ref := ObjectRef{
		TransactionID: b.str("transaction_id"),
		BapID:         b.str("bap_id"), BapURI: b.str("bap_uri"),
		BppID: b.str("bpp_id"), BppURI: b.str("bpp_uri"),
	}
	if ref.TransactionID == "" {
		return
	}
	value, err := json.Marshal(ref)
	if err != nil {
		return
	}
	keys := make([]string, 0, len(b.corr))
	for k := range b.corr {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ctx = context.WithoutCancel(ctx)
	for _, k := range keys {
		id, ok := b.corr[k].(string)
		if !ok || id == "" {
			continue
		}
		if err := g.store.Set(ctx, ObjectKey(g.table.Domain, k, id), value, g.cfg.ObjectTTL); err != nil {
			log.Errorf(ctx, err, "fd-gate: cannot index %s %s", k, id)
		}
	}
}

// lookupObject returns the indexed reference of an identifier.
func (g *Gate) lookupObject(ctx context.Context, idType, id string) (*ObjectRef, error) {
	data, err := g.store.Get(ctx, ObjectKey(g.table.Domain, idType, id))
	if err != nil {
		return nil, err
	}
	var ref ObjectRef
	if err := json.Unmarshal(data, &ref); err != nil {
		return nil, err
	}
	return &ref, nil
}
