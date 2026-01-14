package main

import (
	"testing"
)

func TestGetString(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]any
		key      string
		expected string
	}{
		{"valid string", map[string]any{"key": "value"}, "key", "value"},
		{"missing key", map[string]any{}, "key", ""},
		{"nil value", map[string]any{"key": nil}, "key", ""},
		{"wrong type", map[string]any{"key": 123}, "key", ""},
		{"empty string", map[string]any{"key": ""}, "key", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getString(tt.input, tt.key)
			if result != tt.expected {
				t.Errorf("getString() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestGetInt64(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]any
		key      string
		expected int64
	}{
		{"float64", map[string]any{"key": float64(42)}, "key", 42},
		{"int64", map[string]any{"key": int64(42)}, "key", 42},
		{"int", map[string]any{"key": 42}, "key", 42},
		{"string number", map[string]any{"key": "42"}, "key", 42},
		{"string negative", map[string]any{"key": "-10"}, "key", -10},
		{"missing key", map[string]any{}, "key", 0},
		{"nil value", map[string]any{"key": nil}, "key", 0},
		{"invalid string", map[string]any{"key": "abc"}, "key", 0},
		{"wrong type", map[string]any{"key": true}, "key", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getInt64(tt.input, tt.key)
			if result != tt.expected {
				t.Errorf("getInt64() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestGetBool(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]any
		key      string
		expected bool
	}{
		{"true", map[string]any{"key": true}, "key", true},
		{"false", map[string]any{"key": false}, "key", false},
		{"missing key", map[string]any{}, "key", false},
		{"nil value", map[string]any{"key": nil}, "key", false},
		{"wrong type", map[string]any{"key": "true"}, "key", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getBool(tt.input, tt.key)
			if result != tt.expected {
				t.Errorf("getBool() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestMergeMaps(t *testing.T) {
	a := map[string]any{"a": 1, "b": 2, "shared": "from_a"}
	b := map[string]any{"c": 3, "d": 4, "shared": "from_b"}

	result := mergeMaps(a, b)

	if result["a"].(int) != 1 {
		t.Errorf("result[a] = %v, want 1", result["a"])
	}
	if result["b"].(int) != 2 {
		t.Errorf("result[b] = %v, want 2", result["b"])
	}
	if result["c"].(int) != 3 {
		t.Errorf("result[c] = %v, want 3", result["c"])
	}
	if result["d"].(int) != 4 {
		t.Errorf("result[d] = %v, want 4", result["d"])
	}
	if result["shared"].(string) != "from_b" {
		t.Errorf("result[shared] = %v, want from_b (b should override a)", result["shared"])
	}

	// Ensure original maps are not modified
	if a["c"] != nil {
		t.Errorf("original map a was modified")
	}
	if b["a"] != nil {
		t.Errorf("original map b was modified")
	}
}

func TestSha256Hex(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty string", ""},
		{"hello", "hello"},
		{"special chars", "test@123!"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sha256Hex(tt.input)
			// Just verify it returns a valid hex string of correct length
			if len(result) != 64 {
				t.Errorf("sha256Hex() length = %v, want 64", len(result))
			}
			// Verify it's consistent
			result2 := sha256Hex(tt.input)
			if result != result2 {
				t.Errorf("sha256Hex() not consistent: %v != %v", result, result2)
			}
		})
	}
}

func TestUuidV4(t *testing.T) {
	uuid1, err := uuidV4()
	if err != nil {
		t.Fatalf("uuidV4() error = %v", err)
	}

	if len(uuid1) != 36 {
		t.Errorf("UUID length = %v, want 36", len(uuid1))
	}

	if uuid1[8] != '-' || uuid1[13] != '-' || uuid1[18] != '-' || uuid1[23] != '-' {
		t.Errorf("UUID format incorrect: %v", uuid1)
	}

	// Check version (should be 4)
	if uuid1[14] != '4' {
		t.Errorf("UUID version = %v, want 4", uuid1[14])
	}

	// Generate multiple UUIDs and ensure they're unique
	uuid2, err := uuidV4()
	if err != nil {
		t.Fatalf("uuidV4() error = %v", err)
	}

	if uuid1 == uuid2 {
		t.Errorf("Generated UUIDs are not unique: %v == %v", uuid1, uuid2)
	}
}

func TestGetContextString(t *testing.T) {
	tests := []struct {
		name     string
		body     map[string]any
		key      string
		expected string
	}{
		{"valid context key", map[string]any{"context": map[string]any{"transaction_id": "t1"}}, "transaction_id", "t1"},
		{"missing context", map[string]any{}, "transaction_id", ""},
		{"nil context", map[string]any{"context": nil}, "transaction_id", ""},
		{"wrong context type", map[string]any{"context": "string"}, "transaction_id", ""},
		{"missing key in context", map[string]any{"context": map[string]any{}}, "transaction_id", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getContextString(tt.body, tt.key)
			if result != tt.expected {
				t.Errorf("getContextString() = %v, want %v", result, tt.expected)
			}
		})
	}
}
