package ondcworkbench

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/apiservice"
	contextvalidatotors "github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/contextValidatotors"
	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/ondc/payloadutils"
	"github.com/beckn-one/beckn-onix/pkg/log"
)

// FD (ONDC:FIS14:FD) branches of the workbench plugin. isFD is the one switch: every FD
// behaviour hangs off it, and with any other protocolDomain none of this code runs.

const fdProtocolDomain = "ONDC:FIS14:FD"

func isFD(config *Config) bool {
	return config != nil && config.ProtocolDomain == fdProtocolDomain
}

// fdForcedOnCookies are the validation switches an FD session can never turn off.
var fdForcedOnCookies = []string{"header_validation", "protocol_validation"}

// receiveFD is receive, then forces protocol and header validation on: an FD session never
// skips them, whatever its difficulty says.
func (w *ondcWorkbench) receiveFD(ctx context.Context, request *http.Request, body []byte) error {
	if err := w.receive(ctx, request, body); err != nil {
		return err
	}
	forceFDValidationCookies(request)
	return nil
}

// forceFDValidationCookies sets header_validation and protocol_validation to "true". It drops
// every earlier cookie of those names first, the difficulty's and any the caller sent, because
// http.Request.Cookie returns the first cookie of a name.
func forceFDValidationCookies(req *http.Request) {
	kept := make([]*http.Cookie, 0, len(req.Cookies()))
	for _, c := range req.Cookies() {
		if !slices.Contains(fdForcedOnCookies, c.Name) {
			kept = append(kept, c)
		}
	}
	req.Header.Del("Cookie")
	for _, c := range kept {
		req.AddCookie(c)
	}
	for _, name := range fdForcedOnCookies {
		req.AddCookie(&http.Cookie{Name: name, Value: "true"})
	}
}

// validateFDContext is validateContext for FD: the FD context checks (inReplyTo pairing,
// single-use message_id, replays), then an ACK that always echoes the request's context,
// whatever the version. fd-gate turns it into a 202 for an accepted async request.
func (w *ondcWorkbench) validateFDContext(ctx context.Context, request *http.Request, body []byte) error {
	payloadEnv, raw, err := payloadutils.ValidateAndExtractPayload(ctx, body, w.Config.TransactionProperties)
	if err != nil {
		log.Errorf(ctx, err, "context validation: failed to parse payload")
		return err
	}
	err = w.ContextValidator.ValidateFDContext(ctx, request, payloadEnv, raw, contextvalidatotors.FDMessage{
		Body:   body,
		FromNP: w.Config.ModuleType == string(apiservice.Receiver),
	})
	if err != nil {
		log.Errorf(ctx, err, "context validation failed")
		return err
	}
	ack, _ := json.Marshal(payloadutils.NewAckObject(raw["context"]))
	request.AddCookie(&http.Cookie{
		Name:  "custom-response-body",
		Value: base64.StdEncoding.EncodeToString(ack),
	})
	return nil
}
