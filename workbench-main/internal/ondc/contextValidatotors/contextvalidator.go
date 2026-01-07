package contextvalidatotors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"ondcworkbench/internal/apiservice"
	"ondcworkbench/internal/ondc/cache"
	"ondcworkbench/internal/ondc/cache/transactioncache"
	"ondcworkbench/internal/ondc/payloadutils"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
)

const (
	cookieSubscriberURL   = "subscriber_url"
	cookieRequestOwner    = "request_owner"
	cookieForwardRequest  = "forward_request"
	cookieWorkbenchErrMsg = "workbench_error_message"
)

type Validator struct {
	transactioncache *transactioncache.Service
	properties       apiservice.TransactionProperties
}

func NewContextValidator(txnCache *transactioncache.Service, props apiservice.TransactionProperties) *Validator {
	return &Validator{
		transactioncache: txnCache,
		properties:       props,
	}
}

func (cv *Validator) ValidateContext(ctx context.Context, httpRequest *http.Request, payload apiservice.PayloadEnvelope, payloadRaw apiservice.PayloadRaw) error {
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

	transactionData, err := cv.transactioncache.LoadTransactionThatExists(ctx,
		cv.transactioncache.CreateTransactionKey(payload.Context.TransactionID, subscriberURL.Value),
	)
	if err != nil {
		log.Errorf(ctx, err, "failed to load transaction data for transaction id: %s and subscriber url: %s", payload.Context.TransactionID, subscriberURL.Value)
		return payloadutils.NewInternalServerNackError("failed to load transaction data", payload.Context)
	}
	if err := validateNotStaleTimestamp(ctx, payload, payloadRaw, transactionData); err != nil {
		return err
	}

	result := cv.validateAsyncContext(ctx, payload, transactionData, requestOwner.Value)
	setForwardRequestCookie(httpRequest, result.ForwardRequest, result.Error)

	if result.Valid {
		return nil
	}
	if result.ForwardRequest {
		// Mirror TS behavior: invalid but allowed to proceed (forwardRequest=true).
		log.Warnf(ctx, "Context validation warning (forward_request=true): %s", result.Error)
		return nil
	}
	return payloadutils.NewBadRequestNackError(result.Error, payloadRaw["context"])
}

type contextValidationResult struct {
	Valid          bool
	Error          string
	ForwardRequest bool
}

func (cv *Validator) validateAsyncContext(ctx context.Context, payload apiservice.PayloadEnvelope, transactionData *cache.TransactionCache, requestOwner string) contextValidationResult {
	log.Infof(ctx, "Validating Transaction History")

	apiEntries := apiDataFromTransaction(transactionData)
	sortedContexts := sortApiDataByTimestampDesc(apiEntries)

	subjectAction := payload.Context.Action
	predecessorName := cv.getAsyncPredecessor(subjectAction)
	if predecessorName != "" {
		return cv.validateAsyncPath(ctx, payload, sortedContexts, predecessorName)
	}
	if result := cv.validateSyncPath(ctx, payload, transactionData); !result.Valid {
		return result
	}

	// TTL validation: only for NP requests.
	ttlResult := validateTtl(ctx, payload, transactionData, requestOwner)
	if !ttlResult.Valid {
		return ttlResult
	}

	return cv.validateTransactionId(ctx, subjectAction, sortedContexts)
}

func (cv *Validator) validateAsyncPath(ctx context.Context, payload apiservice.PayloadEnvelope, sortedContexts []cache.ApiData, predecessorName string) contextValidationResult {
	subjectAction := payload.Context.Action
	predecessor := findFirstAction(sortedContexts, predecessorName)
	if predecessor == nil {
		msg := fmt.Sprintf("%s for %s not found in the flow history", predecessorName, subjectAction)
		log.Warnf(ctx, "%s", msg)
		return contextValidationResult{Valid: false, Error: msg, ForwardRequest: false}
	}
	if predecessor.MessageId != payload.Context.MessageID {
		msg := fmt.Sprintf(
			"message_id mismatch between %s and %s expected %s but found %s",
			predecessorName,
			subjectAction,
			predecessor.MessageId,
			payload.Context.MessageID,
		)
		log.Warnf(ctx, "%s", msg)
		return contextValidationResult{Valid: false, Error: msg, ForwardRequest: false}
	}

	// Duplicate message_id check excluding the predecessor entry.
	for i := range sortedContexts {
		c := sortedContexts[i]
		if c.Action == predecessor.Action && c.MessageId == predecessor.MessageId && c.Timestamp == predecessor.Timestamp {
			continue
		}
		if c.MessageId == payload.Context.MessageID {
			msg := fmt.Sprintf("Duplicate message_id found in the transaction history, %s", payload.Context.MessageID)
			log.Warnf(ctx, "%s", msg)
			return contextValidationResult{Valid: false, Error: msg, ForwardRequest: false}
		}
	}

	return contextValidationResult{Valid: true, ForwardRequest: true}
}

