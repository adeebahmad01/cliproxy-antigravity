package main

import (
	"fmt"
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
				cfg.BinaryPath = value
			}
		case "workdir":
			cfg.Workdir = value
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

	if strings.TrimSpace(cfg.BinaryPath) == "" {
		return pluginConfig{}, fmt.Errorf("binary_path cannot be empty")
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
