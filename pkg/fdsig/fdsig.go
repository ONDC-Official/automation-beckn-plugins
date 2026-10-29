// Package fdsig implements the ONDC HTTP signature as FD uses it: the Beckn
// "(created) (expires) digest" scheme, generalised to any signed header list so that a
// "(request-target)" line can cover REST paths and bodiless GETs.
//
// Key formats match the existing signer/ and signvalidator/ plugins: the private key is
// base64 (standard encoding) of at least a 32-byte Ed25519 seed (only the first 32 bytes are
// used), the public key is base64 of the raw 32-byte Ed25519 public key.
package fdsig

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// Pseudo-header names.
const (
	PseudoRequestTarget = "(request-target)"
	PseudoCreated       = "(created)"
	PseudoExpires       = "(expires)"
	HeaderDigest        = "digest"
)

// ObjectObjectLiteral is what a JavaScript signer hashes when it passes a body object, not
// its JSON text, to the digest (String({}) === "[object Object]").
const ObjectObjectLiteral = "[object Object]"

// DefaultHeaders is the ONDC signed-header list, used when an Authorization header has no
// headers parameter (the Beckn signvalidator assumes this list unconditionally).
var DefaultHeaders = []string{PseudoCreated, PseudoExpires, HeaderDigest}

// ErrSignatureMismatch is returned by Verify when the signature does not verify.
var ErrSignatureMismatch = errors.New("fdsig: signature verification failed")

// Authorization is a parsed `Authorization: Signature ...` header.
type Authorization struct {
	KeyID        string // as sent: "subscriber_id|unique_key_id|algorithm"
	SubscriberID string
	UniqueKeyID  string
	KeyAlgorithm string // third part of keyId
	Algorithm    string // the algorithm parameter
	Created      int64
	Expires      int64
	Headers      []string // lower-case, in order
	// HeadersDefaulted is true when the header had no headers parameter and Headers was set
	// to DefaultHeaders.
	HeadersDefaulted bool
	Signature        string // base64, as sent
}

// ParseAuthorization parses an Authorization header of the form
//
//	Signature keyId="sub|ukid|ed25519",algorithm="ed25519",created="1",expires="2",headers="(created) (expires) digest",signature="..."
//
// Parameter order is free, whitespace after commas is ignored, values may be quoted or bare,
// and headers may be written with or without spaces between names ("(created)(expires)digest").
func ParseAuthorization(header string) (*Authorization, error) {
	s := strings.TrimSpace(header)
	if len(s) < len("Signature") || !strings.EqualFold(s[:len("Signature")], "Signature") {
		return nil, errors.New("fdsig: authorization scheme is not Signature")
	}
	s = s[len("Signature"):]
	if s != "" && !isSpace(s[0]) {
		return nil, errors.New("fdsig: authorization scheme is not Signature")
	}
	params, err := parseParams(s)
	if err != nil {
		return nil, err
	}

	a := &Authorization{
		KeyID:     params["keyid"],
		Algorithm: params["algorithm"],
		Signature: params["signature"],
	}
	if a.KeyID == "" {
		return nil, errors.New("fdsig: keyId is missing")
	}
	parts := strings.Split(a.KeyID, "|")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, fmt.Errorf("fdsig: keyId %q is not subscriber_id|unique_key_id|algorithm", a.KeyID)
	}
	a.SubscriberID, a.UniqueKeyID, a.KeyAlgorithm = parts[0], parts[1], parts[2]
	if a.Signature == "" {
		return nil, errors.New("fdsig: signature is missing")
	}

	if raw, ok := params["headers"]; ok {
		if a.Headers, err = parseHeaderList(raw); err != nil {
			return nil, err
		}
	} else {
		a.Headers = append([]string(nil), DefaultHeaders...)
		a.HeadersDefaulted = true
	}

	for _, f := range []struct {
		name   string
		pseudo string
		dst    *int64
	}{{"created", PseudoCreated, &a.Created}, {"expires", PseudoExpires, &a.Expires}} {
		raw, ok := params[f.name]
		if !ok {
			if contains(a.Headers, f.pseudo) {
				return nil, fmt.Errorf("fdsig: %s is signed but missing", f.name)
			}
			continue
		}
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("fdsig: invalid %s %q", f.name, raw)
		}
		*f.dst = v
	}
	return a, nil
}

