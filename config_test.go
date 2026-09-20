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
