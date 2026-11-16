package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveAuthDir(t *testing.T) {
	yaml := []byte(`
host: 127.0.0.1
auth-dir: /tmp/custom-auth-dir
port: 8317
`)
	dir := resolveAuthDir(yaml)
	if dir != "/tmp/custom-auth-dir" {
		t.Fatalf("expected /tmp/custom-auth-dir, got %q", dir)
	}

	// Empty config falls back to ~/.cli-proxy-api
	defaultDir := resolveAuthDir(nil)
	if defaultDir == "" || !filepath.IsAbs(defaultDir) {
		t.Fatalf("expected absolute path for default auth-dir, got %q", defaultDir)
	}
}

func TestSanitizeEmailForFilename(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"user@gmail.com", "user@gmail.com"},
		{"user+test@domain.com", "user_test@domain.com"},
		{"user.name-123@sub.domain.co", "user.name-123@sub.domain.co"},
		{"", "account"},
	}
	for _, tc := range cases {
		got := sanitizeEmailForFilename(tc.input)
		if got != tc.expected {
			t.Errorf("sanitizeEmailForFilename(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestEnsureAuthFileCreatesFile(t *testing.T) {
	tmpDir := t.TempDir()
	configYAML := []byte("auth-dir: " + tmpDir + "\n")

	t.Setenv("ANTIGRAVITY_EMAIL", "test-user@example.com")
	ensureAuthFile(configYAML, defaultConfig())

	expectedPath := filepath.Join(tmpDir, "antigravity-test-user@example.com.json")
	data, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("failed to read created auth file: %v", err)
	}

	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("failed to unmarshal created auth file: %v", err)
	}
	if meta["type"] != "agy" {
		t.Errorf("expected type agy, got %v", meta["type"])
	}
	if meta["email"] != "test-user@example.com" {
		t.Errorf("expected email test-user@example.com, got %v", meta["email"])
	}
}

func TestEnsureAuthFileOverwritesEmptyCandidate(t *testing.T) {
	tmpDir := t.TempDir()
	emptyFile := filepath.Join(tmpDir, "antigravity-old@example.com.json")
	if err := os.WriteFile(emptyFile, []byte(""), 0o600); err != nil {
		t.Fatalf("failed to write empty file: %v", err)
	}

	configYAML := []byte("auth-dir: " + tmpDir + "\n")
	t.Setenv("ANTIGRAVITY_EMAIL", "new-user@example.com")
	ensureAuthFile(configYAML, defaultConfig())

	data, err := os.ReadFile(emptyFile)
	if err != nil {
		t.Fatalf("failed to read auth file: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("expected empty candidate to be populated, but it is still 0 bytes")
	}

	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("failed to unmarshal auth file: %v", err)
	}
	if meta["type"] != "agy" {
		t.Errorf("expected type agy, got %v", meta["type"])
	}
}

func TestEnsureAuthFileSkipsWhenValidExists(t *testing.T) {
	tmpDir := t.TempDir()
	existingFile := filepath.Join(tmpDir, "antigravity-existing.json")
	initialContent := []byte(`{"type":"agy","email":"keep-me@example.com","disabled":false}`)
	if err := os.WriteFile(existingFile, initialContent, 0o600); err != nil {
		t.Fatalf("failed to write existing file: %v", err)
	}

	configYAML := []byte("auth-dir: " + tmpDir + "\n")
	t.Setenv("ANTIGRAVITY_EMAIL", "different@example.com")
	ensureAuthFile(configYAML, defaultConfig())

	// Check that existing file wasn't replaced
	data, err := os.ReadFile(existingFile)
	if err != nil {
		t.Fatalf("failed to read existing file: %v", err)
	}
	if string(data) != string(initialContent) {
		t.Errorf("expected existing file to be untouched, got %s", string(data))
	}

	// And no second file was created
	files, _ := os.ReadDir(tmpDir)
	if len(files) != 1 {
		t.Errorf("expected exactly 1 file in authDir, found %d", len(files))
	}
}
