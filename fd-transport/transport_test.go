package fdtransport

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdroute"
)

// fakeRT records the request it was given.
type fakeRT struct{ got *http.Request }

func (f *fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.got = req
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

func newRequest(target string) *http.Request {
	req := httptest.NewRequest("POST", target, nil)
	req.RequestURI = "" // client request
	req.Header.Set("Cookie", "session=1")
	req.Header.Set(fdroute.HeaderInternalKey, "secret")
	req.Header.Set(fdroute.HeaderFdEnvelope, "{}")
	req.Header["x-pwb-raw"] = []string{"z"}
	req.Header.Set("Authorization", "Signature keyId=\"a|b|ed25519\"")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestParseConfig(t *testing.T) {
	tests := []struct {
		in   map[string]any
		want []string
	}{
		{map[string]any{}, nil},
		{map[string]any{"internalHosts": ""}, nil},
		{map[string]any{"internalHosts": " mock-service:3000 , localhost,, [::1]:8080 "}, []string{"mock-service:3000", "localhost", "[::1]:8080"}},
		{map[string]any{"internalHosts": []string{"a", " b "}}, []string{"a", "b"}},
		{map[string]any{"internalHosts": []any{"a", "b:1"}}, []string{"a", "b:1"}},
	}
	for _, tt := range tests {
		c, err := ParseConfig(tt.in)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(c.InternalHosts, tt.want) {
			t.Fatalf("%v: got %v want %v", tt.in, c.InternalHosts, tt.want)
		}
	}
	for _, bad := range []map[string]any{{"internalHosts": 5}, {"internalHosts": []any{"a", 1}}} {
		if _, err := ParseConfig(bad); err == nil {
			t.Fatalf("want error for %v", bad)
		}
	}
}

func TestExternalRequestIsScrubbed(t *testing.T) {
	base := &fakeRT{}
	rt := New(&Config{InternalHosts: []string{"mock-service:3000"}}).Wrap(base)

	req := newRequest("https://participant.example.com/deposits/D-1")
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	out := base.got
	if out == req {
		t.Fatal("external request was not cloned")
	}
	if len(out.Header) != 2 || out.Header.Get("Authorization") == "" || out.Header.Get("Content-Type") == "" {
		t.Fatalf("outbound headers = %v", out.Header)
	}
	// The caller's request is untouched.
	if req.Header.Get("Cookie") == "" || req.Header.Get(fdroute.HeaderInternalKey) == "" || len(req.Header["x-pwb-raw"]) == 0 {
		t.Fatalf("caller's request was modified: %v", req.Header)
	}
}

func TestInternalRequestPassesUnchanged(t *testing.T) {
	base := &fakeRT{}
	rt := New(&Config{InternalHosts: []string{"mock-service:3000", "LocalHost", "[::1]:8080", "api:80"}}).Wrap(base)
	for _, target := range []string{
		"http://mock-service:3000/mock/on_search",
		"http://localhost:5555/x", // bare host matches any port
		"http://LOCALHOST/x",
		"http://[::1]:8080/x",
		"http://api/x", // default port 80
	} {
		req := newRequest(target)
		before := req.Header.Clone()
		if _, err := rt.RoundTrip(req); err != nil {
			t.Fatal(err)
		}
		if base.got != req || !reflect.DeepEqual(base.got.Header, before) {
			t.Fatalf("%s: internal request was changed", target)
		}
	}
	for _, target := range []string{
		"http://mock-service:3001/x", // other port
		"https://api/x",              // default port 443, entry is :80
		"http://[::1]:9090/x",
		"http://mock-service.evil.com:3000/x",
	} {
		req := newRequest(target)
		if _, err := rt.RoundTrip(req); err != nil {
			t.Fatal(err)
		}
		if base.got == req || base.got.Header.Get("Cookie") != "" {
			t.Fatalf("%s: treated as internal", target)
		}
	}
}

func TestNoInternalHostsScrubsEverything(t *testing.T) {
	base := &fakeRT{}
	rt := New(&Config{}).Wrap(base)
	if _, err := rt.RoundTrip(newRequest("http://localhost:3000/x")); err != nil {
		t.Fatal(err)
	}
	if base.got.Header.Get(fdroute.HeaderInternalKey) != "" {
		t.Fatal("internal header leaked")
	}
}

func TestWrapNilBase(t *testing.T) {
	rt := New(&Config{}).Wrap(nil).(*roundTripper)
	if rt.base != http.DefaultTransport {
		t.Fatal("nil base should mean http.DefaultTransport")
	}
}
