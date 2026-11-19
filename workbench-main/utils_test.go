package ondcworkbench

import (
	"net/url"
	"testing"
)

func TestWithSubscriberURL(t *testing.T) {
	const mock = "http://host.docker.internal:5004/mock/playground/manual"
	const seller = "http://workbench.localhost/api-service/ONDC:NTS10/2.1.0/seller"

	got := withSubscriberURL(mock, seller)
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("not a url: %q: %v", got, err)
	}
	if u.Path != "/mock/playground/manual" {
		t.Errorf("path changed: %q", u.Path)
	}
	if v := u.Query().Get("subscriber_url"); v != seller {
		t.Errorf("subscriber_url = %q, want %q", v, seller)
	}

	if got := withSubscriberURL(mock, ""); got != mock {
		t.Errorf("empty subscriber url changed the mock url: %q", got)
	}
}
