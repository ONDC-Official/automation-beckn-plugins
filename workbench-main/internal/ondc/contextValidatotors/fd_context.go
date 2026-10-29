package contextvalidatotors

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/apiservice"
	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/cache"
	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/payloadutils"
	"github.com/beckn-one/beckn-onix/pkg/log"
)

// FD (ONDC:FIS14:FD) branch of the context validator. The workbench calls ValidateFDContext
// instead of ValidateContext when its protocol domain is FD, so nothing here runs for Beckn.
//
// Where FD differs from Beckn:
//   - A callback (an action with an async_predecessor) carries a fresh context.message_id and
//     names the request it answers in fdEnvelope.inReplyTo. A Beckn callback reuses the
//     request's message_id.
//   - A webhook carries no inReplyTo. Like a request, it has no async_predecessor.
//   - A message_id is single-use within a transaction. It may repeat only with a byte-identical
//     body: a replay, which the mock re-sends on purpose when an NP retries a request. Any other
//     repeat is refused with MESSAGE_ID_REUSED.

const (
	// cookieFDReplay marks a replay for the later steps and the recorder
	// (network-observability sees it as $.ctx.cookies.fd_replay).
	cookieFDReplay = "fd_replay"
	// fdClockSkew is the clock difference tolerated between the NP and the workbench; the same
	// as fd-gate's default clockSkew.
	fdClockSkew = 5 * time.Second
)

// fdNow is the clock of the FD checks; tests replace it.
var fdNow = time.Now

// FDMessage is what the FD branch needs beyond the parsed payload.
type FDMessage struct {
	// Body is the request body exactly as received. A replay must match it byte for byte.
	Body []byte
	// FromNP is true when the NP under test sent the message (the receiver module), false when
	// the workbench's mock did (the caller module).
	FromNP bool
}

type fdMessageIDVerdict int

const (
	fdNewMessageID fdMessageIDVerdict = iota
	fdReplay
	fdMessageIDReused
)

// ValidateFDContext is ValidateContext for FD sessions:
//  1. a repeated message_id is a replay (identical body: let through, flagged) or refused;
//  2. a message from the NP must not be older than the transaction's latest message;
//  3. a callback pairs with its request through fdEnvelope.inReplyTo, and its TTL is that
//     request's TTL; a request or webhook follows the action order, as in Beckn;
//  4. once let through, the body's digest is kept, so an identical re-send is a replay.
func (cv *Validator) ValidateFDContext(ctx context.Context, httpRequest *http.Request, payload apiservice.PayloadEnvelope, payloadRaw apiservice.PayloadRaw, msg FDMessage) error {
	if httpRequest == nil {
		return fmt.Errorf("httpRequest cannot be nil")
	}
	subscriberURL, err := mustGetCookie(httpRequest, cookieSubscriberURL)
	if err != nil {
		log.Errorf(ctx, err, "Error while fetching subscriber URL from cookies")
		return payloadutils.NewInternalServerNackError("Error while fetching subscriber URL from cookies", payload.Context)
	}
	requestOwner, err := mustGetCookie(httpRequest, cookieRequestOwner)
	if err != nil {
		log.Errorf(ctx, err, "Error while fetching request owner from cookies")
		return payloadutils.NewInternalServerNackError("Error while fetching request owner from cookies", payload.Context)
	}
	messageID := strings.TrimSpace(payload.Context.MessageID)
	if messageID == "" {
		return payloadutils.NewBadRequestNackError("context.message_id is missing", payloadRaw["context"])
	}

	txnKey := cv.transactioncache.CreateTransactionKey(payload.Context.TransactionID, subscriberURL.Value)
	transactionData, err := cv.transactioncache.LoadTransactionThatExists(ctx, txnKey)
	if err != nil {
		log.Errorf(ctx, err, "failed to load transaction data for transaction id: %s and subscriber url: %s", payload.Context.TransactionID, subscriberURL.Value)
		return payloadutils.NewInternalServerNackError("failed to load transaction data", payload.Context)
	}
	history := sortApiDataByTimestampDesc(apiDataFromTransaction(transactionData))
	action := payload.Context.Action
	predecessorName := cv.getAsyncPredecessor(action)

	digest := fdBodyDigest(msg.Body)
	storedDigest := cv.transactioncache.LoadFDBodyDigest(ctx, txnKey, messageID)
	switch verdict, earlier := classifyFDMessageID(messageID, digest, storedDigest, history); verdict {
	case fdReplay:
		log.Infof(ctx, "FD replay: %s %s repeats an earlier message byte for byte; letting it through, flagged", action, messageID)
		setForwardRequestCookie(httpRequest, true, "")
		httpRequest.AddCookie(&http.Cookie{Name: cookieFDReplay, Value: "true"})
		return nil
	case fdMessageIDReused:
		reason := fdMessageIDReusedReason(action, messageID, predecessorName, earlier)
		log.Warnf(ctx, "%s", reason)
		return payloadutils.NewNamedNackError(payloadutils.CodeMessageIDReused, reason, payloadRaw["context"])
	}

	if msg.FromNP {
		if result := validateFDNotStale(payload, transactionData); !result.Valid {
			log.Warnf(ctx, "%s", result.Error)
			return payloadutils.NewBadRequestNackError(result.Error, payloadRaw["context"])
		}
	}

	result := cv.validateFDAsyncContext(ctx, payload, fdInReplyTo(payloadRaw), predecessorName, history, transactionData, requestOwner.Value)
	setForwardRequestCookie(httpRequest, result.ForwardRequest, result.Error)
	if !result.Valid && !result.ForwardRequest {
		return payloadutils.NewBadRequestNackError(result.Error, payloadRaw["context"])
	}

	// The message goes on: keep its digest, so that an identical re-send is a replay.
	if err := cv.transactioncache.SaveFDBodyDigest(ctx, txnKey, messageID, digest); err != nil {
		log.Errorf(ctx, err, "failed to save the body digest of %s %s", action, messageID)
		return payloadutils.NewInternalServerNackError("failed to record the message for replay detection", payloadRaw["context"])
	}
	if !result.Valid {
		// Invalid but forwarded (a callback after its request's TTL), as in Beckn; the NACK
		// echoes the context, as every FD answer does.
		log.Warnf(ctx, "Context validation warning (forward_request=true): %s", result.Error)
		nack := getNackErrorPayload(result.Error)
		if ondcContext, ok := payloadRaw["context"]; ok && ondcContext != nil {
			nack["context"] = ondcContext
		}
		nackBytes, _ := json.Marshal(nack)
		httpRequest.AddCookie(&http.Cookie{Name: "custom-response-body", Value: base64.StdEncoding.EncodeToString(nackBytes)})
	}
	return nil
}

