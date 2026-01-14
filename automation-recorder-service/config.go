package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
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
	// Load .env file if it exists
	if err := godotenv.Load(); err != nil {
		// It's okay if .env file doesn't exist, we'll use OS environment variables
		fmt.Printf("[CONFIG] Warning: .env file not found, using OS environment variables only\n")
	} else {
		fmt.Printf("[CONFIG] Successfully loaded .env file\n")
	}

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

	fmt.Printf("[CONFIG] GRPC Listen Address: %s\n", listenAddr)
	fmt.Printf("[CONFIG] HTTP Listen Address: %s\n", httpListenAddr)
	fmt.Printf("[CONFIG] Redis Address: %s\n", redisAddr)

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

	fmt.Printf("[CONFIG] Environment: %s\n", cfg.Env)
	fmt.Printf("[CONFIG] Skip Cache Update: %v\n", cfg.SkipCacheUpdate)
	fmt.Printf("[CONFIG] Skip NO Push: %v\n", cfg.SkipNOPush)
	fmt.Printf("[CONFIG] Skip DB Save: %v\n", cfg.SkipDBSave)
	fmt.Printf("[CONFIG] Async Queue Size: %d\n", cfg.AsyncQueueSize)
	fmt.Printf("[CONFIG] Async Workers: %d\n", cfg.AsyncWorkerCount)
	fmt.Printf("[CONFIG] Drop On Queue Full: %v\n", cfg.DropOnQueueFull)
	fmt.Printf("[CONFIG] API TTL Default: %d seconds\n", cfg.APITTLSecondsDefault)
	fmt.Printf("[CONFIG] Cache TTL Default: %d seconds\n", cfg.CacheTTLSecondsDefault)
	fmt.Printf("[CONFIG] Network Observability URL: %s\n", cfg.NOURL)
	fmt.Printf("[CONFIG] Database Base URL: %s\n", cfg.DBBaseURL)
	fmt.Printf("[CONFIG] Configuration loaded successfully\n")
	// Matches TS: POST `${DATA_BASE_URL}/api/sessions/payload`
	cfg.DBPayloadPath = "/api/sessions/payload"

	return cfg, nil
}

func newRedisClient(addr string) *redis.Client {
	password := os.Getenv("REDIS_PASSWORD")
	username := os.Getenv("REDIS_USERNAME")
	fmt.Println("Connecting to Redis at", addr)
	if username != "" {
		return redis.NewClient(&redis.Options{Addr: addr, Username: username, Password: password, DB: 0})
	}
	return redis.NewClient(&redis.Options{Addr: addr, Password: password, DB: 0})
}
