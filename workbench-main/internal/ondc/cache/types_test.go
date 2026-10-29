package cache

import (
	"encoding/json"
	"testing"
)

// A Beckn entry (no FD fields set) must marshal exactly as it did before the FD fields existed.
func TestApiDataBecknJSONUnchanged(t *testing.T) {
	ttl := int64(30)
	entry := ApiData{
		EntryType:     "API",
		Action:        "on_select",
		PayloadId:     "p1",
		MessageId:     "m1",
		Response:      map[string]any{"message": map[string]any{"ack": map[string]any{"status": "ACK"}}},
		Timestamp:     "2026-01-01T00:00:00.000Z",
		RealTimestamp: "2026-01-01T00:00:00.100Z",
		TTL:           &ttl,
	}
	got, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	want := `{"entryType":"API","action":"on_select","payloadId":"p1","messageId":"m1",` +
		`"response":{"message":{"ack":{"status":"ACK"}}},"timestamp":"2026-01-01T00:00:00.000Z",` +
		`"realTimestamp":"2026-01-01T00:00:00.100Z","ttl":30}`
	if string(got) != want {
		t.Fatalf("Marshal() =\n%s\nwant\n%s", got, want)
	}
}

func TestApiDataFDFields(t *testing.T) {
	got, _ := json.Marshal(ApiData{EntryType: "API", Action: "on_application_create", InReplyTo: "m1", FdReplay: true})
	want := `{"entryType":"API","action":"on_application_create","payloadId":"","messageId":"",` +
		`"response":null,"timestamp":"","realTimestamp":"","inReplyTo":"m1","fdReplay":true}`
	if string(got) != want {
		t.Fatalf("Marshal() =\n%s\nwant\n%s", got, want)
	}

	// The recorder writes entries as plain JSON objects; the FD fields decode from them.
	var decoded ApiData
	raw := `{"entryType":"API","action":"on_application_create","messageId":"m2","inReplyTo":"m1","fdReplay":true}`
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if decoded.InReplyTo != "m1" || !decoded.FdReplay {
		t.Fatalf("decoded = %+v, want InReplyTo m1 and FdReplay true", decoded)
	}
}
