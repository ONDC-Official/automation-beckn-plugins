package fdgate

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/pkg/fdwire"
)

// Mode is the workbench side fd-gate serves.
type Mode string

// Modes.
const (
	ModeNP         Mode = "np"         // in front of a participant under test
	ModeMock       Mode = "mock"       // in front of the workbench's mock participant
	ModeStandalone Mode = "standalone" // resolves the route only
)

// Signature policies (config key signature).
const (
	SignatureRequired = "required"
	SignatureOff      = "off"
)

// Defaults.
const (
	DefaultClockSkew    = 5 * time.Second
	DefaultReplayTTL    = 24 * time.Hour
	DefaultObjectTTL    = 7 * 24 * time.Hour
	DefaultMaxBodyBytes = 8 << 20 // 8 MiB
	// DefaultRegistryURL is the key manager's default, used when neither config nor env
	// names a registry.
	DefaultRegistryURL = "https://preprod.registry.ondc.org/v2.0/"
)

// Environment variables fd-gate reads. The FD_* ones and MOCK_SERVER_URL override the
// config when set, because one generated adapter.yaml serves every environment.
const (
	EnvRequireHTTPS    = "FD_REQUIRE_HTTPS"
	EnvRegistryURL     = "FD_REGISTRY_URL"
	EnvSignature       = "FD_SIGNATURE"
	EnvMockServerURL   = "MOCK_SERVER_URL"
	EnvInHouseRegistry = "IN_HOUSE_REGISTRY"
	EnvRegistryURLAlt  = "REGISTRY_URL"
	EnvInternalKey     = "PWB_INTERNAL_KEY"
	EnvSubscriberID    = "SUBSCRIBER_ID"
	EnvUniqueKeyID     = "UNIQUE_KEY_ID"
	EnvSigningPrivate  = "SIGNING_PRIVATE"
	EnvRedisUsername   = "REDIS_USERNAME"
	EnvRedisPassword   = "REDIS_PASSWORD"
)

// Config is fd-gate's configuration.
type Config struct {
	RouteTable     string        // routeTable (required): path to fd_routes.yaml
	Mode           Mode          // mode (required): np | mock | standalone
	PathPrefix     string        // pathPrefix: the module's URL prefix, removed before matching
	PassThrough    bool          // passThrough: parsed for compatibility, ignored (M2 always forwards)
	Addr           string        // addr: Redis address (required in np and mock modes)
	MockServiceURL string        // mockServiceURL, overridden by env MOCK_SERVER_URL
	RegistryURL    string        // registryURL, overridden by env FD_REGISTRY_URL
	Signature      string        // signature: required | off (np mode), overridden by env FD_SIGNATURE
	RequireHTTPS   bool          // requireHTTPS (default true), overridden by env FD_REQUIRE_HTTPS
	ClockSkew      time.Duration // clockSkew (default 5s)
	ReplayTTL      time.Duration // replayTTL (default 24h)
	ObjectTTL      time.Duration // objectTTL (default 168h): lifetime of the FD_OBJ index entries
	MaxBodyBytes   int64         // maxBodyBytes (default 8 MiB)

	// From the environment only.
	InternalKey       string // PWB_INTERNAL_KEY: the playground's key on mock calls ("" = not checked)
	SubscriberID      string // SUBSCRIBER_ID: the workbench's identity, signs registry lookups
	UniqueKeyID       string // UNIQUE_KEY_ID
	SigningPrivateKey string // SIGNING_PRIVATE
	RedisUsername     string // REDIS_USERNAME
	RedisPassword     string // REDIS_PASSWORD
}

// ParseConfig reads fd-gate's plugin config map (values are strings, as adapter.yaml gives
// them) and the environment.
func ParseConfig(cfg map[string]string) (*Config, error) {
	return parseConfig(cfg, os.Getenv)
}

