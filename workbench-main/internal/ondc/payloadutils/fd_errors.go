package payloadutils

import "github.com/beckn-one/beckn-onix/pkg/model"

// Named NACK codes of the FD (ONDC:FIS14:FD) branches. fd-gate wraps an inner NACK in an FD
// Error and keeps the inner code and message verbatim, so these names reach the NP.
const (
	// CodeMessageIDReused: a message_id already used in the transaction, with a different body.
	CodeMessageIDReused = "MESSAGE_ID_REUSED"
	// CodeExpectationExpired: the request matches only an expectation that has expired.
	CodeExpectationExpired = "EXPECTATION_EXPIRED"
)

// NewNamedNackError is a NACK (HTTP 200) whose error.code is a name rather than an HTTP status.
func NewNamedNackError(code, msg string, ondcContext any) error {
	return &model.WorkbenchErr{
		Err: model.Error{
			Code:    code,
			Message: WithWorkbenchPrefix(msg),
			Context: ondcContext,
		},
		Behavior: "NACK",
	}
}