func (cv *Validator) validateFDAsyncContext(ctx context.Context, payload apiservice.PayloadEnvelope, inReplyTo, predecessorName string, history []cache.ApiData, transactionData *cache.TransactionCache, requestOwner string) contextValidationResult {
	action := payload.Context.Action
	if predecessorName == "" {
		// A request or a webhook: nothing to pair with. The action order is Beckn's sync path.
		if result := cv.validateSyncPath(ctx, payload, transactionData); !result.Valid {
			return result
		}
		return cv.validateTransactionId(ctx, action, history)
	}
	request, result := pairFDCallback(action, predecessorName, inReplyTo, history)
	if !result.Valid {
		log.Warnf(ctx, "%s", result.Error)
		return result
	}
	result = validateFDCallbackTtl(action, request, requestOwner, fdNow())
	if !result.Valid {
		log.Warnf(ctx, "%s", result.Error)
	}
	return result
}

// classifyFDMessageID says whether messageID is new in the transaction, a replay (the
// workbench let a message with this id and this exact body through before) or a reuse. It also
// returns the latest history entry with that message_id, if any.
func classifyFDMessageID(messageID, digest, storedDigest string, history []cache.ApiData) (fdMessageIDVerdict, *cache.ApiData) {
	earlier := findFirstMessageID(history, messageID)
	if storedDigest != "" {
		if storedDigest == digest {
			return fdReplay, earlier
		}
		return fdMessageIDReused, earlier
	}
	if earlier != nil {
		// In the history but never let through (no digest): it was refused, so a repeat of it
		// is not a replay.
		return fdMessageIDReused, earlier
	}
	return fdNewMessageID, nil
}

func fdMessageIDReusedReason(action, messageID, predecessorName string, earlier *cache.ApiData) string {
	reason := fmt.Sprintf("message_id %s of <%s> is already used in this transaction", messageID, action)
	if earlier != nil {
		reason = fmt.Sprintf("message_id %s of <%s> is already used in this transaction by <%s>", messageID, action, earlier.Action)
	}
	reason += "; an FD message_id is single-use, and only a byte-identical re-send (a replay) may repeat it"
	if earlier != nil && predecessorName != "" && earlier.Action == predecessorName {
		reason += fmt.Sprintf(". <%s> answers <%s> with a fresh message_id and names the request in fdEnvelope.inReplyTo", action, predecessorName)
	}
	return reason
}

