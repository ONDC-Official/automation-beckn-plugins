package fdgate

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdroute"
)

// readContext is the context fd-gate writes for a GET read, which has no body of its own.
type readContext struct {
	Domain   string `json:"domain"`
	Location struct {
		Country struct {
			Code string `json:"code"`
		} `json:"country"`
		City struct {
			Code string `json:"code"`
		} `json:"city"`
	} `json:"location"`
	Version       string `json:"version"`
	Action        string `json:"action"`
	BapID         string `json:"bap_id"`
	BapURI        string `json:"bap_uri,omitempty"`
	BppID         string `json:"bpp_id,omitempty"`
	BppURI        string `json:"bpp_uri,omitempty"`
	TransactionID string `json:"transaction_id,omitempty"`
	MessageID     string `json:"message_id"`
	Timestamp     string `json:"timestamp"`
	TTL           string `json:"ttl"`
}

type readEnvelope struct {
	EnvelopeVersion string            `json:"envelopeVersion"`
	PayloadVersion  string            `json:"payloadVersion"`
	Mode            string            `json:"mode"`
	OriginatorRole  string            `json:"originatorRole"`
	Correlation     map[string]string `json:"correlation"`
}

type readMessage struct {
	PathParams map[string]string `json:"pathParams"`
	Query      map[string]string `json:"query"`
}

type readBody struct {
	Context    readContext  `json:"context"`
	FdEnvelope readEnvelope `json:"fdEnvelope"`
	Message    readMessage  `json:"message"`
}

// newReadContext builds a read's context. ref, from the object index, supplies the
// transaction and endpoints; without one the read is placed in a fresh transaction.
func (g *Gate) newReadContext(route *fdroute.Route, signer string, ref *ObjectRef) readContext {
	c := readContext{
		Domain: g.table.Domain, Version: g.table.Version, Action: route.Action,
		BapID: signer, MessageID: newUUID(),
		Timestamp: g.now().UTC().Format("2006-01-02T15:04:05.000Z07:00"), TTL: "PT30S",
	}
	c.Location.Country.Code = "IND"
	c.Location.City.Code = "*"
	if ref != nil {
		if c.BapID == "" {
			c.BapID = ref.BapID
		}
		c.BapURI, c.BppID, c.BppURI, c.TransactionID = ref.BapURI, ref.BppID, ref.BppURI, ref.TransactionID
	} else {
		c.TransactionID = newUUID()
	}
	return c
}

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil { // cannot happen: strings and maps of strings only
		panic(fmt.Sprintf("fd-gate: marshal: %v", err))
	}
	return data
}

// buildReadBody turns a GET into the internal POST body the steps understand (M3):
// context from the object index, an envelope whose correlation is the path parameters,
// and {pathParams, query} as the message. A path parameter that names a correlation
// identifier must be in the index, else OBJECT_UNKNOWN. It returns the body and its context.
func (g *Gate) buildReadBody(ctx context.Context, route *fdroute.Route, params map[string]string, query url.Values, signer string) ([]byte, json.RawMessage, *gateError) {
	var ref *ObjectRef
	var wanted []string
	for i := len(route.PathParams) - 1; i >= 0 && ref == nil; i-- {
		p := route.PathParams[i]
		if !correlationKeys[p] {
			continue
		}
		wanted = append(wanted, fmt.Sprintf("%s %q", p, params[p]))
		found, err := g.lookupObject(ctx, p, params[p])
		switch {
		case err == nil:
			ref = found
		case !errors.Is(err, ErrNotFound):
			c := g.newReadContext(route, signer, nil)
			return nil, mustJSON(c), reject(CodeInternal, "", "the object index is unavailable: %v", err)
		}
	}
	rc := g.newReadContext(route, signer, ref)
	if ref == nil && len(wanted) > 0 {
		return nil, mustJSON(rc), reject(CodeObjectUnknown, "", "the workbench knows no %s: no FD message has named it", wanted[0])
	}

	q := map[string]string{}
	for k, v := range query {
		if len(v) > 0 {
			q[k] = v[0]
		}
	}
	pp := make(map[string]string, len(params))
	for k, v := range params {
		pp[k] = v
	}
	body := readBody{
		Context: rc,
		FdEnvelope: readEnvelope{
			EnvelopeVersion: "1.0", PayloadVersion: g.table.Version, Mode: route.Mode,
			OriginatorRole: route.Caller, Correlation: pp,
		},
		Message: readMessage{PathParams: pp, Query: q},
	}
	return mustJSON(body), mustJSON(rc), nil
}

// newUUID returns a random (version 4) UUID.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms; fall back to the clock.
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (8 * (i % 8)))
		}
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
