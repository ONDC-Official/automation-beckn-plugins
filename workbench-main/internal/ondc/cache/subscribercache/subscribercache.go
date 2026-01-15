package subscribercache

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/cache"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

type Service struct {
	Cache definition.Cache
}

func (s *Service) CheckIfSubscriberExists(ctx context.Context, subscriberURL string) (bool, error) {
	if strings.TrimSpace(subscriberURL) == "" {
		return false, nil
	}
	if s.Cache == nil {
		return false, errors.New("cache is nil")
	}
	raw, err := s.Cache.Get(ctx, subscriberURL)
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(raw) != "", nil
}

func (s *Service) LoadSubscriberThatExists(ctx context.Context, subscriberURL string) (*cache.SubscriberCache, error) {
	if s.Cache == nil {
		return nil, errors.New("cache is nil")
	}
	raw, err := s.Cache.Get(ctx, subscriberURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("subscriber not found")
	}

	var sub cache.SubscriberCache
	if err := json.Unmarshal([]byte(raw), &sub); err != nil {
		return nil, err
	}
	if sub.ActiveSessions == nil {
		sub.ActiveSessions = []cache.Expectation{}
	}
	return &sub, nil
}

func (s *Service) UpdateSubscriber(ctx context.Context, subscriberURL string, subscriber *cache.SubscriberCache, cacheTTL time.Duration) error {
	if s.Cache == nil {
		return errors.New("cache is nil")
	}
	bytes, err := json.Marshal(subscriber)
	if err != nil {
		return err
	}
	return s.Cache.Set(ctx, subscriberURL, string(bytes), cacheTTL)
}
