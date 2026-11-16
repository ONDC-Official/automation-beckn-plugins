package fdgate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdsig"
)

// KeyLookup resolves a signer's Ed25519 signing public key (base64 of the raw 32 bytes).
type KeyLookup interface {
	LookupSigningKey(ctx context.Context, subscriberID, uniqueKeyID string) (string, error)
}

// ErrSignerNotFound is returned (wrapped) when the Registry has no such subscriber/key.
var ErrSignerNotFound = errors.New("signer not found in the registry")

// KeyCacheTTL is how long a looked-up key is reused. Failed lookups are not cached.
const KeyCacheTTL = 10 * time.Minute

// RegistryLookup looks keys up in an ONDC Registry: POST <URL>lookup with
// {"subscriber_id","ukId"}, answered by an array of {"signing_public_key",...}, as the
// workbench key manager does. When the workbench's own keys are set the lookup is signed.
type RegistryLookup struct {
	URL               string // registry base URL; "lookup" is appended
	Client            *http.Client
	SubscriberID      string
	UniqueKeyID       string
	SigningPrivateKey string
	Now               func() time.Time
}

// LookupSigningKey implements KeyLookup.
func (l *RegistryLookup) LookupSigningKey(ctx context.Context, subscriberID, uniqueKeyID string) (string, error) {
	reqBody, err := json.Marshal(map[string]string{"subscriber_id": subscriberID, "ukId": uniqueKeyID})
	if err != nil {
		return "", err
	}
	url := l.URL
	if !strings.HasSuffix(url, "/") {
		url += "/"
	}
	url += "lookup"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("registry lookup: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if l.SubscriberID != "" && l.UniqueKeyID != "" && l.SigningPrivateKey != "" {
		auth, err := l.authorization(reqBody)
		if err != nil {
			return "", fmt.Errorf("registry lookup: sign: %w", err)
		}
		req.Header.Set("Authorization", auth)
	}
	client := l.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("registry lookup: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("registry lookup: read: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("registry lookup: %w (HTTP 404)", ErrSignerNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry lookup: HTTP %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var entries []struct {
		SigningPublicKey string `json:"signing_public_key"`
		UkID             string `json:"ukId"`
		UniqueKeyID      string `json:"unique_key_id"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return "", fmt.Errorf("registry lookup: decode: %w", err)
	}
	// Prefer the entry for the requested key; a registry that does not echo the key id
	// answers for the requested one.
	for _, e := range entries {
		if e.SigningPublicKey != "" && (e.UkID == uniqueKeyID || e.UniqueKeyID == uniqueKeyID) {
			return e.SigningPublicKey, nil
		}
	}
	for _, e := range entries {
		if e.SigningPublicKey != "" && e.UkID == "" && e.UniqueKeyID == "" {
			return e.SigningPublicKey, nil
		}
	}
	return "", fmt.Errorf("registry lookup: %w", ErrSignerNotFound)
}

// authorization signs a lookup body with the Beckn scheme, as the key manager's ONDC crypto
// SDK call does.
func (l *RegistryLookup) authorization(body []byte) (string, error) {
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	created := now().Unix()
	expires := created + 300
	ss, err := fdsig.SigningString(fdsig.DefaultHeaders, fdsig.Inputs{Created: created, Expires: expires, Body: body})
	if err != nil {
		return "", err
	}
	sig, err := fdsig.Sign(ss, l.SigningPrivateKey)
	if err != nil {
		return "", err
	}
	return (&fdsig.Authorization{
		KeyID:     l.SubscriberID + "|" + l.UniqueKeyID + "|ed25519",
		Algorithm: "ed25519", Created: created, Expires: expires,
		Headers: fdsig.DefaultHeaders, Signature: sig,
	}).String(), nil
}

// CachedLookup caches a KeyLookup's answers for TTL. Errors are not cached.
type CachedLookup struct {
	Next KeyLookup
	TTL  time.Duration
	Now  func() time.Time

	mu      sync.Mutex
	entries map[string]cachedKey
}

type cachedKey struct {
	key     string
	expires time.Time
}

// NewCachedLookup wraps next with a cache of the given TTL.
func NewCachedLookup(next KeyLookup, ttl time.Duration) *CachedLookup {
	return &CachedLookup{Next: next, TTL: ttl, entries: map[string]cachedKey{}}
}

// LookupSigningKey implements KeyLookup.
func (c *CachedLookup) LookupSigningKey(ctx context.Context, subscriberID, uniqueKeyID string) (string, error) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	id := subscriberID + "|" + uniqueKeyID
	c.mu.Lock()
	if e, ok := c.entries[id]; ok && now().Before(e.expires) {
		c.mu.Unlock()
		return e.key, nil
	}
	c.mu.Unlock()

	key, err := c.Next.LookupSigningKey(ctx, subscriberID, uniqueKeyID)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]cachedKey{}
	}
	c.entries[id] = cachedKey{key: key, expires: now().Add(c.TTL)}
	c.mu.Unlock()
	return key, nil
}

// signedHeaders are the signed-header entries fd-gate requires besides (request-target),
// so the body and the validity window are covered.
var signedHeaders = []string{fdsig.PseudoCreated, fdsig.PseudoExpires, fdsig.HeaderDigest}

// verifySignature checks an inbound FD request's Authorization header (D-1, decision 16).
// requestURI is the path and query exactly as received; body is the raw body (empty for GET).
func (g *Gate) verifySignature(ctx context.Context, r *http.Request, requestURI string, body []byte) (*fdsig.Authorization, *gateError) {
	raw := r.Header.Get("Authorization")
	if strings.TrimSpace(raw) == "" {
		return nil, reject(CodeSignatureMissing, "", "the request has no Authorization header")
	}
	auth, err := fdsig.ParseAuthorization(raw)
	if err != nil {
		return nil, reject(CodeSignatureInvalid, "", "the Authorization header cannot be parsed: %v", err)
	}
	if !fdsig.RequiresRequestTarget(auth.Headers) {
		return nil, reject(CodeRequestTargetNotSig, "",
			"the signature's headers (%s) do not include (request-target); FD signatures must cover the method and path",
			strings.Join(auth.Headers, " "))
	}
	for _, h := range signedHeaders {
		if !containsFold(auth.Headers, h) {
			return nil, reject(CodeSignatureInvalid, "", "the signature's headers (%s) do not include %s",
				strings.Join(auth.Headers, " "), h)
		}
	}
	if !strings.EqualFold(auth.KeyAlgorithm, "ed25519") || (auth.Algorithm != "" && !strings.EqualFold(auth.Algorithm, "ed25519")) {
		return nil, reject(CodeSignatureInvalid, "", "unsupported signature algorithm (keyId %q, algorithm %q); ed25519 is required",
			auth.KeyID, auth.Algorithm)
	}

	now := g.now()
	skew := g.cfg.ClockSkew
	created, expires := time.Unix(auth.Created, 0), time.Unix(auth.Expires, 0)
	switch {
	case auth.Expires < auth.Created:
		return nil, reject(CodeSignatureExpired, "", "expires (%d) is before created (%d)", auth.Expires, auth.Created)
	case created.After(now.Add(skew)):
		return nil, reject(CodeSignatureExpired, "", "created (%d) is in the future (now %d, allowed skew %s)", auth.Created, now.Unix(), skew)
	case now.After(expires.Add(skew)):
		return nil, reject(CodeSignatureExpired, "", "the signature expired at %d (now %d, allowed skew %s)", auth.Expires, now.Unix(), skew)
	}

	pub, err := g.keys.LookupSigningKey(ctx, auth.SubscriberID, auth.UniqueKeyID)
	if err != nil {
		return nil, reject(CodeSignerUnknown, "", "no signing key for %s|%s: %v", auth.SubscriberID, auth.UniqueKeyID, err)
	}

	in := fdsig.Inputs{
		Method: r.Method, RequestTarget: requestURI,
		Created: auth.Created, Expires: auth.Expires, Body: body, Header: r.Header,
	}
	ss, err := fdsig.SigningString(auth.Headers, in)
	if err != nil {
		return nil, reject(CodeSignatureInvalid, "", "cannot rebuild the signing string: %v", err)
	}
	if err := fdsig.Verify(ss, auth.Signature, pub); err != nil {
		if fdsig.ObjectObjectDiagnostic(auth.Headers, in, auth.Signature, pub) {
			return nil, reject(CodeSignatureObjObj, "",
				`the signature verifies over the literal body "[object Object]": the signer digested a JavaScript object instead of the JSON text it sent`)
		}
		return nil, reject(CodeSignatureInvalid, "", "the signature does not verify against %s|%s over %q: %v",
			auth.SubscriberID, auth.UniqueKeyID, strings.ToLower(r.Method)+" "+requestURI, err)
	}
	return auth, nil
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), s) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
