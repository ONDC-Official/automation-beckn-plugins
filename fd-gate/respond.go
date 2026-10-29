package fdgate

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdroute"
	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdwire"
)

// bufferWriter captures the inner chain's response so fd-gate can shape it before anything
// reaches the client.
type bufferWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newBufferWriter() *bufferWriter { return &bufferWriter{header: http.Header{}} }

func (b *bufferWriter) Header() http.Header { return b.header }

func (b *bufferWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

func (b *bufferWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

// Flush is a no-op: nothing leaves fd-gate before the response is complete. It exists so
// httputil.ReverseProxy's flushes succeed.
func (b *bufferWriter) Flush() {}

// response is a complete response ready to write.
type response struct {
	status   int
	header   http.Header
	body     []byte
	storable bool // may be stored for replay
}

func (r *response) contentType() string { return r.header.Get("Content-Type") }

// innerAck is the part of an acknowledgement body fd-gate reads.
type innerAck struct {
	Message *struct {
		Ack *struct {
			Status string `json:"status"`
		} `json:"ack"`
	} `json:"message"`
	Error json.RawMessage `json:"error"`
}

// shapeNP turns the inner chain's response into the participant's (§3 np step 8):
// an ACK becomes 202 (asynchronous request) or 200 (callback, webhook) with the request's
// context echoed; a Beckn NACK becomes a 200 FD Error; a proxy failure becomes
// COUNTERPARTY_FAILED; a SYNC answer passes through.
func (g *Gate) shapeNP(route *fdroute.Route, reqCtx json.RawMessage, bw *bufferWriter, obliged string) *response {
	status, body := bw.status, bw.body.Bytes()
	if status == 0 {
		status = http.StatusOK
	}
	nack := func(e *gateError, storable bool) *response {
		h := cleanHeader(bw.header, true)
		h.Set("Content-Type", "application/json")
		return &response{status: e.status, header: h, body: fdwire.NackBody(reqCtx, e.fdError(obliged)), storable: storable}
	}
	passThrough := func(status int) *response {
		return &response{status: status, header: cleanHeader(bw.header, false), body: append([]byte(nil), body...), storable: status < 500}
	}

	if status == http.StatusBadGateway || status == http.StatusGatewayTimeout || len(bytes.TrimSpace(body)) == 0 {
		return nack(reject(CodeCounterpartyFailed, "", "the workbench's counterparty did not answer %s (HTTP %d, %d-byte body)", route.Action, status, len(body)), false)
	}

	var ack innerAck
	isJSON := !encoded(bw.header) && json.Unmarshal(body, &ack) == nil
	ackStatus := ""
	if isJSON && ack.Message != nil && ack.Message.Ack != nil {
		ackStatus = ack.Message.Ack.Status
	}

	switch ackStatus {
	case fdwire.StatusNACK:
		if isFDError(ack.Error) {
			return passThrough(http.StatusOK) // already an FD rejection (e.g. the mock's SYNC NACK)
		}
		var be struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Paths   string `json:"paths"`
		}
		_ = json.Unmarshal(ack.Error, &be)
		detail := strings.TrimSpace(be.Code + ": " + be.Message)
		if be.Code == "" {
			detail = be.Message
		}
		if status >= 500 {
			return nack(reject(CodeInternal, pathToPointer(be.Paths), "%s", detail), false)
		}
		return nack(reject(CodeWorkbenchRejected, pathToPointer(be.Paths), "%s", detail), true)

	case fdwire.StatusACK:
		h := cleanHeader(bw.header, true)
		h.Set("Content-Type", "application/json")
		switch {
		case route.Kind == fdroute.KindRequest && route.Mode == fdroute.ModeAsyncAckCallback:
			return &response{status: http.StatusAccepted, header: h, body: fdwire.AckBody(reqCtx), storable: true}
		case route.Kind == fdroute.KindCallback || route.Kind == fdroute.KindWebhook:
			return &response{status: http.StatusOK, header: h, body: fdwire.AckBody(reqCtx), storable: true}
		}
		return passThrough(status)
	}

	if status >= 500 {
		if route.Mode == fdroute.ModeSync {
			return nack(reject(CodeCounterpartyFailed, "", "the workbench's counterparty failed on %s (HTTP %d)", route.Action, status), false)
		}
		return nack(reject(CodeInternal, "", "the workbench failed on %s (HTTP %d)", route.Action, status), false)
	}
	return passThrough(status)
}

// isFDError reports whether an error object is an FD Error (it has a class), as opposed to
// a Beckn error ({code, message, paths}).
func isFDError(raw json.RawMessage) bool {
	var e map[string]json.RawMessage
	if json.Unmarshal(raw, &e) != nil {
		return false
	}
	_, ok := e["class"]
	return ok
}

// encoded reports whether the body carries a content coding fd-gate cannot read.
func encoded(h http.Header) bool {
	ce := strings.TrimSpace(h.Get("Content-Encoding"))
	return ce != "" && !strings.EqualFold(ce, "identity")
}

// cleanHeader copies the inner response's headers for a rewritten (reshaped=true) or
// passed-through body. Length and, for a rewritten body, encoding and type are reset.
func cleanHeader(h http.Header, reshaped bool) http.Header {
	out := h.Clone()
	out.Del("Content-Length")
	out.Del("Transfer-Encoding")
	if reshaped {
		out.Del("Content-Encoding")
		out.Del("Content-Type")
	}
	return out
}

// stripForParticipant removes what a participant must not see on a response: internal
// headers and cookies.
func stripForParticipant(h http.Header) {
	fdroute.StripInternal(h)
	h.Del("Set-Cookie")
}

// pathToPointer turns an inner error's path ("$.context.action", "message.items[0].id" or
// already "/context/action") into a JSON Pointer; the first of several paths is used.
func pathToPointer(p string) string {
	p = strings.TrimSpace(p)
	if i := strings.IndexAny(p, ", \n"); i >= 0 {
		p = p[:i]
	}
	if p == "" || strings.HasPrefix(p, "/") {
		return p
	}
	p = strings.TrimPrefix(strings.TrimPrefix(p, "$"), ".")
	var b strings.Builder
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '.' || r == '[' || r == ']' }) {
		part = strings.Trim(part, `'"`)
		b.WriteByte('/')
		b.WriteString(strings.NewReplacer("~", "~0", "/", "~1").Replace(part))
	}
	return b.String()
}