func (cv *Validator) validateSyncPath(ctx context.Context, payload apiservice.PayloadEnvelope, transactionData *cache.TransactionCache) contextValidationResult {
	subjectAction := payload.Context.Action
	if containsString(transactionData.MessageIds, payload.Context.MessageID) {
		msg := "Duplicate message_id found in the flow history"
		log.Warnf(ctx, "%s", msg)
		return contextValidationResult{Valid: false, Error: msg, ForwardRequest: false}
	}

	supported := cv.getSupportedActions(transactionData.LatestAction)
	if !containsString(supported, subjectAction) {
		msg := fmt.Sprintf("%s not supported after %s", subjectAction, transactionData.LatestAction)
		log.Warnf(ctx, "%s", msg)
		return contextValidationResult{Valid: false, Error: msg, ForwardRequest: false}
	}

	return contextValidationResult{Valid: true, ForwardRequest: true}
}

func (cv *Validator) validateTransactionId(ctx context.Context, action string, sortedContexts []cache.ApiData) contextValidationResult {
	log.Infof(ctx, "Running Transaction Id Checks")
	partners := cv.getTransactionPartners(action)
	if len(partners) == 0 {
		return contextValidationResult{Valid: true, ForwardRequest: true}
	}
	matched := findFirstMatches(sortedContexts, partners)
	notFound := make([]string, 0)
	for _, partner := range partners {
		found := false
		for _, ctxItem := range matched {
			if ctxItem.Action == partner {
				found = true
				break
			}
		}
		if !found {
			notFound = append(notFound, partner)
		}
	}
	if len(notFound) > 0 {
		msg := fmt.Sprintf("Transaction partners %s not found in the transaction history to proceed with %s", strings.Join(notFound, ", "), action)
		log.Warnf(ctx, "%s", msg)
		return contextValidationResult{Valid: false, Error: msg, ForwardRequest: false}
	}
	log.Infof(ctx, "Transaction History Checks passed")
	return contextValidationResult{Valid: true, ForwardRequest: true}
}

func (cv *Validator) getAsyncPredecessor(action string) string {
	props, ok := cv.properties.APIProperties[action]
	if !ok || props.AsyncPredecessor == nil {
		return ""
	}
	return strings.TrimSpace(*props.AsyncPredecessor)
}

func (cv *Validator) getSupportedActions(action string) []string {
	if action == "" {
		action = "null"
	}
	if cv.properties.SupportedActions == nil {
		return []string{}
	}
	actions, ok := cv.properties.SupportedActions[action]
	if !ok {
		return []string{}
	}
	return actions
}

func (cv *Validator) getTransactionPartners(action string) []string {
	props, ok := cv.properties.APIProperties[action]
	if !ok {
		return []string{}
	}
	return props.TransactionPartner
}

func validateTtl(ctx context.Context, payload apiservice.PayloadEnvelope, transactionData *cache.TransactionCache, requestOwner string) contextValidationResult {
	if !isNPRequestOwner(requestOwner) {
		log.Infof(ctx, "Skipping TTL validation as request source is not NP")
		return contextValidationResult{Valid: true, ForwardRequest: true}
	}

	action := payload.Context.Action
	log.Infof(ctx, "Running TTL Validations for action: %s", action)
	if !strings.HasPrefix(action, "on_") {
		log.Infof(ctx, "Skipping TTL validation for non-on_ action: %s", action)
		return contextValidationResult{Valid: true, ForwardRequest: true}
	}

	syncAction := strings.TrimPrefix(action, "on_")
	apiEntries := apiDataFromTransaction(transactionData)
	matching := make([]cache.ApiData, 0)
	for _, item := range apiEntries {
		if item.EntryType == "API" && item.Action == syncAction && item.MessageId == payload.Context.MessageID {
			matching = append(matching, item)
		}
	}
	if len(matching) == 0 {
		log.Warnf(ctx, "No matching %s found for %s with message_id: %s , skipping TTL validation", syncAction, action, payload.Context.MessageID)
		return contextValidationResult{Valid: true, ForwardRequest: true}
	}

	latest := matching[0]
	for i := 1; i < len(matching); i++ {
		if parseTimeMillis(matching[i].RealTimestamp) > parseTimeMillis(latest.RealTimestamp) {
			latest = matching[i]
		}
	}

	if latest.TTL == nil {
		log.Warnf(ctx, "No TTL defined for %s, skipping TTL validation for %s", syncAction, action)
		return contextValidationResult{Valid: true, ForwardRequest: true}
	}

	previousMillis := parseTimeMillis(latest.RealTimestamp)
	if previousMillis == 0 {
		previousMillis = parseTimeMillis(latest.Timestamp)
	}
	if previousMillis == 0 {
		log.Warnf(ctx, "Unable to parse timestamps for TTL validation, skipping")
		return contextValidationResult{Valid: true, ForwardRequest: true}
	}

	ttlExpiry := previousMillis + (*latest.TTL * 1000)
	currentMillis := time.Now().UnixMilli()
	if currentMillis > ttlExpiry {
		msg := fmt.Sprintf(
			"TTL expired for %s. $.context.timestamp: %s, TTL expiry: %s",
			action,
			time.UnixMilli(currentMillis).UTC().Format(time.RFC3339Nano),
			time.UnixMilli(ttlExpiry).UTC().Format(time.RFC3339Nano),
		)
		log.Warnf(ctx, "%s", msg)
		return contextValidationResult{Valid: false, Error: msg, ForwardRequest: true}
	}

	log.Infof(ctx, "TTL validation passed for %s", action)
	return contextValidationResult{Valid: true, ForwardRequest: true}
}

