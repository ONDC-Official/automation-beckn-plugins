package fdroute

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Canonical renders v as the route table's canonical JSON: every object's keys sorted
// (recursively), no whitespace, UTF-8 with non-ASCII left as-is — byte-for-byte what
// JavaScript's JSON.stringify produces for a key-sorted value.
//
// v is first marshalled and re-read as generic values (numbers kept literally), so structs
// are canonicalised by their json tags; pass generic values (as decoded from YAML/JSON) to
// hash a table exactly as written.
func Canonical(v any) ([]byte, error) {
	raw, err := encode(v)
	if err != nil {
		return nil, fmt.Errorf("fdroute: canonical: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, fmt.Errorf("fdroute: canonical: %w", err)
	}
	out, err := encode(generic)
	if err != nil {
		return nil, fmt.Errorf("fdroute: canonical: %w", err)
	}
	return unescapeLineSeparators(out), nil
}

// ComputeHash returns sha256(Canonical(routes)) as lower-case hex.
func ComputeHash(routes any) (string, error) {
	c, err := Canonical(routes)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:]), nil
}

// HashRoutes returns the tableHash of typed routes, rendered in the format's shape (nil
// slices as [], nil pointers as null).
func HashRoutes(routes []Route) (string, error) {
	return ComputeHash(genericRoutes(routes))
}

// Marshal renders t as a route table YAML file, setting t.TableHash from t.Routes.
func Marshal(t *Table) ([]byte, error) {
	h, err := HashRoutes(t.Routes)
	if err != nil {
		return nil, err
	}
	t.TableHash = h
	out := struct {
		FormatVersion int              `yaml:"formatVersion"`
		Domain        string           `yaml:"domain"`
		Version       string           `yaml:"version"`
		TableHash     string           `yaml:"tableHash"`
		Routes        []map[string]any `yaml:"routes"`
	}{t.FormatVersion, t.Domain, t.Version, t.TableHash, genericRoutes(t.Routes)}
	return yaml.Marshal(out)
}

func genericRoutes(routes []Route) []map[string]any {
	out := make([]map[string]any, len(routes))
	for i, r := range routes {
		out[i] = map[string]any{
			"action":       r.Action,
			"kind":         r.Kind,
			"method":       r.Method,
			"path":         r.Path,
			"pathParams":   nonNil(r.PathParams),
			"mode":         r.Mode,
			"caller":       r.Caller,
			"callee":       r.Callee,
			"callback":     r.Callback,
			"inReplyTo":    r.InReplyTo,
			"hasBody":      r.HasBody,
			"versionGuard": r.VersionGuard,
			"sla":          r.SLA,
			"requirements": nonNil(r.Requirements),
			"operationId":  r.OperationID,
		}
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// unescapeLineSeparators undoes encoding/json's unconditional escaping of U+2028 and U+2029,
// which JSON.stringify leaves as literal characters. It walks escape sequences, so an
// escaped backslash followed by "u2028" is left alone.
func unescapeLineSeparators(b []byte) []byte {
	if !bytes.Contains(b, []byte(`\u202`)) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' || i+1 >= len(b) {
			out = append(out, b[i])
			continue
		}
		if b[i+1] == 'u' && i+5 < len(b) && string(b[i+2:i+5]) == "202" && (b[i+5] == '8' || b[i+5] == '9') {
			if b[i+5] == '8' {
				out = append(out, "\u2028"...)
			} else {
				out = append(out, "\u2029"...)
			}
			i += 5
			continue
		}
		out = append(out, b[i], b[i+1])
		i++
	}
	return out
}
