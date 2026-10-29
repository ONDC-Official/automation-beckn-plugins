// Package fdroute loads and matches the FD (ONDC:FIS14:FD) route table, format v1.
//
// The route table is the single description of FD's operations shared by every workbench
// component (see FD-ROUTE-TABLE.md). The api-service generator writes it to
// config/fd_routes.yaml; the FD plugins (fd-gate, fd-transport) load it from there.
package fdroute

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FormatVersion is the route-table format this package understands.
const FormatVersion = 1

// Route kinds.
const (
	KindRequest  = "request"
	KindCallback = "callback"
	KindWebhook  = "webhook"
)

// Interaction modes.
const (
	ModeSync             = "SYNC"
	ModeAsyncAckCallback = "ASYNC_ACK_CALLBACK"
	ModeWebhook          = "WEBHOOK"
)

// Participant roles (the spec's "N-01 " / "N-02 " prefixes are dropped in the table).
const (
	RoleInvestorApp   = "INVESTOR_APP"
	RoleDepositIssuer = "DEPOSIT_ISSUER"
	RoleIndex         = "INDEX"
)

// Table is an FD route table (the content of `x-fd-routes`).
type Table struct {
	FormatVersion int     `yaml:"formatVersion" json:"formatVersion"`
	Domain        string  `yaml:"domain" json:"domain"`
	Version       string  `yaml:"version" json:"version"`
	TableHash     string  `yaml:"tableHash" json:"tableHash"`
	Routes        []Route `yaml:"routes" json:"routes"`

	byAction map[string]*Route
	trees    map[string]*node // method -> path trie
}

// Route is one FD operation. Nullable fields of the format are pointers (nil = null).
type Route struct {
	Action       string   `yaml:"action" json:"action"`
	Kind         string   `yaml:"kind" json:"kind"`
	Method       string   `yaml:"method" json:"method"`
	Path         string   `yaml:"path" json:"path"`
	PathParams   []string `yaml:"pathParams" json:"pathParams"`
	Mode         string   `yaml:"mode" json:"mode"`
	Caller       string   `yaml:"caller" json:"caller"`
	Callee       string   `yaml:"callee" json:"callee"`
	Callback     *string  `yaml:"callback" json:"callback"`
	InReplyTo    *string  `yaml:"inReplyTo" json:"inReplyTo"`
	HasBody      bool     `yaml:"hasBody" json:"hasBody"`
	VersionGuard *string  `yaml:"versionGuard" json:"versionGuard"`
	SLA          *string  `yaml:"sla" json:"sla"`
	Requirements []string `yaml:"requirements" json:"requirements"`
	OperationID  string   `yaml:"operationId" json:"operationId"`
}

// ErrNoRoute is matched (errors.Is) by every "no route for this request" error from Match.
var ErrNoRoute = errors.New("fdroute: no route")

// NoRouteError reports that no route matches a method and path. Allowed lists the methods
// that do have a route for the path (empty when the path itself is unknown).
type NoRouteError struct {
	Method  string
	Path    string
	Allowed []string
}

func (e *NoRouteError) Error() string {
	if len(e.Allowed) > 0 {
		return fmt.Sprintf("fdroute: no route for %s %s (path is routed for %s)", e.Method, e.Path, strings.Join(e.Allowed, ", "))
	}
	return fmt.Sprintf("fdroute: no route for %s %s", e.Method, e.Path)
}

// Is makes errors.Is(err, ErrNoRoute) true.
func (e *NoRouteError) Is(target error) bool { return target == ErrNoRoute }

// Load reads a route table YAML file (top-level keys formatVersion, domain, version,
// tableHash, routes; a file wrapped in a single `x-fd-routes:` key is accepted too),
// verifies its tableHash and builds the match index.
func Load(path string) (*Table, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fdroute: read %s: %w", path, err)
	}
	t, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%w (file %s)", err, path)
	}
	return t, nil
}

