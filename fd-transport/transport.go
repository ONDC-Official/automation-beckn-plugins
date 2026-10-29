// Package fdtransport is the FD (ONDC:FIS14:FD) outbound transport wrapper. It wraps the
// ONIX handler's http.RoundTripper and — in later milestones — rebuilds the REST call from
// the internal POST .../<action> form and signs it.
//
// M1 skeleton: a request leaving for a participant (any host not listed as internal) is
// cloned with its Cookie and X-Pwb-* headers removed; requests to internal hosts (such as
// the mock service) pass unchanged.
package fdtransport

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdroute"
)

// Config is fd-transport's configuration.
type Config struct {
	// InternalHosts are the workbench-internal destinations, as host or host:port. A bare
	// host matches any port; host:port matches that port only (an absent port in the
	// request URL counts as the scheme's default).
	InternalHosts []string
}

// ParseConfig reads the plugin config map. internalHosts may be a comma-separated string
// (as the ONIX plugin manager passes it) or a list of strings.
func ParseConfig(cfg map[string]any) (*Config, error) {
	c := &Config{}
	raw, ok := cfg["internalHosts"]
	if !ok || raw == nil {
		return c, nil
	}
	var items []string
	switch v := raw.(type) {
	case string:
		items = strings.Split(v, ",")
	case []string:
		items = v
	case []any:
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("fd-transport: internalHosts entry %v is not a string", e)
			}
			items = append(items, s)
		}
	default:
		return nil, fmt.Errorf("fd-transport: internalHosts must be a comma-separated string, got %T", raw)
	}
	for _, it := range items {
		if it = strings.TrimSpace(it); it != "" {
			c.InternalHosts = append(c.InternalHosts, it)
		}
	}
	return c, nil
}

type hostKey struct {
	host string // lower-case, no brackets
	port string // "" = any port
}

// Wrapper implements beckn-onix's definition.TransportWrapper.
type Wrapper struct {
	internal map[hostKey]bool
}

// New builds a Wrapper.
func New(c *Config) *Wrapper {
	w := &Wrapper{internal: map[hostKey]bool{}}
	for _, h := range c.InternalHosts {
		w.internal[parseHost(h, "")] = true
	}
	return w
}

// Wrap returns base wrapped by fd-transport. A nil base means http.DefaultTransport.
func (w *Wrapper) Wrap(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &roundTripper{base: base, w: w}
}

// IsInternal reports whether a request to hostport (scheme supplies the default port) stays
// inside the workbench.
func (w *Wrapper) IsInternal(scheme, hostport string) bool {
	k := parseHost(hostport, defaultPort(scheme))
	return w.internal[k] || w.internal[hostKey{host: k.host}]
}

type roundTripper struct {
	base http.RoundTripper
	w    *Wrapper
}

func (rt *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	if host == "" {
		host = req.Host
	}
	if rt.w.IsInternal(req.URL.Scheme, host) {
		return rt.base.RoundTrip(req)
	}
	// A RoundTripper must not modify the caller's request: work on a clone.
	out := req.Clone(req.Context())
	out.Header.Del("Cookie")
	fdroute.StripInternal(out.Header)
	return rt.base.RoundTrip(out)
}

// parseHost splits "host[:port]" (IPv6 in brackets) into a lower-case key; port defaults
// to def.
func parseHost(hostport, def string) hostKey {
	hostport = strings.TrimSpace(hostport)
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return hostKey{host: strings.ToLower(h), port: p}
	}
	return hostKey{host: strings.ToLower(strings.Trim(hostport, "[]")), port: def}
}

func defaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}
