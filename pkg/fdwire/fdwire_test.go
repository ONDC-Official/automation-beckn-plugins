package fdwire

import (
	"encoding/json"
	"testing"
	"time"
)

func TestParseISODuration(t *testing.T) {
	good := map[string]time.Duration{
		"PT30S":       30 * time.Second,
		"PT5M":        5 * time.Minute,
		"PT1H30M":     90 * time.Minute,
		"PT0.5S":      500 * time.Millisecond,
		"P1D":         24 * time.Hour,
		"P1W":         7 * 24 * time.Hour,
		"P1DT2H":      26 * time.Hour,
		"P1Y":         365 * 24 * time.Hour,
		"P1M":         30 * 24 * time.Hour,
		"P1MT1M":      30*24*time.Hour + time.Minute,
		"PT1H2M3.25S": time.Hour + 2*time.Minute + 3250*time.Millisecond,
	}
	for in, want := range good {
		got, err := ParseISODuration(in)
		if err != nil || got != want {
			t.Fatalf("%s: got %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "P", "PT", "P1DT", "30S", "PT30", "PTS", "PT1S1M", "P1.5D", "PT1.5M", "PT5X", "P1H", "pt30s", "PT-5S"} {
		if _, err := ParseISODuration(in); err == nil {
			t.Fatalf("%q: want error", in)
		}
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"5s": 5 * time.Second, " 24h ": 24 * time.Hour, "PT5S": 5 * time.Second, "P1D": 24 * time.Hour} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Fatalf("%q: got %v, %v", in, got, err)
		}
	}
	if _, err := ParseDuration("soon"); err == nil {
		t.Fatal("want error")
	}
}

func TestBodies(t *testing.T) {
	ctx := json.RawMessage(`{"action":"application_confirm","message_id":"m-1"}`)
	if got := string(AckBody(ctx)); got != `{"context":{"action":"application_confirm","message_id":"m-1"},"message":{"ack":{"status":"ACK"}}}` {
		t.Fatalf("ack = %s", got)
	}
	if got := string(AckBody(nil)); got != `{"message":{"ack":{"status":"ACK"}}}` {
		t.Fatalf("ack without context = %s", got)
	}
	e := WorkbenchError("FD-PWB-0010", "SIGNATURE_MISSING: no Authorization header", Party("INVESTOR_APP"), "")
	want := `{"context":{"action":"application_confirm","message_id":"m-1"},"message":{"ack":{"status":"NACK"}},"error":{"code":"FD-PWB-0010","class":"PROTOCOL","reason":"FIELD_CONSTRAINT_VIOLATED","message":"SIGNATURE_MISSING: no Authorization header","obligedParty":"N-01 INVESTOR_APP","retryable":false,"fieldPath":""}}`
	if got := string(NackBody(ctx, e)); got != want {
		t.Fatalf("nack:\n got %s\nwant %s", got, want)
	}
	// Invalid context JSON is dropped rather than breaking the body.
	if got := string(NackBody(json.RawMessage(`{"a":`), e)); got[:9] != `{"message` {
		t.Fatalf("invalid context not dropped: %s", got)
	}
}

func TestParty(t *testing.T) {
	if Party("INVESTOR_APP") != "N-01 INVESTOR_APP" || Party("DEPOSIT_ISSUER") != "N-02 DEPOSIT_ISSUER" || Party("INDEX") != "INDEX" {
		t.Fatal("party names")
	}
}