// pairFDCallback finds the request a callback answers: the <predecessorName> entry whose
// message_id is the callback's fdEnvelope.inReplyTo. An FD callback names its request, so any
// such request in the history pairs, not only the latest; of several (a refused repeat is
// recorded too), the first one sent is the request.
func pairFDCallback(action, predecessorName, inReplyTo string, history []cache.ApiData) (*cache.ApiData, contextValidationResult) {
	if inReplyTo == "" {
		msg := fmt.Sprintf("fdEnvelope.inReplyTo is missing: <%s> answers a <%s> and must carry its message_id in fdEnvelope.inReplyTo", action, predecessorName)
		return nil, contextValidationResult{Valid: false, Error: msg, ForwardRequest: false}
	}
	var request *cache.ApiData
	for i := range history { // newest first, so the last match is the first sent
		if history[i].Action == predecessorName && history[i].MessageId == inReplyTo && !history[i].FdReplay {
			request = &history[i]
		}
	}
	if request != nil {
		return request, contextValidationResult{Valid: true, ForwardRequest: true}
	}

	var msg string
	if other := findFirstMessageID(history, inReplyTo); other != nil {
		msg = fmt.Sprintf("inReplyTo mismatch: fdEnvelope.inReplyTo %s of <%s> names a <%s>, but <%s> answers a <%s>", inReplyTo, action, other.Action, action, predecessorName)
	} else if latest := findFirstAction(history, predecessorName); latest != nil {
		msg = fmt.Sprintf("inReplyTo mismatch: no <%s> with message_id %s (fdEnvelope.inReplyTo of <%s>) is in the flow history; the latest <%s> is %s", predecessorName, inReplyTo, action, predecessorName, latest.MessageId)
	} else {
		msg = fmt.Sprintf("%s for %s not found in the flow history", predecessorName, action)
	}
	return nil, contextValidationResult{Valid: false, Error: msg, ForwardRequest: false}
}

// validateFDCallbackTtl is validateTtl paired through inReplyTo: a callback must arrive within
// the TTL of the request it answers. Like Beckn's, a late callback is still forwarded, with a
// NACK.
func validateFDCallbackTtl(action string, request *cache.ApiData, requestOwner string, now time.Time) contextValidationResult {
	if !isNPRequestOwner(requestOwner) || request == nil || request.TTL == nil {
		return contextValidationResult{Valid: true, ForwardRequest: true}
	}
	sentMillis := parseTimeMillis(request.RealTimestamp)
	if sentMillis == 0 {
		sentMillis = parseTimeMillis(request.Timestamp)
	}
	if sentMillis == 0 {
		return contextValidationResult{Valid: true, ForwardRequest: true}
	}
	expiryMillis := sentMillis + (*request.TTL * 1000)
	if now.UnixMilli() > expiryMillis {
		msg := fmt.Sprintf(
			"TTL expired for %s: it answers <%s> %s, whose TTL ran out at %s; received at %s",
			action,
			request.Action,
			request.MessageId,
			time.UnixMilli(expiryMillis).UTC().Format(time.RFC3339Nano),
			now.UTC().Format(time.RFC3339Nano),
		)
		return contextValidationResult{Valid: false, Error: msg, ForwardRequest: true}
	}
	return contextValidationResult{Valid: true, ForwardRequest: true}
}

// validateFDNotStale is the stale-timestamp check Beckn means to run (validateNotStaleTimestamp
// tests latestTimestamp == "", so it never does): a message must not be older than the latest
// message already in the transaction, give or take fdClockSkew, since the two come from
// different clocks. It applies to the NP's messages only.
func validateFDNotStale(payload apiservice.PayloadEnvelope, transactionData *cache.TransactionCache) contextValidationResult {
	latest, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(transactionData.LatestTimestamp))
	if err != nil || latest.Unix() <= 0 {
		// No message yet: a new transaction's latestTimestamp is the epoch.
		return contextValidationResult{Valid: true, ForwardRequest: true}
	}
	timestamp, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(payload.Context.Timestamp))
	if err != nil {
		msg := fmt.Sprintf("context.timestamp %q is not an RFC 3339 date-time", payload.Context.Timestamp)
		return contextValidationResult{Valid: false, Error: msg, ForwardRequest: false}
	}
	if timestamp.Before(latest.Add(-fdClockSkew)) {
		msg := fmt.Sprintf(
			"stale context timestamp: %s is before the latest timestamp in the transaction history, %s, by more than the %s allowed for clock skew",
			payload.Context.Timestamp,
			transactionData.LatestTimestamp,
			fdClockSkew,
		)
		return contextValidationResult{Valid: false, Error: msg, ForwardRequest: false}
	}
	return contextValidationResult{Valid: true, ForwardRequest: true}
}

func fdInReplyTo(payloadRaw apiservice.PayloadRaw) string {
	envelope, _ := payloadRaw["fdEnvelope"].(map[string]any)
	inReplyTo, _ := envelope["inReplyTo"].(string)
	return strings.TrimSpace(inReplyTo)
}

func fdBodyDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func findFirstMessageID(items []cache.ApiData, messageID string) *cache.ApiData {
	for i := range items {
		if items[i].MessageId == messageID {
			return &items[i]
		}
	}
	return nil
}
