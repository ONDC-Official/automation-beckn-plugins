package fdgate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdroute"
	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdwire"
)

// correlationKeys are the identifiers of the spec's Correlation block. A path parameter
// with one of these names must be repeated in fdEnvelope.correlation.
var correlationKeys = map[string]bool{
	"applicationId": true, "quoteId": true, "depositId": true, "servicingRequestId": true,
	"grievanceId": true, "refundId": true, "eventId": true,
}

// broadcastAction is the one action whose context carries no bpp_id/bpp_uri.
const broadcastAction = "discovery_schemes_query"

// fdBody is a decoded FD message body ({context, fdEnvelope, message|error}).
type fdBody struct {
	contextRaw json.RawMessage
	context    map[string]any
	envelope   map[string]any
	corr       map[string]any
}

// str returns context[key] when it is a string.
func (b *fdBody) str(key string) string {
	s, _ := b.context[key].(string)
	return s
}

// parseFDBody decodes a body into its FD parts. It fails with BODY_MALFORMED unless the
// body is a JSON object with object-valued context and fdEnvelope and a message (or, on a
// refusal, an error).
func parseFDBody(data []byte) (*fdBody, *gateError) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil || top == nil {
		return nil, reject(CodeBodyMalformed, "", "the body is not a JSON object")
	}
	b := &fdBody{contextRaw: top["context"]}
	if err := decodeObject(top["context"], &b.context); err != nil {
		return nil, reject(CodeBodyMalformed, "/context", "context is missing or not an object")
	}
	if err := decodeObject(top["fdEnvelope"], &b.envelope); err != nil {
		return nil, reject(CodeBodyMalformed, "/fdEnvelope", "fdEnvelope is missing or not an object")
	}
	if !isObject(top["message"]) && !isObject(top["error"]) {
		return nil, reject(CodeBodyMalformed, "/message", "message (or, on a refusal, error) is missing or not an object")
	}
	if raw, ok := b.envelope["correlation"]; ok && raw != nil {
		corr, ok := raw.(map[string]any)
		if !ok {
			return nil, reject(CodeBodyMalformed, "/fdEnvelope/correlation", "fdEnvelope.correlation is not an object")
		}
		b.corr = corr
	}
	return b, nil
}

// peekContext returns a body's context, raw, when the body is a JSON object with an object
// context; nil otherwise. It is used to echo the context on a NACK.
func peekContext(data []byte) json.RawMessage {
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil {
		return nil
	}
	if isObject(top["context"]) {
		return top["context"]
	}
	return nil
}

