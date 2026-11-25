package fdsig_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdsig"
	"github.com/ONDC-Official/automation-beckn-plugins/signer"
	"github.com/ONDC-Official/automation-beckn-plugins/signvalidator"
)

const (
	body       = `{"context":{"action":"search"}}`
	bodyDigest = "BLAKE-512=slhPbgolJr6Cchium7xxe8wsNedCyzj5RcEOKsrcmQq37b9/4pfbgA/D7ITIWuWkH2daOZKTZaHHaISorFWVPQ=="
	// BLAKE2b-512 of the empty string.
	emptyDigest = "BLAKE-512=eGoC90IBWQPGxv2FJVLScpEvR0DhWEdhiobiF/cfVBnSXhAxr+5YUxOJZESTTrBLkDpoWxRIt1XVb3Aa/pvizg=="
)

// testKeys returns a fixed Ed25519 key pair as the signer/signvalidator plugins take them:
// base64 seed and base64 raw public key.
func testKeys() (priv, pub string) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	k := ed25519.NewKeyFromSeed(seed)
	return base64.StdEncoding.EncodeToString(seed), base64.StdEncoding.EncodeToString(k.Public().(ed25519.PublicKey))
}

var becknHeaders = []string{"(created)", "(expires)", "digest"}

func TestSigningStringBecknVector(t *testing.T) {
	got, err := fdsig.SigningString(becknHeaders, fdsig.Inputs{Created: 1700000000, Expires: 1700003600, Body: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	// The format of signer.hash / signvalidator.hash.
	want := "(created): 1700000000\n(expires): 1700003600\ndigest: " + bodyDigest
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// Ed25519 is deterministic, so equal signatures under one key mean equal signing strings:
// this pins SigningString to the Beckn signer plugin's own hash(), byte for byte.
func TestSigningStringEqualsBecknSigner(t *testing.T) {
	priv, pub := testKeys()
	s, _, err := signer.New(context.Background(), &signer.Config{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, b := range [][]byte{[]byte(body), {}, []byte("plain text \u00e9")} {
		in := fdsig.Inputs{Created: now - 10, Expires: now + 3600, Body: b}
		ss, err := fdsig.SigningString(becknHeaders, in)
		if err != nil {
			t.Fatal(err)
		}
		ours, err := fdsig.Sign(ss, priv)
		if err != nil {
			t.Fatal(err)
		}
		theirs, err := s.Sign(context.Background(), b, priv, in.Created, in.Expires)
		if err != nil {
			t.Fatal(err)
		}
		if ours != theirs {
			t.Fatalf("signature differs from signer plugin for body %q", b)
		}

		// And the Beckn signvalidator accepts what we sign.
		v, _, _ := signvalidator.New(context.Background(), &signvalidator.Config{})
		auth := (&fdsig.Authorization{KeyID: "sub|k1|ed25519", Algorithm: "ed25519", Created: in.Created, Expires: in.Expires, Headers: becknHeaders, Signature: ours}).String()
		if err := v.Validate(context.Background(), b, auth, pub); err != nil {
			t.Fatalf("signvalidator rejected our signature: %v", err)
		}
	}
}

func TestDigest(t *testing.T) {
	if got := fdsig.Digest(nil); got != emptyDigest {
		t.Fatalf("Digest(nil) = %s", got)
	}
	if got := fdsig.Digest([]byte{}); got != emptyDigest {
		t.Fatalf("Digest(empty) = %s", got)
	}
	if got := fdsig.Digest([]byte(body)); got != bodyDigest {
		t.Fatalf("Digest(body) = %s", got)
	}
}

func TestSigningStringRequestTarget(t *testing.T) {
	headers := []string{"(request-target)", "(created)", "(expires)", "digest"}
	if !fdsig.RequiresRequestTarget(headers) || fdsig.RequiresRequestTarget(becknHeaders) {
		t.Fatal("RequiresRequestTarget wrong")
	}
	got, err := fdsig.SigningString(headers, fdsig.Inputs{
		Method: "GET", RequestTarget: "/deposits/D-1/events?limit=10", Created: 1, Expires: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "(request-target): get /deposits/D-1/events?limit=10\n(created): 1\n(expires): 2\ndigest: " + emptyDigest
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if _, err := fdsig.SigningString(headers, fdsig.Inputs{Method: "GET", Created: 1, Expires: 2}); err == nil {
		t.Fatal("want error for missing request target")
	}
}

func TestSigningStringOtherHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", " application/json ")
	h.Add("X-Multi", "a")
	h.Add("X-Multi", "b")
	got, err := fdsig.SigningString([]string{"(created)", "Content-Type", "x-multi"}, fdsig.Inputs{Created: 5, Header: h})
	if err != nil {
		t.Fatal(err)
	}
	want := "(created): 5\ncontent-type: application/json\nx-multi: a, b"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if h.Get("Content-Type") != " application/json " {
		t.Fatal("SigningString modified the request header")
	}
	if _, err := fdsig.SigningString([]string{"x-absent"}, fdsig.Inputs{Header: h}); err == nil {
		t.Fatal("want error for absent header")
	}
	if _, err := fdsig.SigningString([]string{"(method)"}, fdsig.Inputs{}); err == nil {
		t.Fatal("want error for unknown pseudo-header")
	}
	if _, err := fdsig.SigningString(nil, fdsig.Inputs{}); err == nil {
		t.Fatal("want error for no headers")
	}
}

func TestSignVerify(t *testing.T) {
	priv, pub := testKeys()
	ss := "(created): 1\n(expires): 2\ndigest: " + bodyDigest
	sig, err := fdsig.Sign(ss, priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := fdsig.Verify(ss, sig, pub); err != nil {
		t.Fatal(err)
	}
	if err := fdsig.Verify(ss+"x", sig, pub); !errors.Is(err, fdsig.ErrSignatureMismatch) {
		t.Fatalf("want mismatch, got %v", err)
	}

	// A 64-byte private key (seed||public) signs the same as its seed, as in the signer plugin.
	seed, _ := base64.StdEncoding.DecodeString(priv)
	full := base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(seed))
	if sig2, _ := fdsig.Sign(ss, full); sig2 != sig {
		t.Fatal("64-byte key signed differently")
	}

	if _, err := fdsig.Sign(ss, "not base64!"); err == nil {
		t.Fatal("want error for bad private key")
	}
	if _, err := fdsig.Sign(ss, base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("want error for short private key")
	}
	if err := fdsig.Verify(ss, sig, base64.StdEncoding.EncodeToString([]byte("short"))); err == nil || errors.Is(err, fdsig.ErrSignatureMismatch) {
		t.Fatalf("want key-length error, got %v", err)
	}
	if err := fdsig.Verify(ss, "%%%", pub); err == nil {
		t.Fatal("want error for bad signature encoding")
	}
}

func TestParseAuthorization(t *testing.T) {
	want := []string{"(created)", "(expires)", "digest"}
	tests := map[string]string{
		"beckn spelling":  `Signature keyId="bap.example.com|k1|ed25519",algorithm="ed25519",created="1700000000",expires="1700003600",headers="(created) (expires) digest",signature="c2ln"`,
		"fd spelling":     `Signature keyId="bap.example.com|k1|ed25519", algorithm="ed25519", created="1700000000", expires="1700003600", headers="(created)(expires)digest", signature="c2ln"`,
		"bare numbers":    `Signature keyId="bap.example.com|k1|ed25519",algorithm="ed25519",created=1700000000,expires=1700003600,headers="(created)(expires) digest",signature="c2ln"`,
		"order and case":  `signature  signature="c2ln" ,Headers="(Created) (Expires) Digest",expires="1700003600",created="1700000000",algorithm="ed25519",keyId="bap.example.com|k1|ed25519"`,
		"multi-line (FD)": "Signature keyId=\"bap.example.com|k1|ed25519\",\n algorithm=\"ed25519\",\n created=\"1700000000\", expires=\"1700003600\",\n headers=\"(created)(expires)digest\",\n signature=\"c2ln\"",
	}
	for name, h := range tests {
		t.Run(name, func(t *testing.T) {
			a, err := fdsig.ParseAuthorization(h)
			if err != nil {
				t.Fatal(err)
			}
			if a.SubscriberID != "bap.example.com" || a.UniqueKeyID != "k1" || a.KeyAlgorithm != "ed25519" || a.Algorithm != "ed25519" {
				t.Fatalf("keyId/algorithm parsed wrongly: %+v", a)
			}
			if a.Created != 1700000000 || a.Expires != 1700003600 || a.Signature != "c2ln" {
				t.Fatalf("values parsed wrongly: %+v", a)
			}
			if !reflect.DeepEqual(a.Headers, want) || a.HeadersDefaulted {
				t.Fatalf("headers = %v", a.Headers)
			}
		})
	}

	t.Run("request-target, run together", func(t *testing.T) {
		a, err := fdsig.ParseAuthorization(`Signature keyId="s|k|ed25519",created="1",expires="2",headers="(request-target)(created)(expires)digest content-type",signature="x"`)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a.Headers, []string{"(request-target)", "(created)", "(expires)", "digest", "content-type"}) {
			t.Fatalf("headers = %v", a.Headers)
		}
	})

	t.Run("headers absent defaults to ONDC list", func(t *testing.T) {
		a, err := fdsig.ParseAuthorization(`Signature keyId="s|k|ed25519",created="1",expires="2",signature="x"`)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a.Headers, want) || !a.HeadersDefaulted {
			t.Fatalf("headers = %v defaulted=%v", a.Headers, a.HeadersDefaulted)
		}
	})

	t.Run("round trip", func(t *testing.T) {
		in := &fdsig.Authorization{KeyID: "s|k|ed25519", Algorithm: "ed25519", Created: 1, Expires: 2, Headers: want, Signature: "abc="}
		a, err := fdsig.ParseAuthorization(in.String())
		if err != nil {
			t.Fatal(err)
		}
		if a.String() != in.String() {
			t.Fatalf("round trip: %s != %s", a.String(), in.String())
		}
	})

	bad := map[string]string{
		"empty":             ``,
		"bearer":            `Bearer abc`,
		"no space":          `SignaturekeyId="s|k|ed25519",signature="x"`,
		"missing keyId":     `Signature created="1",expires="2",signature="x"`,
		"two-part keyId":    `Signature keyId="s|k",created="1",expires="2",signature="x"`,
		"empty keyId part":  `Signature keyId="s||ed25519",created="1",expires="2",signature="x"`,
		"missing signature": `Signature keyId="s|k|ed25519",created="1",expires="2"`,
		"bad created":       `Signature keyId="s|k|ed25519",created="soon",expires="2",signature="x"`,
		"signed, missing":   `Signature keyId="s|k|ed25519",created="1",headers="(created) (expires) digest",signature="x"`,
		"unterminated":      `Signature keyId="s|k|ed25519,signature="x`,
		"duplicate":         `Signature keyId="s|k|ed25519",signature="x",signature="y"`,
		"no equals":         `Signature keyId="s|k|ed25519",garbage,signature="x"`,
		"open pseudo":       `Signature keyId="s|k|ed25519",created="1",expires="2",headers="(created",signature="x"`,
		"empty headers":     `Signature keyId="s|k|ed25519",created="1",expires="2",headers="",signature="x"`,
	}
	for name, h := range bad {
		t.Run("bad/"+name, func(t *testing.T) {
			if a, err := fdsig.ParseAuthorization(h); err == nil {
				t.Fatalf("want error, got %+v", a)
			}
		})
	}
}

func TestObjectObjectDiagnostic(t *testing.T) {
	priv, pub := testKeys()
	in := fdsig.Inputs{Created: 1, Expires: 2, Body: []byte(body)}

	// A JavaScript signer that digested the body object signed "[object Object]".
	broken := in
	broken.Body = []byte(fdsig.ObjectObjectLiteral)
	ss, _ := fdsig.SigningString(becknHeaders, broken)
	sig, _ := fdsig.Sign(ss, priv)

	real, _ := fdsig.SigningString(becknHeaders, in)
	if fdsig.Verify(real, sig, pub) == nil {
		t.Fatal("broken signature verified against the real body")
	}
	if !fdsig.ObjectObjectDiagnostic(becknHeaders, in, sig, pub) {
		t.Fatal("diagnostic did not recognise an [object Object] signature")
	}

	// A correct signature is not diagnosed as [object Object].
	good, _ := fdsig.Sign(real, priv)
	if fdsig.ObjectObjectDiagnostic(becknHeaders, in, good, pub) {
		t.Fatal("diagnostic fired for a correct signature")
	}
	// Without digest the body is not signed, so the diagnosis does not apply.
	if fdsig.ObjectObjectDiagnostic([]string{"(created)", "(expires)"}, in, sig, pub) {
		t.Fatal("diagnostic fired without digest")
	}
}