// Parse is Load over in-memory YAML.
func Parse(data []byte) (*Table, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("fdroute: parse yaml: %w", err)
	}
	root := &doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return nil, errors.New("fdroute: empty route table")
		}
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("fdroute: route table must be a YAML mapping")
	}
	if len(root.Content) == 2 && root.Content[0].Value == "x-fd-routes" {
		root = root.Content[1]
		if root.Kind != yaml.MappingNode {
			return nil, errors.New("fdroute: x-fd-routes must be a YAML mapping")
		}
	}

	var t Table
	if err := root.Decode(&t); err != nil {
		return nil, fmt.Errorf("fdroute: decode route table: %w", err)
	}
	if t.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("fdroute: unsupported formatVersion %d (want %d)", t.FormatVersion, FormatVersion)
	}
	if t.Domain == "" || t.Version == "" {
		return nil, errors.New("fdroute: domain and version are required")
	}

	// The hash is taken over the routes exactly as written (generic values), so fields this
	// version does not model still count.
	routesNode := mappingValue(root, "routes")
	if routesNode == nil {
		return nil, errors.New("fdroute: routes is required")
	}
	var generic any
	if err := routesNode.Decode(&generic); err != nil {
		return nil, fmt.Errorf("fdroute: decode routes: %w", err)
	}
	got, err := ComputeHash(generic)
	if err != nil {
		return nil, err
	}
	if t.TableHash == "" {
		return nil, errors.New("fdroute: tableHash is required")
	}
	if strings.ToLower(t.TableHash) != got {
		return nil, fmt.Errorf("fdroute: tableHash mismatch: table says %s, routes hash to %s", t.TableHash, got)
	}

	if err := t.index(); err != nil {
		return nil, err
	}
	return &t, nil
}

// ByAction returns the route with the given x-fd-action.
func (t *Table) ByAction(action string) (*Route, bool) {
	r, ok := t.byAction[action]
	return r, ok
}

// Match resolves a request to a route. method is matched first (case-insensitively), then
// path. path is the request's escaped path (r.URL.EscapedPath(), any module prefix already
// removed); a query string, if present, is ignored. A literal segment beats a {param}
// segment; a {param} matches one non-empty segment and its value is percent-decoded.
// The returned map holds the path parameters by name. Every "not found" error satisfies
// errors.Is(err, ErrNoRoute).
func (t *Table) Match(method, path string) (*Route, map[string]string, error) {
	method = strings.ToUpper(method)
	segs, ok := splitRequestPath(path)
	if !ok {
		return nil, nil, &NoRouteError{Method: method, Path: path}
	}
	if root := t.trees[method]; root != nil {
		if n, vals := root.match(segs, nil); n != nil {
			params := make(map[string]string, len(vals))
			for i, name := range n.route.PathParams {
				params[name] = vals[i]
			}
			return n.route, params, nil
		}
	}
	var allowed []string
	for m, root := range t.trees {
		if m == method {
			continue
		}
		if n, _ := root.match(segs, nil); n != nil {
			allowed = append(allowed, m)
		}
	}
	sort.Strings(allowed)
	return nil, nil, &NoRouteError{Method: method, Path: path, Allowed: allowed}
}

var actionRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

var (
	validKinds   = map[string]bool{KindRequest: true, KindCallback: true, KindWebhook: true}
	validModes   = map[string]bool{ModeSync: true, ModeAsyncAckCallback: true, ModeWebhook: true}
	validMethods = map[string]bool{"GET": true, "POST": true}
	validCallers = map[string]bool{RoleInvestorApp: true, RoleDepositIssuer: true}
	validCallees = map[string]bool{RoleInvestorApp: true, RoleDepositIssuer: true, RoleIndex: true}
)