func decodeObject(raw json.RawMessage, dst *map[string]any) error {
	if !isObject(raw) {
		return fmt.Errorf("not an object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(dst)
}

func isObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{'
}

// senderID is the context id of the route's caller: bap_id for the investor app, bpp_id for
// the issuer.
func senderID(route *fdroute.Route, b *fdBody) (string, string) {
	if route.Caller == fdroute.RoleDepositIssuer {
		return b.str("bpp_id"), "/context/bpp_id"
	}
	return b.str("bap_id"), "/context/bap_id"
}

// senderURI is the caller's subscriber URL from the context.
func senderURI(route *fdroute.Route, b *fdBody) string {
	if route.Caller == fdroute.RoleDepositIssuer {
		return b.str("bpp_uri")
	}
	return b.str("bap_uri")
}

// checkSigner enforces that the signer is the sender the context names (FD-PWB-0016).
func checkSigner(route *fdroute.Route, b *fdBody, signer string) *gateError {
	id, path := senderID(route, b)
	if id != signer {
		return reject(CodeSignerNotSender, path, "the request is signed by %q but %s is %q", signer, strings.TrimPrefix(path, "/"), id)
	}
	return nil
}

// checkTransportRules enforces §3.5 on a POST body.
func (g *Gate) checkTransportRules(route *fdroute.Route, params map[string]string, b *fdBody) *gateError {
	if a := b.str("action"); a != route.Action {
		return reject(CodeActionPathMismatch, "/context/action", "context.action is %q but the path is the %s operation", a, route.Action)
	}
	if d := b.str("domain"); d != g.table.Domain {
		return reject(CodeVersionMismatch, "/context/domain", "context.domain is %q; this workbench serves %s", d, g.table.Domain)
	}
	if v := b.str("version"); v != g.table.Version {
		return reject(CodeVersionMismatch, "/context/version", "context.version is %q; this workbench serves %s %s", v, g.table.Domain, g.table.Version)
	}
	if role, _ := b.envelope["originatorRole"].(string); role != route.Caller {
		return reject(CodeWrongDirection, "/fdEnvelope/originatorRole", "fdEnvelope.originatorRole is %q but %s is sent by %s", role, route.Action, route.Caller)
	}
	for _, p := range route.PathParams {
		v, present := b.corr[p]
		if !present && !correlationKeys[p] {
			continue // not a correlation identifier (e.g. consentId), and not repeated
		}
		if s, _ := v.(string); s != params[p] {
			return reject(CodePathCorrelation, "/fdEnvelope/correlation/"+p,
				"the path names %s %q but fdEnvelope.correlation.%s is %s", p, params[p], p, describe(v, present))
		}
	}

	for _, k := range []string{"bap_id", "bap_uri", "transaction_id", "message_id", "timestamp", "ttl"} {
		if b.str(k) == "" {
			return reject(CodeContextInvalid, "/context/"+k, "context.%s is missing or not a string", k)
		}
	}
	ts, err := time.Parse(time.RFC3339Nano, b.str("timestamp"))
	if err != nil {
		return reject(CodeContextInvalid, "/context/timestamp", "context.timestamp %q is not an RFC 3339 date-time", b.str("timestamp"))
	}
	ttl, err := fdwire.ParseISODuration(b.str("ttl"))
	if err != nil {
		return reject(CodeContextInvalid, "/context/ttl", "context.ttl %q is not an ISO 8601 duration", b.str("ttl"))
	}
	now, skew := g.now(), g.cfg.ClockSkew
	if ts.After(now.Add(skew)) {
		return reject(CodeTimestampInFuture, "/context/timestamp", "context.timestamp %s is ahead of the workbench clock (%s) by more than %s",
			b.str("timestamp"), now.UTC().Format(time.RFC3339), skew)
	}
	if now.After(ts.Add(ttl).Add(skew)) {
		return reject(CodeTTLExpired, "/context/ttl", "the message expired at %s (timestamp %s + ttl %s); the workbench clock is %s",
			ts.Add(ttl).UTC().Format(time.RFC3339), b.str("timestamp"), b.str("ttl"), now.UTC().Format(time.RFC3339))
	}

	bppID, bppURI := b.str("bpp_id"), b.str("bpp_uri")
	if route.Action == broadcastAction {
		if (bppID == "") != (bppURI == "") {
			return reject(CodeContextInvalid, "/context/bpp_uri", "context.bpp_id and context.bpp_uri must appear together")
		}
	} else if bppID == "" || bppURI == "" {
		missing := "bpp_id"
		if bppID != "" {
			missing = "bpp_uri"
		}
		return reject(CodeContextInvalid, "/context/"+missing, "context.%s is required on %s", missing, route.Action)
	}
	for _, k := range []string{"bap_uri", "bpp_uri"} {
		v := b.str(k)
		if v == "" {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return reject(CodeContextInvalid, "/context/"+k, "context.%s %q is not an absolute URL", k, v)
		}
		if g.cfg.RequireHTTPS && u.Scheme != "https" {
			return reject(CodeContextInvalid, "/context/"+k, "context.%s %q must use https", k, v)
		}
	}
	return nil
}

func describe(v any, present bool) string {
	if !present {
		return "absent"
	}
	data, _ := json.Marshal(v)
	return string(data)
}
