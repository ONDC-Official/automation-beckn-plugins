package transactioncache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"ondcworkbench/internal/apiservice"
	cache "ondcworkbench/internal/ondc/cache"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

type Service struct {
	Cache definition.Cache
}

func (s *Service) TryLoadTransaction(ctx context.Context, transactionID, subscriberURL string) (*cache.TransactionCache, error) {
	key := s.CreateTransactionKey(transactionID, subscriberURL)
	exists, err := s.CheckIfTransactionExists(ctx, key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return s.LoadTransactionThatExists(ctx, key)
}

func (s *Service) CheckIfTransactionExists(ctx context.Context, transSubKey string) (bool, error) {
	if s.Cache == nil {
		return false, errors.New("cache is nil")
	}
	raw, err := s.Cache.Get(ctx, transSubKey)
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(raw) != "", nil
}

func (s *Service) LoadTransactionThatExists(ctx context.Context, transSubKey string) (*cache.TransactionCache, error) {
	if s.Cache == nil {
		return nil, errors.New("cache is nil")
	}
	raw, err := s.Cache.Get(ctx, transSubKey)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("transaction with id %s not found", transSubKey)
	}

	var txn cache.TransactionCache
	if err := json.Unmarshal([]byte(raw), &txn); err != nil {
		return nil, err
	}
	if txn.MessageIds == nil {
		txn.MessageIds = []string{}
	}
	if txn.ApiList == nil {
		txn.ApiList = []any{}
	}
	if txn.ReferenceData == nil {
		txn.ReferenceData = map[string]any{}
	}
	return &txn, nil
}

// UpdateTransactionCache appends an API entry to an existing transaction.
//
// Notes:
// - This mirrors your TS code but uses definition.Cache.Set which requires a TTL.
// - If you don't want expiration, pass ttl=0.
func (s *Service) UpdateTransactionCache(
	ctx context.Context,
	payloadID string,
	transactionID string,
	action string,
	messageID string,
	timestamp string,
	ttlSeconds int64,
	responseBody any,
	subscriberURL string,
	cacheTTL time.Duration,
) error {
	if strings.TrimSpace(subscriberURL) == "" {
		return errors.New("subscriber url not provided")
	}
	key := s.CreateTransactionKey(transactionID, subscriberURL)

	exists, err := s.CheckIfTransactionExists(ctx, key)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("transaction not found")
	}

	txn, err := s.LoadTransactionThatExists(ctx, key)
	if err != nil {
		return err
	}

	ttlPtr := &ttlSeconds
	txn.ApiList = append(txn.ApiList, cache.ApiData{
		EntryType:     "API",
		Action:        action,
		MessageId:     messageID,
		PayloadId:     payloadID,
		Response:      responseBody,
		Timestamp:     timestamp,
		RealTimestamp: time.Now().UTC().Format(time.RFC3339Nano),
		TTL:           ttlPtr,
	})
	txn.LatestAction = action
	txn.LatestTimestamp = timestamp

	bytes, err := json.Marshal(txn)
	if err != nil {
		return err
	}
	return s.Cache.Set(ctx, key, string(bytes), cacheTTL)
}

// CreateTransaction overwrites any existing transaction at transSubKey (same as TS comment).
func (s *Service) CreateTransaction(ctx context.Context, transSubKey string, req *apiservice.WorkbenchRequestData, cacheTTL time.Duration) (*cache.TransactionCache, error) {
	if s.Cache == nil {
		return nil, errors.New("cache is nil")
	}

	subType := apiservice.GetSubscriberTypeFromModuleType(apiservice.ModuleRole(req.ModuleType))

	txn := &cache.TransactionCache{
		SessionId:       req.SessionID,
		FlowId:          req.FlowID,
		LatestAction:    "",
		SubscriberType:  string(subType),
		LatestTimestamp: time.Unix(0, 0).UTC().Format(time.RFC3339Nano),
		MessageIds:      []string{},
		ApiList:         []any{},
		ReferenceData:   map[string]any{},
	}

	bytes, err := json.Marshal(txn)
	if err != nil {
		return nil, err
	}
	if err := s.Cache.Set(ctx, transSubKey, string(bytes), cacheTTL); err != nil {
		return nil, err
	}
	return txn, nil
}

func (s *Service) CreateTransactionKey(transactionID, subscriberURL string) string {
	subscriberURL = strings.TrimSpace(subscriberURL)
	subscriberURL = strings.TrimSuffix(subscriberURL, "/")
	return strings.TrimSpace(transactionID) + "::" + subscriberURL
}

func (s *Service) OverrideTransaction(ctx context.Context, subscriberURL, transactionID string, txn *cache.TransactionCache, cacheTTL time.Duration) error {
	if s.Cache == nil {
		return errors.New("cache is nil")
	}
	key := s.CreateTransactionKey(transactionID, subscriberURL)
	bytes, err := json.Marshal(txn)
	if err != nil {
		return err
	}
	return s.Cache.Set(ctx, key, string(bytes), cacheTTL)
}
