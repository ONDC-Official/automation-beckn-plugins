package ondcvalidator

import (
	"context"
	"fmt"
	"time"
	"validationpkg/validationutils"
)

// FD (ONDC:FIS14:FD) only: the lifetime of the data saved for stateful validations.
//
// cacheWrapper.SaveKey passes 60*5 to definition.Cache.Set, whose ttl is a time.Duration, so
// the data is kept for 300 ns; go-redis rounds that up to PX 1, and the key is gone 1 ms later.
// Cross-message rules (_SESSION_DATA_) therefore never see what an earlier message saved.
// Fixing that for every domain would switch those rules on for Beckn sessions, which run with
// stateFullValidations: true, and change their verdicts; so Beckn keeps the old value and only
// FD payloads get a real duration.

// fdProtocolDomain is the context.domain of every FD message.
const fdProtocolDomain = "ONDC:FIS14:FD"

// fdStatefulTTL is how long an FD transaction's saved validation data lives. It is deliberately
// longer than the 5 minutes 60*5 was meant to be: an FD run waits on people (KYC, payment) and
// on webhooks, and data that expires mid-flow makes cross-message rules pass silently. 24 hours
// matches fd-gate's default replay window.
const fdStatefulTTL = 24 * time.Hour

// isFDPayload is the FD switch of this plugin, which has no domain in its config: it reads the
// payload's context.domain. In an FD service fd-gate has already checked that domain against the
// route table before the validator runs.
func isFDPayload(env payloadEnvelope) bool {
	return env.Context.Domain == fdProtocolDomain
}

// storageFor returns the store for a payload: FD payloads save with fdStatefulTTL, every other
// payload uses v.storage exactly as before.
func (v *ondcValidator) storageFor(env payloadEnvelope) validationutils.StorageInterface {
	if isFDPayload(env) {
		return &fdCacheWrapper{cacheWrapper{cache: v.cache}}
	}
	return v.storage
}

// fdCacheWrapper is cacheWrapper with a real TTL on SaveKey; reads and deletes are cacheWrapper's.
type fdCacheWrapper struct {
	cacheWrapper
}

func (c *fdCacheWrapper) SaveKey(uniquePrefix string, key string, value string) error {
	fullKey := fmt.Sprintf("%s:%s", uniquePrefix, key)
	return c.cache.Set(context.Background(), fullKey, value, fdStatefulTTL)
}
