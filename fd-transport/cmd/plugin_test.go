package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// The plugin manager looks Provider up and type-asserts the symbol (a pointer to the
// variable) to definition.TransportWrapperProvider.
var (
	_ definition.TransportWrapperProvider = Provider
	_ definition.TransportWrapperProvider = &Provider
)

type recordRT struct{ got *http.Request }

func (r *recordRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.got = req
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

func TestProviderNew(t *testing.T) {
	// The manager passes the YAML config's string values in a map[string]any.
	w, closer, err := Provider.New(context.Background(), map[string]any{"internalHosts": "mock:3000"})
	if err != nil {
		t.Fatal(err)
	}
	if closer != nil {
		t.Fatal("unexpected closer")
	}
	base := &recordRT{}
	rt := w.Wrap(base)

	req := httptest.NewRequest("GET", "http://participant.example.com/deposits/D-1", nil)
	req.RequestURI = ""
	req.Header.Set("X-Pwb-Internal-Key", "k")
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if base.got.Header.Get("X-Pwb-Internal-Key") != "" {
		t.Fatal("internal header leaked to participant")
	}

	if _, _, err := Provider.New(context.Background(), map[string]any{"internalHosts": 3}); err == nil {
		t.Fatal("want config error")
	}
	if _, _, err := Provider.New(nil, map[string]any{}); err == nil {
		t.Fatal("want error for nil context")
	}
}