func parseConfig(cfg map[string]string, getenv func(string) string) (*Config, error) {
	get := func(k string) string { return strings.TrimSpace(cfg[k]) }
	env := func(k string) string { return strings.TrimSpace(getenv(k)) }

	c := &Config{
		RouteTable:   get("routeTable"),
		RequireHTTPS: true,
		ClockSkew:    DefaultClockSkew,
		ReplayTTL:    DefaultReplayTTL,
		ObjectTTL:    DefaultObjectTTL,
		MaxBodyBytes: DefaultMaxBodyBytes,
	}
	if c.RouteTable == "" {
		return nil, errors.New("fd-gate: routeTable is required")
	}
	switch m := Mode(get("mode")); m {
	case ModeNP, ModeMock, ModeStandalone:
		c.Mode = m
	case "":
		return nil, errors.New("fd-gate: mode is required (np|mock|standalone)")
	default:
		return nil, fmt.Errorf("fd-gate: invalid mode %q (np|mock|standalone)", m)
	}
	if raw := get("passThrough"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("fd-gate: invalid passThrough %q", raw)
		}
		c.PassThrough = v
	}
	c.PathPrefix = strings.TrimRight(get("pathPrefix"), "/")
	if c.PathPrefix != "" && !strings.HasPrefix(c.PathPrefix, "/") {
		return nil, fmt.Errorf("fd-gate: pathPrefix %q must start with /", c.PathPrefix)
	}

	c.Addr = get("addr")
	if c.Addr == "" && c.Mode != ModeStandalone {
		return nil, fmt.Errorf("fd-gate: addr (Redis) is required in %s mode", c.Mode)
	}

	c.MockServiceURL = firstNonEmpty(env(EnvMockServerURL), get("mockServiceURL"))
	c.RegistryURL = firstNonEmpty(env(EnvRegistryURL), get("registryURL"), env(EnvInHouseRegistry), env(EnvRegistryURLAlt), DefaultRegistryURL)

	c.Signature = firstNonEmpty(env(EnvSignature), get("signature"), SignatureRequired)
	switch c.Signature = strings.ToLower(c.Signature); c.Signature {
	case SignatureRequired, SignatureOff:
	default:
		return nil, fmt.Errorf("fd-gate: invalid signature %q (required|off)", c.Signature)
	}

	if raw := firstNonEmpty(env(EnvRequireHTTPS), get("requireHTTPS")); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("fd-gate: invalid requireHTTPS %q", raw)
		}
		c.RequireHTTPS = v
	}

	for _, d := range []struct {
		key string
		dst *time.Duration
	}{{"clockSkew", &c.ClockSkew}, {"replayTTL", &c.ReplayTTL}, {"objectTTL", &c.ObjectTTL}} {
		raw := get(d.key)
		if raw == "" {
			continue
		}
		v, err := fdwire.ParseDuration(raw)
		if err != nil || v < 0 {
			return nil, fmt.Errorf("fd-gate: invalid %s %q", d.key, raw)
		}
		*d.dst = v
	}
	if c.ReplayTTL == 0 || c.ObjectTTL == 0 {
		return nil, errors.New("fd-gate: replayTTL and objectTTL must be positive")
	}

	if raw := get("maxBodyBytes"); raw != "" {
		v, err := parseBytes(raw)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("fd-gate: invalid maxBodyBytes %q", raw)
		}
		c.MaxBodyBytes = v
	}

	c.InternalKey = getenv(EnvInternalKey)
	c.SubscriberID = env(EnvSubscriberID)
	c.UniqueKeyID = env(EnvUniqueKeyID)
	c.SigningPrivateKey = env(EnvSigningPrivate)
	c.RedisUsername = getenv(EnvRedisUsername)
	c.RedisPassword = getenv(EnvRedisPassword)
	return c, nil
}

// parseBytes reads a byte count: a plain integer, or one with a KiB/MiB/KB/MB suffix.
func parseBytes(s string) (int64, error) {
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"KB", 1000}, {"MB", 1000 * 1000}} {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
