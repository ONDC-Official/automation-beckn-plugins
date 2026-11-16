package fdgate

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdroute"
	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdwire"
	"github.com/beckn-one/beckn-onix/pkg/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// EnvRecorderGRPC is the recorder's gRPC address the container entrypoint uses
// (RECORDER_SERVICE_GRPC_URL); fd-gate's recorderTarget defaults to it.
const EnvRecorderGRPC = "RECORDER_SERVICE_GRPC_URL"

// recorderMethod is the recorder's audit method, as network-observability calls it.
const recorderMethod = "/beckn.audit.v1.AuditService/LogEvent"

// Edge event kinds.
const (
	EdgeRefused = "refused" // fd-gate refused the participant's message itself
	EdgeReplay  = "replay"  // fd-gate served the stored response to a byte-identical repeat
)

// EdgeEvent is an exchange fd-gate answered itself. network-observability runs inside
// fd-gate, so it never sees these; without them the report could not name why a
// participant's message was refused at the edge, or that it was repeated.
type EdgeEvent struct {
	Kind     string // EdgeRefused | EdgeReplay
	Code     string // the FD-PWB code of a refusal
	Route    *fdroute.Route
	Method   string // the REST call as received
	Path     string // without the module prefix, with any query
	Body     []byte // the participant's body, as received
	Status   int    // the answer's HTTP status
	Response []byte // the answer's body
}

// Auditor records edge events. Implementations must not block the request.
type Auditor interface {
	Audit(ctx context.Context, ev EdgeEvent)
}

// edgeAuditPayload is the recorder's LogEvent payload for an edge event: the shape
// network-observability sends ({requestBody, responseBody, additionalData}), with the FD
// remap's fields. It returns false when the body names no transaction, which the recorder
// could not file.
func edgeAuditPayload(ev EdgeEvent, payloadID string) ([]byte, bool) {
	if ev.Route == nil {
		return nil, false
	}
	var request map[string]any
	if json.Unmarshal(ev.Body, &request) != nil || request == nil {
		return nil, false
	}
	b, perr := parseFDBody(ev.Body)
	if perr != nil {
		b = &fdBody{context: map[string]any{}}
		if c, ok := request["context"].(map[string]any); ok {
			b.context = c
		}
	}
	txn, subscriber := b.str("transaction_id"), senderURI(ev.Route, b)
	if txn == "" || subscriber == "" {
		return nil, false
	}
	var response map[string]any
	if json.Unmarshal(ev.Response, &response) != nil || response == nil {
		response = map[string]any{}
	}
	edge := ev.Kind
	if ev.Code != "" {
		edge = ev.Code
	}
	// As network-observability's headerMaps gives them: lower-case names, value lists.
	headers := map[string][]string{
		strings.ToLower(fdroute.HeaderFdOrigin): {fdroute.OriginNP},
		strings.ToLower(fdroute.HeaderFdRoute):  {ev.Route.Action},
		strings.ToLower(fdroute.HeaderFdMethod): {ev.Method},
		strings.ToLower(fdroute.HeaderFdPath):   {ev.Path},
		"x-pwb-fd-edge":                         {edge},
	}
	inReplyTo := ""
	if b.envelope != nil {
		inReplyTo, _ = b.envelope["inReplyTo"].(string)
	}
	data := map[string]any{
		"payload_id":        payloadID,
		"transaction_id":    txn,
		"message_id":        b.str("message_id"),
		"subscriber_url":    subscriber,
		"action":            ev.Route.Action,
		"timestamp":         b.str("timestamp"),
		"api_name":          ev.Route.Action,
		"status_code":       ev.Status,
		"cache_ttl_seconds": 0,
		"is_mock":           false,
		"in_reply_to":       inReplyTo,
		"fd_replay":         ev.Kind == EdgeReplay,
		"fd_edge":           edge,
		"request_headers":   headers,
	}
	out, err := json.Marshal(map[string]any{"requestBody": request, "responseBody": response, "additionalData": data})
	return out, err == nil
}

// GRPCAuditor sends edge events to the recorder over gRPC, from a bounded queue on one
// worker: a slow or absent recorder never delays the participant's answer; events that do
// not fit the queue are dropped and logged.
type GRPCAuditor struct {
	target  string
	timeout time.Duration
	queue   chan []byte

	once sync.Once
	conn *grpc.ClientConn
}

// NewGRPCAuditor returns an auditor for the recorder at target (host:port, plaintext as
// network-observability's grpc_insecure).
func NewGRPCAuditor(target string) *GRPCAuditor {
	a := &GRPCAuditor{target: target, timeout: 5 * time.Second, queue: make(chan []byte, 256)}
	go a.run()
	return a
}

// Audit implements Auditor.
func (a *GRPCAuditor) Audit(ctx context.Context, ev EdgeEvent) {
	payload, ok := edgeAuditPayload(ev, newUUID())
	if !ok {
		return
	}
	select {
	case a.queue <- payload:
	default:
		log.Warnf(ctx, "fd-gate: recorder queue full; edge event for %s dropped", ev.Route.Action)
	}
}

func (a *GRPCAuditor) run() {
	for payload := range a.queue {
		a.send(payload)
	}
}

func (a *GRPCAuditor) send(payload []byte) {
	ctx := context.Background()
	a.once.Do(func() {
		conn, err := grpc.NewClient(a.target, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			log.Errorf(ctx, err, "fd-gate: cannot reach the recorder at %s; edge events are not recorded", a.target)
			return
		}
		a.conn = conn
	})
	if a.conn == nil {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	if err := a.conn.Invoke(callCtx, recorderMethod, &wrapperspb.BytesValue{Value: payload}, &emptypb.Empty{}); err != nil {
		log.Warnf(ctx, "fd-gate: recorder refused an edge event: %v", err)
	}
}

// audit records an edge event when an auditor is configured.
func (g *Gate) audit(ctx context.Context, ev EdgeEvent) {
	if g.auditor != nil {
		g.auditor.Audit(ctx, ev)
	}
}

// refuse answers a participant's message with fd-gate's own NACK and records it.
func (g *Gate) refuse(w http.ResponseWriter, ev EdgeEvent, echo json.RawMessage, obliged string, e *gateError) {
	writeNack(w, echo, obliged, e)
	if g.auditor == nil {
		return
	}
	ev.Kind, ev.Code, ev.Status = EdgeRefused, e.code, e.status
	ev.Response = fdwire.NackBody(echo, e.fdError(obliged))
	g.auditor.Audit(context.Background(), ev)
}
