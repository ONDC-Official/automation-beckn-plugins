package fdgate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdwire"
)

// Error codes fd-gate answers with. The FD catalogue has none for faults at the workbench's
// own edge, so these follow the spec's code pattern (^FD-[A-Z]+-[0-9]{4}$) under PWB
// (decision D-5). Every one is class PROTOCOL, reason FIELD_CONSTRAINT_VIOLATED.
const (
	CodeRouteNotFound       = "FD-PWB-0001" // ROUTE_NOT_FOUND (HTTP 404)
	CodeInternal            = "FD-PWB-0003" // INTERNAL
	CodeMessageIDReused     = "FD-PWB-0004" // MESSAGE_ID_REUSED
	CodeObjectUnknown       = "FD-PWB-0005" // OBJECT_UNKNOWN
	CodeBodyTooLarge        = "FD-PWB-0006" // BODY_TOO_LARGE (HTTP 413)
	CodeInternalKeyInvalid  = "FD-PWB-0007" // INTERNAL_KEY_INVALID (mock mode, HTTP 403)
	CodePathParamsInvalid   = "FD-PWB-0008" // PATH_PARAMS_INVALID (mock mode, HTTP 400)
	CodeSignatureMissing    = "FD-PWB-0010" // SIGNATURE_MISSING
	CodeRequestTargetNotSig = "FD-PWB-0011" // REQUEST_TARGET_NOT_SIGNED
	CodeSignatureExpired    = "FD-PWB-0012" // SIGNATURE_EXPIRED
	CodeSignerUnknown       = "FD-PWB-0013" // SIGNER_UNKNOWN
	CodeSignatureInvalid    = "FD-PWB-0014" // SIGNATURE_INVALID
	CodeSignatureObjObj     = "FD-PWB-0015" // SIGNATURE_OVER_OBJECT_OBJECT
	CodeSignerNotSender     = "FD-PWB-0016" // SIGNER_NOT_SENDER
	CodeBodyMalformed       = "FD-PWB-0020" // BODY_MALFORMED
	CodeActionPathMismatch  = "FD-PWB-0021" // ACTION_PATH_MISMATCH
	CodeVersionMismatch     = "FD-PWB-0022" // VERSION_MISMATCH
	CodeWrongDirection      = "FD-PWB-0023" // WRONG_DIRECTION
	CodePathCorrelation     = "FD-PWB-0024" // PATH_CORRELATION_MISMATCH
	CodeTTLExpired          = "FD-PWB-0025" // TTL_EXPIRED
	CodeTimestampInFuture   = "FD-PWB-0026" // TIMESTAMP_IN_FUTURE
	CodeContextInvalid      = "FD-PWB-0027" // CONTEXT_INVALID
	CodeWorkbenchRejected   = "FD-PWB-0030" // WORKBENCH_REJECTED
	CodeCounterpartyFailed  = "FD-PWB-0031" // COUNTERPARTY_FAILED
)

// codeNames gives each code its symbolic name, which prefixes the human message.
var codeNames = map[string]string{
	CodeRouteNotFound:       "ROUTE_NOT_FOUND",
	CodeInternal:            "INTERNAL",
	CodeMessageIDReused:     "MESSAGE_ID_REUSED",
	CodeObjectUnknown:       "OBJECT_UNKNOWN",
	CodeBodyTooLarge:        "BODY_TOO_LARGE",
	CodeInternalKeyInvalid:  "INTERNAL_KEY_INVALID",
	CodePathParamsInvalid:   "PATH_PARAMS_INVALID",
	CodeSignatureMissing:    "SIGNATURE_MISSING",
	CodeRequestTargetNotSig: "REQUEST_TARGET_NOT_SIGNED",
	CodeSignatureExpired:    "SIGNATURE_EXPIRED",
	CodeSignerUnknown:       "SIGNER_UNKNOWN",
	CodeSignatureInvalid:    "SIGNATURE_INVALID",
	CodeSignatureObjObj:     "SIGNATURE_OVER_OBJECT_OBJECT",
	CodeSignerNotSender:     "SIGNER_NOT_SENDER",
	CodeBodyMalformed:       "BODY_MALFORMED",
	CodeActionPathMismatch:  "ACTION_PATH_MISMATCH",
	CodeVersionMismatch:     "VERSION_MISMATCH",
	CodeWrongDirection:      "WRONG_DIRECTION",
	CodePathCorrelation:     "PATH_CORRELATION_MISMATCH",
	CodeTTLExpired:          "TTL_EXPIRED",
	CodeTimestampInFuture:   "TIMESTAMP_IN_FUTURE",
	CodeContextInvalid:      "CONTEXT_INVALID",
	CodeWorkbenchRejected:   "WORKBENCH_REJECTED",
	CodeCounterpartyFailed:  "COUNTERPARTY_FAILED",
}

// CodeName returns the symbolic name of an FD-PWB code ("" if unknown).
func CodeName(code string) string { return codeNames[code] }

// gateError is a rejection fd-gate answers itself.
type gateError struct {
	status    int    // HTTP status; 200 for every rejection of a resolved FD call (D-2)
	code      string // FD-PWB-nnnn
	detail    string // human detail, after the code name
	fieldPath string // JSON Pointer into the request payload; "" = the whole payload
}

func (e *gateError) Error() string { return e.code + " " + e.message() }

func (e *gateError) message() string {
	if name := codeNames[e.code]; name != "" {
		return name + ": " + e.detail
	}
	return e.detail
}

// reject builds a 200 rejection.
func reject(code, fieldPath, format string, args ...any) *gateError {
	return &gateError{status: http.StatusOK, code: code, detail: fmt.Sprintf(format, args...), fieldPath: fieldPath}
}

// withStatus returns e with another HTTP status.
func (e *gateError) withStatus(status int) *gateError {
	c := *e
	c.status = status
	return &c
}

// fdError renders e as an FD Error, naming obliged as the party that must act.
func (e *gateError) fdError(obliged string) fdwire.Error {
	return fdwire.WorkbenchError(e.code, e.message(), obliged, e.fieldPath)
}

// writeBody writes a complete JSON response and flushes it, so the client has it before any
// post-response hook runs.
func writeBody(w http.ResponseWriter, status int, header http.Header, body []byte) {
	h := w.Header()
	for k, v := range header {
		h[k] = v
	}
	h.Del("Content-Length")
	h.Del("Transfer-Encoding")
	if h.Get("Content-Type") == "" && json.Valid(body) {
		h.Set("Content-Type", "application/json")
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
	_ = http.NewResponseController(w).Flush()
}

// writeNack writes e as a NACK, echoing ctx when it is known.
func writeNack(w http.ResponseWriter, ctx json.RawMessage, obliged string, e *gateError) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	writeBody(w, e.status, h, fdwire.NackBody(ctx, e.fdError(obliged)))
}
