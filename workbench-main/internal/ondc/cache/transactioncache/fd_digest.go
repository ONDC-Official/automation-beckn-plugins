package transactioncache

import (
	"context"
	"errors"
	"strings"
	"time"
)

// FD (ONDC:FIS14:FD) only. The SHA-256 of each message body the workbench let through, by
// transaction and message_id, so that a repeated message_id can be told apart: a byte-identical
// body is a replay, any other body a reuse. It is a key of its own rather than a field of the
// transaction because the recorder rewrites the transaction concurrently, and a plain Get/Set
// here could overwrite the entry it has just appended.

const fdBodyDigestKeyPrefix = "FD_BODY_SHA256::"

// fdBodyDigestTTL keeps a digest as long as the transaction it belongs to, which neither the
// workbench nor (by default) the recorder expires. A digest that expired early would turn a
// genuine replay into a MESSAGE_ID_REUSED refusal.
const fdBodyDigestTTL time.Duration = 0

// FDBodyDigestKey is the cache key of a message's body digest within a transaction.
func (s *Service) FDBodyDigestKey(transSubKey, messageID string) string {
	return fdBodyDigestKeyPrefix + transSubKey + "::" + strings.TrimSpace(messageID)
}

// SaveFDBodyDigest records the body digest of a message the workbench let through.
func (s *Service) SaveFDBodyDigest(ctx context.Context, transSubKey, messageID, digest string) error {
	if s.Cache == nil {
		return errors.New("cache is nil")
	}
	return s.Cache.Set(ctx, s.FDBodyDigestKey(transSubKey, messageID), digest, fdBodyDigestTTL)
}

// LoadFDBodyDigest returns the recorded body digest of a message, or "" when there is none.
// Like CheckIfTransactionExists, a failed read counts as absent.
func (s *Service) LoadFDBodyDigest(ctx context.Context, transSubKey, messageID string) string {
	if s.Cache == nil {
		return ""
	}
	digest, err := s.Cache.Get(ctx, s.FDBodyDigestKey(transSubKey, messageID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(digest)
}