func apiDataFromTransaction(txn *cache.TransactionCache) []cache.ApiData {
	if txn == nil {
		return []cache.ApiData{}
	}
	result := make([]cache.ApiData, 0)
	for _, raw := range txn.ApiList {
		b, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var item cache.ApiData
		if err := json.Unmarshal(b, &item); err != nil {
			continue
		}
		if item.EntryType == "API" {
			result = append(result, item)
		}
	}
	return result
}

func sortApiDataByTimestampDesc(items []cache.ApiData) []cache.ApiData {
	if len(items) <= 1 {
		return items
	}
	sorted := make([]cache.ApiData, len(items))
	copy(sorted, items)
	sort.SliceStable(sorted, func(i, j int) bool {
		return parseTimeMillis(sorted[i].Timestamp) > parseTimeMillis(sorted[j].Timestamp)
	})
	return sorted
}

func findFirstAction(items []cache.ApiData, action string) *cache.ApiData {
	for i := range items {
		if items[i].Action == action {
			return &items[i]
		}
	}
	return nil
}

func findFirstMatches(items []cache.ApiData, actions []string) []cache.ApiData {
	result := make([]cache.ApiData, 0)
	found := make(map[string]bool, len(actions))
	for _, item := range items {
		if containsString(actions, item.Action) && !found[item.Action] {
			result = append(result, item)
			found[item.Action] = true
			if len(found) == len(actions) {
				break
			}
		}
	}
	return result
}

func containsString(list []string, v string) bool {
	return slices.Contains(list, v)
}

func parseTimeMillis(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UnixMilli()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli()
	}
	return 0
}

func isNPRequestOwner(owner string) bool {
	owner = strings.TrimSpace(owner)
	return owner == string(apiservice.BuyerNP) || owner == string(apiservice.SellerNP)
}

func setForwardRequestCookie(req *http.Request, forward bool, errorMessage string) {
	if req == nil {
		return
	}
	// Replace any existing forward_request/workbench_error_message cookies on the request.
	existing := req.Cookies()
	kept := make([]*http.Cookie, 0, len(existing)+1)
	for _, c := range existing {
		if c == nil {
			continue
		}
		if c.Name == cookieForwardRequest {
			continue
		}
		if c.Name == cookieWorkbenchErrMsg {
			continue
		}
		kept = append(kept, c)
	}

	req.Header.Del("Cookie")
	for _, c := range kept {
		req.AddCookie(c)
	}
	req.AddCookie(&http.Cookie{Name: cookieForwardRequest, Value: fmt.Sprintf("%t", forward)})
	if errorMessage != "" {
		req.AddCookie(&http.Cookie{Name: cookieWorkbenchErrMsg, Value: errorMessage})
	}
}

func mustGetCookie(req *http.Request, name string) (*http.Cookie, error) {
	if req == nil {
		return nil, fmt.Errorf("request is nil")
	}
	return req.Cookie(name)
}

func validateNotStaleTimestamp(ctx context.Context, payload apiservice.PayloadEnvelope, payloadRaw apiservice.PayloadRaw, transactionData *cache.TransactionCache) error {
	// NOTE: Preserves the current behavior exactly, including the condition.
	latestTimestamp := transactionData.LatestTimestamp
	if latestTimestamp == "" {
		parsedLatestTime, err := time.Parse(time.RFC3339, latestTimestamp)
		if err != nil {
			log.Errorf(ctx, err, "failed to parse context timestamp: %s", payload.Context.Timestamp)
			return payloadutils.NewInternalServerNackError("failed to parse context timestamp", payloadRaw["context"])
		}
		payloadTimestamp, err := time.Parse(time.RFC3339, payload.Context.Timestamp)
		if err != nil {
			log.Errorf(ctx, err, "failed to parse context timestamp: %s", payload.Context.Timestamp)
			return payloadutils.NewBadRequestNackError("failed to parse context timestamp", payloadRaw["context"])
		}
		if payloadTimestamp.Before(parsedLatestTime) {
			msg := fmt.Sprintf(
				"stale context timestamp: %s is before latest timestamp in transaction history: %s",
				payload.Context.Timestamp,
				latestTimestamp,
			)
			log.Warnf(ctx, "%s", msg)
			return payloadutils.NewBadRequestNackError(msg, payloadRaw["context"])
		}
	}
	return nil
}
