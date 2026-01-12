package main

import (
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type config struct {
	ListenAddr     string
	HTTPListenAddr string
	RedisAddr      string

	SkipCacheUpdate bool
	SkipNOPush      bool
	SkipDBSave      bool

	AsyncQueueSize   int
	AsyncWorkerCount int
	DropOnQueueFull  bool

	Env string

	APITTLSecondsDefault   int64
	CacheTTLSecondsDefault int64

	NOURL       string
	NOToken     string
	NOTimeout   time.Duration
	NOEnabledIn map[string]bool

	DBBaseURL     string
	DBAPIKey      string
	DBTimeout     time.Duration
	DBEnabledIn   map[string]bool
	DBSessionPath string
	DBPayloadPath string
}

func loadConfig() (config, error) {
	listenAddr := strings.TrimSpace(os.Getenv("RECORDER_LISTEN_ADDR"))
	if listenAddr == "" {
		listenAddr = ":8089"
	}

	httpListenAddr := strings.TrimSpace(os.Getenv("RECORDER_HTTP_LISTEN_ADDR"))
	if httpListenAddr == "" {
		httpListenAddr = ":8090"
	}

	redisAddr := strings.TrimSpace(os.Getenv("REDIS_ADDR"))
	if redisAddr == "" {
		redisAddr = strings.TrimSpace(os.Getenv("REDIS_HOST"))
	}
	if redisAddr == "" {
		redisAddr = "127.0.0.1:6379"
	}

	cfg := config{ListenAddr: listenAddr, HTTPListenAddr: httpListenAddr, RedisAddr: redisAddr}

	cfg.SkipCacheUpdate = envBool("RECORDER_SKIP_CACHE_UPDATE", false)
	cfg.SkipNOPush = envBool("RECORDER_SKIP_NO_PUSH", false)
	cfg.SkipDBSave = envBool("RECORDER_SKIP_DB_SAVE", false)

	cfg.AsyncQueueSize = envInt("RECORDER_ASYNC_QUEUE_SIZE", 1000)
	cfg.AsyncWorkerCount = envInt("RECORDER_ASYNC_WORKERS", 2)
	if cfg.AsyncWorkerCount < 1 {
		cfg.AsyncWorkerCount = 1
	}
	cfg.DropOnQueueFull = envBool("RECORDER_ASYNC_DROP_ON_FULL", true)

	cfg.Env = strings.ToLower(strings.TrimSpace(os.Getenv("RECORDER_ENV")))
	if cfg.Env == "" {
		cfg.Env = "dev"
	}

	cfg.APITTLSecondsDefault = int64(envInt("RECORDER_API_TTL_SECONDS_DEFAULT", 30000))
	if cfg.APITTLSecondsDefault < 0 {
		cfg.APITTLSecondsDefault = 0
	}
	cfg.CacheTTLSecondsDefault = int64(envInt("RECORDER_CACHE_TTL_SECONDS_DEFAULT", 0))
	if cfg.CacheTTLSecondsDefault < 0 {
		cfg.CacheTTLSecondsDefault = 0
	}

	cfg.NOURL = strings.TrimSpace(os.Getenv("RECORDER_NO_URL"))
	cfg.NOToken = strings.TrimSpace(os.Getenv("RECORDER_NO_BEARER_TOKEN"))
	cfg.NOTimeout = time.Duration(envInt("RECORDER_NO_TIMEOUT_MS", 5000)) * time.Millisecond
	cfg.NOEnabledIn = parseEnvSet(os.Getenv("RECORDER_NO_ENABLED_ENVS"))

	cfg.DBBaseURL = strings.TrimSpace(os.Getenv("RECORDER_DB_BASE_URL"))
	cfg.DBAPIKey = strings.TrimSpace(os.Getenv("RECORDER_DB_API_KEY"))
	cfg.DBTimeout = time.Duration(envInt("RECORDER_DB_TIMEOUT_MS", 5000)) * time.Millisecond
	cfg.DBEnabledIn = parseEnvSet(os.Getenv("RECORDER_DB_ENABLED_ENVS"))
	cfg.DBSessionPath = "/api/sessions"
	// Matches TS: POST `${DATA_BASE_URL}/api/sessions/payload`
	cfg.DBPayloadPath = "/api/sessions/payload"

	return cfg, nil
}

func newRedisClient(addr string) *redis.Client {
	password := os.Getenv("REDIS_PASSWORD")
	return redis.NewClient(&redis.Options{Addr: addr, Password: password, DB: 0})
}