// index validates the routes and builds the action map and per-method tries.
func (t *Table) index() error {
	if len(t.Routes) == 0 {
		return errors.New("fdroute: route table has no routes")
	}
	t.byAction = make(map[string]*Route, len(t.Routes))
	t.trees = make(map[string]*node)
	templates := make(map[string]string) // method + normalised template -> action

	for i := range t.Routes {
		r := &t.Routes[i]
		if !actionRe.MatchString(r.Action) {
			return fmt.Errorf("fdroute: route %d: invalid action %q", i, r.Action)
		}
		if _, dup := t.byAction[r.Action]; dup {
			return fmt.Errorf("fdroute: duplicate action %q", r.Action)
		}
		if !validKinds[r.Kind] {
			return fmt.Errorf("fdroute: route %s: invalid kind %q", r.Action, r.Kind)
		}
		if !validModes[r.Mode] {
			return fmt.Errorf("fdroute: route %s: invalid mode %q", r.Action, r.Mode)
		}
		if !validMethods[r.Method] {
			return fmt.Errorf("fdroute: route %s: invalid method %q", r.Action, r.Method)
		}
		if !validCallers[r.Caller] {
			return fmt.Errorf("fdroute: route %s: invalid caller %q", r.Action, r.Caller)
		}
		if !validCallees[r.Callee] {
			return fmt.Errorf("fdroute: route %s: invalid callee %q", r.Action, r.Callee)
		}
		segs, err := parseTemplate(r.Path)
		if err != nil {
			return fmt.Errorf("fdroute: route %s: %w", r.Action, err)
		}
		var names []string
		for _, s := range segs {
			if s.param {
				names = append(names, s.value)
			}
		}
		if !equalStrings(names, r.PathParams) {
			return fmt.Errorf("fdroute: route %s: pathParams %v do not match path %s (want %v)", r.Action, r.PathParams, r.Path, names)
		}
		key := r.Method + " " + normalise(segs)
		if other, dup := templates[key]; dup {
			return fmt.Errorf("fdroute: ambiguous routes %q and %q: both are %s", other, r.Action, key)
		}
		templates[key] = r.Action
		t.byAction[r.Action] = r

		root := t.trees[r.Method]
		if root == nil {
			root = &node{}
			t.trees[r.Method] = root
		}
		root.insert(segs, r)
	}
	return nil
}

type segment struct {
	value string // literal text, or the parameter name
	param bool
}

// parseTemplate splits "/a/{b}/c" into segments. Every segment must be non-empty and a
// parameter must fill its whole segment.
func parseTemplate(path string) ([]segment, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("path %q must start with /", path)
	}
	if path == "/" {
		return nil, nil
	}
	parts := strings.Split(path[1:], "/")
	segs := make([]segment, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		switch {
		case p == "":
			return nil, fmt.Errorf("path %q has an empty segment", path)
		case strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}"):
			name := p[1 : len(p)-1]
			if name == "" || strings.ContainsAny(name, "{}") {
				return nil, fmt.Errorf("path %q has an invalid parameter segment %q", path, p)
			}
			if seen[name] {
				return nil, fmt.Errorf("path %q repeats parameter %q", path, name)
			}
			seen[name] = true
			segs = append(segs, segment{value: name, param: true})
		case strings.ContainsAny(p, "{}"):
			return nil, fmt.Errorf("path %q: a parameter must fill its whole segment (%q)", path, p)
		default:
			segs = append(segs, segment{value: p})
		}
	}
	return segs, nil
}

// normalise renders a template with every parameter name erased, so /a/{x} and /a/{y}
// compare equal.
func normalise(segs []segment) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteByte('/')
		if s.param {
			b.WriteString("{}")
		} else {
			b.WriteString(s.value)
		}
	}
	if b.Len() == 0 {
		return "/"
	}
	return b.String()
}

// splitRequestPath splits an escaped request path into raw segments.
func splitRequestPath(path string) ([]string, bool) {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if path == "" || path == "/" {
		return nil, true
	}
	if !strings.HasPrefix(path, "/") {
		return nil, false
	}
	return strings.Split(path[1:], "/"), true
}

type node struct {
	literals map[string]*node
	param    *node
	route    *Route // set when a route's template ends here
}

func (n *node) insert(segs []segment, r *Route) {
	cur := n
	for _, s := range segs {
		if s.param {
			if cur.param == nil {
				cur.param = &node{}
			}
			cur = cur.param
			continue
		}
		if cur.literals == nil {
			cur.literals = make(map[string]*node)
		}
		next := cur.literals[s.value]
		if next == nil {
			next = &node{}
			cur.literals[s.value] = next
		}
		cur = next
	}
	cur.route = r
}

// match walks the trie depth-first, trying the literal child before the parameter child at
// every level, and returns the terminal node and the decoded parameter values in path order.
func (n *node) match(segs []string, vals []string) (*node, []string) {
	if len(segs) == 0 {
		if n.route != nil {
			return n, vals
		}
		return nil, nil
	}
	dec, err := url.PathUnescape(segs[0])
	if err != nil {
		return nil, nil
	}
	if child := n.literals[dec]; child != nil {
		if m, v := child.match(segs[1:], vals); m != nil {
			return m, v
		}
	}
	if n.param != nil && dec != "" {
		next := append(vals[:len(vals):len(vals)], dec)
		if m, v := n.param.match(segs[1:], next); m != nil {
			return m, v
		}
	}
	return nil, nil
}

func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
