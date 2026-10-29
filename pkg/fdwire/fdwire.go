// Package fdwire holds the small pieces of the FD (ONDC:FIS14:FD) wire format that the
// workbench's FD plugins share: the acknowledgement and error bodies, party names and
// ISO 8601 durations.
package fdwire

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Error classes and rejection reasons the workbench uses (values of the spec's ErrorClass
// and RejectionReason enums). Every FD-PWB-* code is PROTOCOL / FIELD_CONSTRAINT_VIOLATED
// (decision D-5), which the spec pairs with retryable=false and a fieldPath.
const (
	ClassProtocol                 = "PROTOCOL"
	ReasonFieldConstraintViolated = "FIELD_CONSTRAINT_VIOLATED"
)

// Ack statuses.
const (
	StatusACK  = "ACK"
	StatusNACK = "NACK"
)

// Party renders a route-table role as the spec's obligedParty value ("N-01 INVESTOR_APP",
// "N-02 DEPOSIT_ISSUER"). Other roles are returned unchanged.
func Party(role string) string {
	switch role {
	case "INVESTOR_APP":
		return "N-01 INVESTOR_APP"
	case "DEPOSIT_ISSUER":
		return "N-02 DEPOSIT_ISSUER"
	}
	return role
}

// Error is the FD protocol's Error object, with the fields the workbench fills.
//
// FieldPath is always written: the spec requires it whenever reason is
// FIELD_CONSTRAINT_VIOLATED. It is a JSON Pointer into the request payload; "" (the whole
// payload) when the fault is not in one field, e.g. a header.
type Error struct {
	Code         string `json:"code"`
	Class        string `json:"class"`
	Reason       string `json:"reason,omitempty"`
	Message      string `json:"message"`
	ObligedParty string `json:"obligedParty"`
	Retryable    bool   `json:"retryable"`
	FieldPath    string `json:"fieldPath"`
	Reference    string `json:"reference,omitempty"`
}

// Ack is the {"ack":{"status":...}} message of an acknowledgement.
type Ack struct {
	Ack struct {
		Status string `json:"status"`
	} `json:"ack"`
}

// Body is an acknowledgement: {context?, message:{ack:{status}}, error?}.
type Body struct {
	Context json.RawMessage `json:"context,omitempty"`
	Message Ack             `json:"message"`
	Error   *Error          `json:"error,omitempty"`
}

// WorkbenchError builds an FD-PWB-* error: class PROTOCOL, reason FIELD_CONSTRAINT_VIOLATED,
// not retryable.
func WorkbenchError(code, message, obligedParty, fieldPath string) Error {
	return Error{
		Code:         code,
		Class:        ClassProtocol,
		Reason:       ReasonFieldConstraintViolated,
		Message:      message,
		ObligedParty: obligedParty,
		FieldPath:    fieldPath,
	}
}

// AckBody renders {context, message:{ack:{status:"ACK"}}}. context is echoed verbatim and
// left out when empty; it must be valid JSON.
func AckBody(context json.RawMessage) []byte {
	b := Body{Context: validOrNil(context)}
	b.Message.Ack.Status = StatusACK
	return mustMarshal(b)
}

// NackBody renders {context?, message:{ack:{status:"NACK"}}, error}.
func NackBody(context json.RawMessage, e Error) []byte {
	b := Body{Context: validOrNil(context), Error: &e}
	b.Message.Ack.Status = StatusNACK
	return mustMarshal(b)
}

func validOrNil(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil
	}
	return raw
}

func mustMarshal(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil { // cannot happen: every field is a string, bool or valid raw JSON
		panic(fmt.Sprintf("fdwire: marshal: %v", err))
	}
	return data
}

// ParseISODuration parses an ISO 8601 duration as the FD context's ttl uses it
// (P[nY][nM][nW][nD][T[nH][nM][n[.n]S]]). Years count 365 days and months 30 days.
func ParseISODuration(s string) (time.Duration, error) {
	bad := func() (time.Duration, error) { return 0, fmt.Errorf("fdwire: invalid ISO 8601 duration %q", s) }
	if len(s) < 3 || s[0] != 'P' {
		return bad()
	}
	datePart, timePart, hasT := strings.Cut(s[1:], "T")
	if hasT && timePart == "" {
		return bad()
	}
	day := 24 * time.Hour
	date, err := sumUnits(datePart, []unit{{'Y', 365 * day}, {'M', 30 * day}, {'W', 7 * day}, {'D', day}}, false)
	if err != nil {
		return bad()
	}
	tm, err := sumUnits(timePart, []unit{{'H', time.Hour}, {'M', time.Minute}, {'S', time.Second}}, true)
	if err != nil {
		return bad()
	}
	total := date + tm
	if total > math.MaxInt64 || total < 0 {
		return bad()
	}
	return time.Duration(total), nil
}

type unit struct {
	designator byte
	size       time.Duration
}

// sumUnits adds "<n><designator>" groups, which must appear in the order of units, each at
// most once. Only the last unit may carry a fraction, and only when fracLast is set.
func sumUnits(s string, units []unit, fracLast bool) (float64, error) {
	var total float64
	next := 0
	for s != "" {
		i := 0
		for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
			i++
		}
		if i == 0 || i == len(s) {
			return 0, errors.New("malformed")
		}
		num, d := s[:i], s[i]
		s = s[i+1:]
		found := false
		for next < len(units) {
			u := units[next]
			next++
			if u.designator != d {
				continue
			}
			isLast := next == len(units)
			if strings.Contains(num, ".") && !(fracLast && isLast) {
				return 0, errors.New("fraction not allowed")
			}
			v, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, err
			}
			total += v * float64(u.size)
			found = true
			break
		}
		if !found {
			return 0, errors.New("unit out of order")
		}
	}
	return total, nil
}

// ParseDuration accepts a Go duration ("5s", "24h") or an ISO 8601 one ("PT5S", "P1D").
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "P") {
		return ParseISODuration(s)
	}
	return time.ParseDuration(s)
}
