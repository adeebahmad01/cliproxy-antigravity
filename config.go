package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type pluginConfig struct {
	BinaryPath                 string
	Workdir                    string
	PrintTimeout               string
	DangerouslySkipPermissions bool
	Sandbox                    bool
	DefaultReasoningEffort     string
}

func defaultConfig() pluginConfig {
	return pluginConfig{
		BinaryPath:   "agy",
		PrintTimeout: "30m",
	}
}

var configState = struct {
	sync.RWMutex
	value pluginConfig
}{value: defaultConfig()}

func setConfig(cfg pluginConfig) {
	configState.Lock()
	configState.value = cfg
	configState.Unlock()
	invalidateModels()
}

func currentConfig() pluginConfig {
	configState.RLock()
	defer configState.RUnlock()
	return configState.value
}

func validateBinaryPath(path string) error {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return fmt.Errorf("binary_path cannot be empty")
	}
	if strings.ContainsAny(trimmed, ";|&$`\n\r<>") {
		return fmt.Errorf("security violation: binary_path contains illegal shell metacharacters: %q", path)
	}

	base := strings.ToLower(filepath.Base(strings.ReplaceAll(trimmed, "\\", "/")))
	base = strings.TrimSuffix(base, ".exe")
	base = strings.TrimSuffix(base, ".cmd")
	base = strings.TrimSuffix(base, ".bat")
	if base != "agy" && base != "antigravity" {
		return fmt.Errorf("security policy violation: binary_path must be an 'agy' or 'antigravity' executable, got %q", filepath.Base(trimmed))
	}
	return nil
}

func validateWorkdir(dir string) (string, error) {
	trimmed := strings.TrimSpace(dir)
	if trimmed == "" {
		return "", nil
	}
	clean := filepath.Clean(trimmed)
	abs, err := filepath.Abs(clean)
	if err != nil {
		return "", fmt.Errorf("invalid workdir path: %w", err)
	}
	if stat, err := os.Stat(abs); err == nil {
		if !stat.IsDir() {
			return "", fmt.Errorf("workdir is not a directory: %s", abs)
		}
	}
	return abs, nil
}

func parsePluginConfig(raw []byte) (pluginConfig, error) {
	cfg := defaultConfig()
	if len(raw) == 0 {
		return cfg, nil
	}

	for lineNo, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(stripYAMLComment(line))
		if line == "" || strings.HasPrefix(line, "---") {
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue
		}
		key := normalizeConfigKey(strings.TrimSpace(line[:idx]))
		value := strings.TrimSpace(line[idx+1:])
		if value == "" { // nested mapping such as store:
			continue
		}
		value = unquoteYAMLScalar(value)

		switch key {
		case "binary_path":
			if value != "" {
				if err := validateBinaryPath(value); err != nil {
					return pluginConfig{}, fmt.Errorf("line %d: %w", lineNo+1, err)
				}
				cfg.BinaryPath = value
			}
		case "workdir":
			cleaned, err := validateWorkdir(value)
			if err != nil {
				return pluginConfig{}, fmt.Errorf("line %d: %w", lineNo+1, err)
			}
			cfg.Workdir = cleaned
		case "print_timeout":
			if value == "" {
				continue
			}
			if _, err := time.ParseDuration(value); err != nil {
				return pluginConfig{}, fmt.Errorf("line %d: invalid print_timeout %q: %w", lineNo+1, value, err)
			}
			cfg.PrintTimeout = value
		case "dangerously_skip_permissions":
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return pluginConfig{}, fmt.Errorf("line %d: dangerously_skip_permissions must be true or false", lineNo+1)
			}
			cfg.DangerouslySkipPermissions = parsed
		case "sandbox":
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return pluginConfig{}, fmt.Errorf("line %d: sandbox must be true or false", lineNo+1)
			}
			cfg.Sandbox = parsed
		case "reasoning_effort", "effort":
			effort := normalizeEffort(value)
			if effort == "" {
				return pluginConfig{}, fmt.Errorf("line %d: invalid reasoning_effort %q: must be low, medium, or high", lineNo+1, value)
			}
			cfg.DefaultReasoningEffort = effort
		}
	}

	if err := validateBinaryPath(cfg.BinaryPath); err != nil {
		return pluginConfig{}, err
	}
	return cfg, nil
}

func normalizeConfigKey(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.ReplaceAll(s, "-", "_")
	return s
}

func stripYAMLComment(line string) string {
	var quote rune
	escaped := false
	for i, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if quote == '"' && r == '\\' {
			escaped = true
			continue
		}
		if r == '\'' || r == '"' {
			if quote == 0 {
				quote = r
			} else if quote == r {
				quote = 0
			}
			continue
		}
		if r == '#' && quote == 0 {
			return line[:i]
		}
	}
	return line
}

func unquoteYAMLScalar(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		if v[0] == '\'' && v[len(v)-1] == '\'' {
			return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
		}
		if v[0] == '"' && v[len(v)-1] == '"' {
			if unquoted, err := strconv.Unquote(v); err == nil {
				return unquoted
			}
		}
	}
	return v
}
