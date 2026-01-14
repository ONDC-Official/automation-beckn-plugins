package main

import (
	"os"
	"testing"
)

func TestEnvBool(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		def      bool
		expected bool
	}{
		{"empty returns default", "", true, true},
		{"empty returns default false", "", false, false},
		{"1 returns true", "1", false, true},
		{"true returns true", "true", false, true},
		{"TRUE returns true", "TRUE", false, true},
		{"yes returns true", "yes", false, true},
		{"y returns true", "y", false, true},
		{"on returns true", "on", false, true},
		{"0 returns false", "0", true, false},
		{"false returns false", "false", true, false},
		{"FALSE returns false", "FALSE", true, false},
		{"no returns false", "no", true, false},
		{"n returns false", "n", true, false},
		{"off returns false", "off", true, false},
		{"invalid returns default", "invalid", true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv("TEST_BOOL", tt.envValue)
			defer os.Unsetenv("TEST_BOOL")
			
			result := envBool("TEST_BOOL", tt.def)
			if result != tt.expected {
				t.Errorf("envBool() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestEnvInt(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		def      int
		expected int
	}{
		{"empty returns default", "", 42, 42},
		{"valid number", "100", 42, 100},
		{"negative number", "-10", 42, -10},
		{"zero", "0", 42, 0},
		{"invalid returns default", "abc", 42, 42},
		{"float returns default", "3.14", 42, 42},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv("TEST_INT", tt.envValue)
			defer os.Unsetenv("TEST_INT")
			
			result := envInt("TEST_INT", tt.def)
			if result != tt.expected {
				t.Errorf("envInt() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestParseEnvSet(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected map[string]bool
	}{
		{"empty string", "", map[string]bool{}},
		{"single value", "dev", map[string]bool{"dev": true}},
		{"multiple values", "dev,staging,prod", map[string]bool{"dev": true, "staging": true, "prod": true}},
		{"with spaces", " dev , staging , prod ", map[string]bool{"dev": true, "staging": true, "prod": true}},
		{"with empty elements", "dev,,prod", map[string]bool{"dev": true, "prod": true}},
		{"uppercase normalized", "DEV,STAGING", map[string]bool{"dev": true, "staging": true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseEnvSet(tt.input)
			if len(result) != len(tt.expected) {
				t.Errorf("parseEnvSet() length = %v, want %v", len(result), len(tt.expected))
			}
			for k := range tt.expected {
				if !result[k] {
					t.Errorf("parseEnvSet() missing key %v", k)
				}
			}
		})
	}
}

func TestLoadConfig(t *testing.T) {
	// Save original env vars
	origListenAddr := os.Getenv("RECORDER_LISTEN_ADDR")
	origHTTPListenAddr := os.Getenv("RECORDER_HTTP_LISTEN_ADDR")
	origRedisAddr := os.Getenv("REDIS_ADDR")
	origEnv := os.Getenv("RECORDER_ENV")
	
	defer func() {
		os.Setenv("RECORDER_LISTEN_ADDR", origListenAddr)
		os.Setenv("RECORDER_HTTP_LISTEN_ADDR", origHTTPListenAddr)
		os.Setenv("REDIS_ADDR", origRedisAddr)
		os.Setenv("RECORDER_ENV", origEnv)
	}()

	// Test default values (unset all vars after env load)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}

	// Just verify config loads successfully
	if cfg.ListenAddr == "" {
		t.Error("ListenAddr should not be empty")
	}
	if cfg.HTTPListenAddr == "" {
		t.Error("HTTPListenAddr should not be empty")
	}
	if cfg.RedisAddr == "" {
		t.Error("RedisAddr should not be empty")
	}
	if cfg.Env == "" {
		t.Error("Env should not be empty")
	}

	// Test custom values override everything
	os.Setenv("RECORDER_LISTEN_ADDR", ":9000")
	os.Setenv("RECORDER_HTTP_LISTEN_ADDR", ":9001")
	os.Setenv("REDIS_ADDR", "redis.local:6380")
	os.Setenv("RECORDER_ENV", "production")

	cfg, err = loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}

	if cfg.ListenAddr != ":9000" {
		t.Errorf("ListenAddr = %v, want :9000", cfg.ListenAddr)
	}
	if cfg.HTTPListenAddr != ":9001" {
		t.Errorf("HTTPListenAddr = %v, want :9001", cfg.HTTPListenAddr)
	}
	if cfg.RedisAddr != "redis.local:6380" {
		t.Errorf("RedisAddr = %v, want redis.local:6380", cfg.RedisAddr)
	}
	if cfg.Env != "production" {
		t.Errorf("Env = %v, want production", cfg.Env)
	}
}

func TestLoadConfigAsyncSettings(t *testing.T) {
	origQueueSize := os.Getenv("RECORDER_ASYNC_QUEUE_SIZE")
	origWorkers := os.Getenv("RECORDER_ASYNC_WORKERS")
	origDropOnFull := os.Getenv("RECORDER_ASYNC_DROP_ON_FULL")

	defer func() {
		os.Setenv("RECORDER_ASYNC_QUEUE_SIZE", origQueueSize)
		os.Setenv("RECORDER_ASYNC_WORKERS", origWorkers)
		os.Setenv("RECORDER_ASYNC_DROP_ON_FULL", origDropOnFull)
	}()

	os.Setenv("RECORDER_ASYNC_QUEUE_SIZE", "5000")
	os.Setenv("RECORDER_ASYNC_WORKERS", "10")
	os.Setenv("RECORDER_ASYNC_DROP_ON_FULL", "false")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}

	if cfg.AsyncQueueSize != 5000 {
		t.Errorf("AsyncQueueSize = %v, want 5000", cfg.AsyncQueueSize)
	}
	if cfg.AsyncWorkerCount != 10 {
		t.Errorf("AsyncWorkerCount = %v, want 10", cfg.AsyncWorkerCount)
	}
	if cfg.DropOnQueueFull != false {
		t.Errorf("DropOnQueueFull = %v, want false", cfg.DropOnQueueFull)
	}

	// Test minimum worker count
	os.Setenv("RECORDER_ASYNC_WORKERS", "-1")
	cfg, err = loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.AsyncWorkerCount < 1 {
		t.Errorf("AsyncWorkerCount = %v, should be at least 1", cfg.AsyncWorkerCount)
	}
}
