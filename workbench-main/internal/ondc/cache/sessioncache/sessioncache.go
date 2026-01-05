package sessioncache

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	cache "ondcworkbench/internal/ondc/cache"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

type Service struct {
	Cache definition.Cache
}

func (s *Service) CheckIfSessionExists(ctx context.Context, sessionID string) (bool, error) {
	if strings.TrimSpace(sessionID) == "" {
		return false, nil
	}
	if s.Cache == nil {
		return false, errors.New("cache is nil")
	}
	raw, err := s.Cache.Get(ctx, sessionID)
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(raw) != "", nil
}

func (s *Service) LoadSessionThatExists(ctx context.Context, sessionID string) (*cache.SessionCache, error) {
	if s.Cache == nil {
		return nil, errors.New("cache is nil")
	}
	raw, err := s.Cache.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("session not found")
	}

	var sess cache.SessionCache
	if err := json.Unmarshal([]byte(raw), &sess); err != nil {
		return nil, err
	}
	if sess.TransactionIds == nil {
		sess.TransactionIds = []string{}
	}
	if sess.FlowMap == nil {
		sess.FlowMap = map[string]string{}
	}
	return &sess, nil
}

func (s *Service) UpdateSessionCache(ctx context.Context, sessionID, flowID, transactionID string, cacheTTL time.Duration) error {
	exists, err := s.CheckIfSessionExists(ctx, sessionID)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	sess, err := s.LoadSessionThatExists(ctx, sessionID)
	if err != nil {
		return err
	}

	sess.TransactionIds = append(sess.TransactionIds, transactionID)
	if sess.FlowMap == nil {
		sess.FlowMap = map[string]string{}
	}
	sess.FlowMap[flowID] = transactionID

	bytes, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	return s.Cache.Set(ctx, sessionID, string(bytes), cacheTTL)
}
