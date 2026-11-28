package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestParsePluginConfig(t *testing.T) {
	cfg, err := parsePluginConfig([]byte(`
enabled: true
binary_path: "/opt/bin/agy"
workdir: '/tmp/project'
print_timeout: 12m
dangerously_skip_permissions: true
sandbox: false
reasoning_effort: high
store:
  version: 0.1.0
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BinaryPath != "/opt/bin/agy" {
		t.Fatalf("BinaryPath = %q", cfg.BinaryPath)
	}
	if cfg.Workdir != "/tmp/project" {
		t.Fatalf("Workdir = %q", cfg.Workdir)
	}
	if cfg.PrintTimeout != "12m" {
		t.Fatalf("PrintTimeout = %q", cfg.PrintTimeout)
	}
	if !cfg.DangerouslySkipPermissions {
		t.Fatal("expected dangerously_skip_permissions=true")
	}
	if cfg.Sandbox {
		t.Fatal("expected sandbox=false")
	}
	if cfg.DefaultReasoningEffort != "high" {
		t.Fatalf("expected DefaultReasoningEffort 'high', got %q", cfg.DefaultReasoningEffort)
	}
}

func TestParsePluginConfigRejectsBadTimeout(t *testing.T) {
	if _, err := parsePluginConfig([]byte("print_timeout: forever\n")); err == nil {
		t.Fatal("expected invalid timeout error")
	}
}

func TestParsePluginConfigRejectsBadReasoningEffort(t *testing.T) {
	if _, err := parsePluginConfig([]byte("reasoning_effort: extreme\n")); err == nil {
		t.Fatal("expected invalid reasoning_effort error")
	}
}

func TestParsePluginConfigAcceptsExtendedReasoningEfforts(t *testing.T) {
	cases := map[string]string{
		"low":        "low",
		"medium":     "medium",
		"med":        "medium",
		"high":       "high",
		"xhigh":      "xhigh",
		"x-high":     "xhigh",
		"extra-high": "xhigh",
		"max":        "max",
	}
	for input, want := range cases {
		cfg, err := parsePluginConfig([]byte("reasoning_effort: " + input + "\n"))
		if err != nil {
			t.Fatalf("parsePluginConfig(%q) unexpected error: %v", input, err)
		}
		if cfg.DefaultReasoningEffort != want {
			t.Fatalf("parsePluginConfig(%q).DefaultReasoningEffort = %q, want %q", input, cfg.DefaultReasoningEffort, want)
		}
	}
}

func TestParsePluginConfigSecurityValidations(t *testing.T) {
	// Rejects arbitrary non-agy executables
	badBinaries := []string{
		"/bin/sh",
		"/usr/bin/bash",
		"cmd.exe",
		"powershell.exe",
		"curl",
		"/usr/bin/python3",
		"agy; rm -rf /",
		"agy && whoami",
		"agy | cat",
	}
	for _, bad := range badBinaries {
		if _, err := parsePluginConfig([]byte("binary_path: " + bad + "\n")); err == nil {
			t.Fatalf("expected security violation for binary_path %q, but got nil error", bad)
		}
	}

	// Accepts valid agy binaries and variants
	goodBinaries := []string{
		"agy",
		"antigravity",
		"/usr/local/bin/agy",
		"/opt/google/antigravity",
		"C:\\Program Files\\Google\\agy.exe",
		"C:\\Tools\\antigravity.exe",
	}
	for _, good := range goodBinaries {
		if _, err := parsePluginConfig([]byte("binary_path: " + good + "\n")); err != nil {
			t.Fatalf("expected binary_path %q to be allowed, got error: %v", good, err)
		}
	}
}

func TestParsePluginConfigEmptyValuesKeepDefaults(t *testing.T) {
	cfg, err := parsePluginConfig([]byte(`
enabled: true
priority: 1
reasoning_effort: ""
limits_refresh_interval: ''
enforce_limits: null
`))
	if err != nil {
		t.Fatal(err)
	}
	want := defaultConfig()
	if cfg.DefaultReasoningEffort != "" || cfg.LimitsRefreshInterval != want.LimitsRefreshInterval || cfg.EnforceLimits != want.EnforceLimits {
		t.Fatalf("empty values should keep defaults, got %+v", cfg)
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("skipping TestExpandHome: cannot determine home dir")
	}

	tests := []struct {
		input string
		want  string
	}{
		{"~", home},
		{"~/", home},
		{"~/.local/bin/agy", filepath.Join(home, ".local", "bin", "agy")},
		{"~/bin/antigravity", filepath.Join(home, "bin", "antigravity")},
		{"/opt/homebrew/bin/agy", "/opt/homebrew/bin/agy"},
		{"agy", "agy"},
	}

	for _, tc := range tests {
		got := expandHome(tc.input)
		if got != tc.want {
			t.Errorf("expandHome(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestResolveBinaryPathWithTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("skipping: cannot determine home dir")
	}

	// Create a temp executable in home/.local/bin or temp dir
	tmpDir := t.TempDir()
	binName := "agy"
	if runtime.GOOS == "windows" {
		binName = "agy.exe"
	}
	fakeAgy := filepath.Join(tmpDir, binName)
	if err := os.WriteFile(fakeAgy, []byte("#!/bin/sh\necho test\n"), 0755); err != nil {
		t.Fatal(err)
	}

	// Direct resolution with explicit path
	resolved := resolveBinaryPath(fakeAgy)
	if resolved != fakeAgy {
		t.Errorf("resolveBinaryPath(%q) = %q, want %q", fakeAgy, resolved, fakeAgy)
	}
}

func TestResolveBinaryPathCandidateSearch(t *testing.T) {
	// Verify that candidates directory list contains standard macOS and Linux paths
	dirs := getBinarySearchDirectories()
	foundLocalBin := false
	foundHomebrew := false
	foundUsrLocal := false

	home, _ := os.UserHomeDir()
	expectedLocalBin := filepath.Join(home, ".local", "bin")

	for _, d := range dirs {
		if d == expectedLocalBin {
			foundLocalBin = true
		}
		if d == "/opt/homebrew/bin" {
			foundHomebrew = true
		}
		if d == "/usr/local/bin" {
			foundUsrLocal = true
		}
	}

	if home != "" && !foundLocalBin {
		t.Errorf("expected %q in search directories", expectedLocalBin)
	}
	if !foundHomebrew {
		t.Errorf("expected /opt/homebrew/bin in search directories")
	}
	if !foundUsrLocal {
		t.Errorf("expected /usr/local/bin in search directories")
	}
}

func TestParsePluginConfigPromptOverflowAndKeepalive(t *testing.T) {
	cfg, err := parsePluginConfig([]byte(`
max_prompt_bytes: 150000
max_prompt_action: error
stream_keepalive_interval: 15s
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxPromptBytes != 150000 || cfg.MaxPromptAction != promptActionError || cfg.StreamKeepaliveInterval != "15s" {
		t.Fatalf("unexpected config: %+v", cfg)
	}

	disabled, err := parsePluginConfig([]byte("max_prompt_bytes: 0\nstream_keepalive_interval: 0s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if disabled.MaxPromptBytes != 0 || disabled.StreamKeepaliveInterval != "" {
		t.Fatalf("expected 0 max_prompt_bytes and 0s keepalive to disable, got %+v", disabled)
	}

	def := defaultConfig()
	if def.MaxPromptBytes != defaultMaxPromptBytes || def.StreamKeepaliveInterval != defaultStreamKeepalive {
		t.Fatalf("unexpected prompt/keepalive defaults: %+v", def)
	}

	for _, bad := range []string{
		"max_prompt_bytes: -1\n",
		"max_prompt_bytes: 1024\n",
		"max_prompt_action: drop\n",
		"stream_keepalive_interval: 10s\n",
		"stream_keepalive_interval: -5s\n",
		"stream_keepalive_interval: nope\n",
	} {
		if _, err := parsePluginConfig([]byte(bad)); err == nil {
			t.Fatalf("expected error for config %q", bad)
		}
	}
}

func TestParseACPSessionSettings(t *testing.T) {
	cfg, err := parsePluginConfig([]byte("acp_session_idle_ttl: 12h\nacp_oneshot_session_ttl: 2m\nacp_session_cache_size: 512\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ACPSessionIdleTTL != 12*time.Hour || cfg.ACPOneShotSessionTTL != 2*time.Minute || cfg.ACPSessionCacheSize != 512 {
		t.Fatalf("parsed %v %v %d", cfg.ACPSessionIdleTTL, cfg.ACPOneShotSessionTTL, cfg.ACPSessionCacheSize)
	}
	def := defaultConfig()
	if def.ACPSessionIdleTTL != 30*time.Minute || def.ACPOneShotSessionTTL != 30*time.Minute || def.ACPSessionCacheSize != 256 {
		t.Fatalf("defaults changed: %v %v %d", def.ACPSessionIdleTTL, def.ACPOneShotSessionTTL, def.ACPSessionCacheSize)
	}
	for _, bad := range []string{"acp_session_idle_ttl: 1s", "acp_oneshot_session_ttl: soon", "acp_session_cache_size: 3"} {
		if _, err := parsePluginConfig([]byte(bad + "\n")); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
