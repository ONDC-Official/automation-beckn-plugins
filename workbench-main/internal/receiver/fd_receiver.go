package receiver

import (
	"context"
	"fmt"
	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/apiservice"
	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/cache"
	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/payloadutils"
	"github.com/beckn-one/beckn-onix/pkg/log"
)

// FD (ONDC:FIS14:FD) branches of the receiver. They run only after EnableFD, which the
// workbench calls when its protocol domain is FD.

// fdNow is the clock of the FD checks; tests replace it.
var fdNow = time.Now

// EnableFD switches on the FD branches of the receiver.
func (r *WorkbenchRequestReceiver) EnableFD() {
	r.fd = true
}

// fdExpectationExpired names the case Beckn leaves silent: an NP request that opens a
// transaction matches no live expectation, only an expired one for its action. Beckn drops the
// expired expectation and answers "No active expectation found"; FD answers the named NACK
// EXPECTATION_EXPIRED. expectations is the subscriber's list as it was before
// tryFulfillExpectation dropped the expired ones; when the request was matched (its session is
// set) there is nothing to report.
func (r *WorkbenchRequestReceiver) fdExpectationExpired(ctx context.Context, expectations []cache.Expectation, requestData *apiservice.WorkbenchRequestData) error {
	if !r.fd || requestData.SessionID != "" {
		return nil
	}
	action := requestData.BodyEnvelope.Context.Action
	now := fdNow()
	var expired *cache.Expectation
	var expiredAt time.Time
	for i := range expectations {
		e := &expectations[i]
		if e.ExpectedAction != action {
			continue
		}
		at, err := time.Parse(time.RFC3339, e.ExpireAt)
		if err != nil || !now.After(at) {
			continue
		}
		if expired == nil || at.After(expiredAt) {
			expired, expiredAt = e, at
		}
	}
	if expired == nil {
		return nil
	}
	msg := fmt.Sprintf(
		"the workbench expected <%s> from %s for flow %s of session %s until %s; it arrived at %s, after the expectation expired. Start the step again",
		action,
		requestData.SubscriberURL,
		expired.FlowId,
		expired.SessionId,
		expired.ExpireAt,
		now.UTC().Format(time.RFC3339),
	)
	log.Warnf(ctx, "%s", msg)
	return payloadutils.NewNamedNackError(payloadutils.CodeExpectationExpired, msg, requestData.BodyRaw["context"])
}