// String renders the header value in the ONDC form, with headers space-separated.
func (a *Authorization) String() string {
	return fmt.Sprintf(`Signature keyId="%s",algorithm="%s",created="%d",expires="%d",headers="%s",signature="%s"`,
		a.KeyID, a.Algorithm, a.Created, a.Expires, strings.Join(a.Headers, " "), a.Signature)
}

// parseParams parses `name=value` pairs separated by commas. Names are lower-cased; quoted
// values may contain commas and backslash escapes.
func parseParams(s string) (map[string]string, error) {
	params := map[string]string{}
	i, n := 0, len(s)
	skip := func() {
		for i < n && isSpace(s[i]) {
			i++
		}
	}
	for {
		skip()
		if i >= n {
			return params, nil
		}
		start := i
		for i < n && s[i] != '=' && s[i] != ',' {
			i++
		}
		name := strings.ToLower(strings.TrimSpace(s[start:i]))
		if i >= n || s[i] != '=' || name == "" {
			return nil, fmt.Errorf("fdsig: malformed authorization parameter near %q", s[start:])
		}
		i++ // '='
		skip()
		var val string
		if i < n && s[i] == '"' {
			i++
			var b strings.Builder
			closed := false
			for i < n {
				c := s[i]
				if c == '\\' && i+1 < n {
					b.WriteByte(s[i+1])
					i += 2
					continue
				}
				i++
				if c == '"' {
					closed = true
					break
				}
				b.WriteByte(c)
			}
			if !closed {
				return nil, fmt.Errorf("fdsig: unterminated quoted value for %s", name)
			}
			val = b.String()
		} else {
			start := i
			for i < n && s[i] != ',' {
				i++
			}
			val = strings.TrimSpace(s[start:i])
		}
		if _, dup := params[name]; dup {
			return nil, fmt.Errorf("fdsig: duplicate authorization parameter %s", name)
		}
		params[name] = val
		skip()
		if i >= n {
			return params, nil
		}
		if s[i] != ',' {
			return nil, fmt.Errorf("fdsig: expected ',' after %s", name)
		}
		i++
	}
}

// parseHeaderList splits a headers parameter. Names are separated by whitespace; a
// parenthesised pseudo-header is a token of its own, so "(created)(expires)digest" splits
// the same as "(created) (expires) digest".
func parseHeaderList(raw string) ([]string, error) {
	var out []string
	i, n := 0, len(raw)
	for i < n {
		switch c := raw[i]; {
		case isSpace(c):
			i++
		case c == '(':
			end := strings.IndexByte(raw[i:], ')')
			if end < 0 {
				return nil, fmt.Errorf("fdsig: unterminated pseudo-header in headers %q", raw)
			}
			out = append(out, strings.ToLower(raw[i:i+end+1]))
			i += end + 1
		default:
			start := i
			for i < n && !isSpace(raw[i]) && raw[i] != '(' {
				i++
			}
			out = append(out, strings.ToLower(raw[start:i]))
		}
	}
	if len(out) == 0 {
		return nil, errors.New("fdsig: headers parameter is empty")
	}
	return out, nil
}

// Inputs are the request facts a signing string is built from.
type Inputs struct {
	Method        string // HTTP method, any case
	RequestTarget string // path and query as sent, e.g. "/deposits/D-1/events?limit=10"
	Created       int64
	Expires       int64
	Body          []byte // raw body; nil or empty for a bodiless GET
	Header        http.Header
}

