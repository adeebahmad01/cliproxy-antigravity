package main

import "testing"

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