// SigningString builds the signing string for the listed headers, one line per header joined
// by "\n". For headers ["(created)","(expires)","digest"] it is byte-for-byte the Beckn
// signer's string.
func SigningString(headers []string, in Inputs) (string, error) {
	if len(headers) == 0 {
		return "", errors.New("fdsig: no headers to sign")
	}
	lines := make([]string, 0, len(headers))
	for _, h := range headers {
		name := strings.ToLower(strings.TrimSpace(h))
		switch {
		case name == PseudoRequestTarget:
			if in.Method == "" || in.RequestTarget == "" {
				return "", errors.New("fdsig: (request-target) needs a method and a request target")
			}
			lines = append(lines, fmt.Sprintf("%s: %s %s", PseudoRequestTarget, strings.ToLower(in.Method), in.RequestTarget))
		case name == PseudoCreated:
			lines = append(lines, fmt.Sprintf("%s: %d", PseudoCreated, in.Created))
		case name == PseudoExpires:
			lines = append(lines, fmt.Sprintf("%s: %d", PseudoExpires, in.Expires))
		case name == HeaderDigest:
			lines = append(lines, HeaderDigest+": "+Digest(in.Body))
		case strings.HasPrefix(name, "("):
			return "", fmt.Errorf("fdsig: unknown pseudo-header %s", name)
		case name == "":
			return "", errors.New("fdsig: empty header name")
		default:
			vals := in.Header.Values(name) // the header's own slice: do not modify
			if len(vals) == 0 {
				return "", fmt.Errorf("fdsig: signed header %s is not present", name)
			}
			trimmed := make([]string, len(vals))
			for i, v := range vals {
				trimmed[i] = strings.TrimSpace(v)
			}
			lines = append(lines, name+": "+strings.Join(trimmed, ", "))
		}
	}
	return strings.Join(lines, "\n"), nil
}

// Digest returns "BLAKE-512=<base64 of BLAKE2b-512(body)>". An empty body is allowed.
func Digest(body []byte) string {
	sum := blake2b.Sum512(body)
	return "BLAKE-512=" + base64.StdEncoding.EncodeToString(sum[:])
}

// RequiresRequestTarget reports whether headers includes (request-target).
func RequiresRequestTarget(headers []string) bool {
	for _, h := range headers {
		if strings.EqualFold(strings.TrimSpace(h), PseudoRequestTarget) {
			return true
		}
	}
	return false
}

// Sign signs signingString with an Ed25519 key given as base64 of its seed (at least 32
// bytes; the first 32 are used, as the signer plugin does) and returns the base64 signature.
func Sign(signingString string, privateKeyBase64 string) (string, error) {
	seed, err := base64.StdEncoding.DecodeString(privateKeyBase64)
	if err != nil {
		return "", fmt.Errorf("fdsig: decode private key: %w", err)
	}
	if len(seed) < ed25519.SeedSize {
		return "", fmt.Errorf("fdsig: private key is %d bytes, want at least %d", len(seed), ed25519.SeedSize)
	}
	key := ed25519.NewKeyFromSeed(seed[:ed25519.SeedSize])
	return base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte(signingString))), nil
}

// Verify checks a base64 Ed25519 signature over signingString against a base64 raw 32-byte
// public key. It returns ErrSignatureMismatch when the signature does not verify.
func Verify(signingString, signatureBase64, publicKeyBase64 string) error {
	sig, err := base64.StdEncoding.DecodeString(signatureBase64)
	if err != nil {
		return fmt.Errorf("fdsig: decode signature: %w", err)
	}
	pub, err := base64.StdEncoding.DecodeString(publicKeyBase64)
	if err != nil {
		return fmt.Errorf("fdsig: decode public key: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("fdsig: invalid public key length: %d", len(pub))
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), []byte(signingString), sig) {
		return ErrSignatureMismatch
	}
	return nil
}

// ObjectObjectDiagnostic reports whether the signature verifies when the body is replaced by
// the literal string "[object Object]" — the signature a JavaScript signer produces when it
// digests a body object instead of its JSON text. It is a diagnosis for a signature that
// failed against the real body, never a reason to accept one; it is false when digest is not
// among the signed headers.
func ObjectObjectDiagnostic(headers []string, in Inputs, signatureBase64, publicKeyBase64 string) bool {
	if !contains(lowerAll(headers), HeaderDigest) {
		return false
	}
	in.Body = []byte(ObjectObjectLiteral)
	ss, err := SigningString(headers, in)
	if err != nil {
		return false
	}
	return Verify(ss, signatureBase64, publicKeyBase64) == nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func lowerAll(list []string) []string {
	out := make([]string, len(list))
	for i, v := range list {
		out[i] = strings.ToLower(strings.TrimSpace(v))
	}
	return out
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }
