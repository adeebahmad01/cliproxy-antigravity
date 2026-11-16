package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	LimitsTracking             bool
	LimitsRefreshInterval      string
	EnforceLimits              bool
}

func defaultConfig() pluginConfig {
	return pluginConfig{
		BinaryPath:            "agy",
		PrintTimeout:          "30m",
		LimitsTracking:        true,
		LimitsRefreshInterval: "1m",
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
	invalidateLimits()
}

func currentConfig() pluginConfig {
	configState.RLock()
	defer configState.RUnlock()
	return configState.value
}


func isExecutableFile(path string) bool {
	stat, err := os.Stat(path)
	if err != nil || stat.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return stat.Mode().Perm()&0111 != 0
}

func getBinarySearchDirectories() []string {
	var dirs []string
	home, _ := os.UserHomeDir()

	if home != "" {
		dirs = append(dirs,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "bin"),
			filepath.Join(home, ".gemini", "antigravity-cli"),
			filepath.Join(home, ".gemini", "antigravity-cli", "bin"),
			filepath.Join(home, ".gemini", "bin"),
			filepath.Join(home, ".antigravity", "bin"),
			filepath.Join(home, ".antigravity"),
			filepath.Join(home, "Applications", "Antigravity.app", "Contents", "MacOS"),
			filepath.Join(home, "Library", "Application Support", "Antigravity", "bin"),
			filepath.Join(home, "Library", "Application Support", "antigravity-cli"),
			filepath.Join(home, ".cargo", "bin"),
			filepath.Join(home, ".npm-global", "bin"),
			filepath.Join(home, ".yarn", "bin"),
			filepath.Join(home, ".nix-profile", "bin"),
			filepath.Join(home, "go", "bin"),
		)
		if runtime.GOOS == "windows" {
			dirs = append(dirs,
				filepath.Join(home, "AppData", "Local", "Programs", "Antigravity"),
				filepath.Join(home, "AppData", "Local", "Programs", "antigravity-cli"),
				filepath.Join(home, "AppData", "Local", "Microsoft", "WinGet", "Links"),
				filepath.Join(home, "scoop", "shims"),
			)
		}
	}

	// Standard system paths across macOS, Linux, and Unix
	dirs = append(dirs,
		"/opt/homebrew/bin",
		"/opt/homebrew/sbin",
		"/usr/local/bin",
		"/usr/local/sbin",
		"/usr/bin",
		"/bin",
		"/usr/sbin",
		"/sbin",
		"/opt/local/bin",
		"/opt/local/sbin",
		"/nix/var/nix/profiles/default/bin",
		"/var/lib/snapd/snap/bin",
		"/snap/bin",
		"/Applications/Antigravity.app/Contents/MacOS",
	)

	// Windows system paths
	if runtime.GOOS == "windows" {
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			dirs = append(dirs, filepath.Join(pf, "Antigravity"), filepath.Join(pf, "antigravity-cli"))
		}
		if pfx86 := os.Getenv("ProgramFiles(x86)"); pfx86 != "" {
			dirs = append(dirs, filepath.Join(pfx86, "Antigravity"), filepath.Join(pfx86, "antigravity-cli"))
		}
		if pd := os.Getenv("ProgramData"); pd != "" {
			dirs = append(dirs, filepath.Join(pd, "chocolatey", "bin"))
		}
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			dirs = append(dirs,
				filepath.Join(la, "Programs", "Antigravity"),
				filepath.Join(la, "Microsoft", "WinGet", "Links"),
			)
		}
	}

	return dirs
}

func resolveBinaryPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		trimmed = "agy"
	}
	expanded := expandHome(trimmed)

	// If explicit path with directory separators was provided:
	if strings.ContainsAny(expanded, `/\`) {
		if isExecutableFile(expanded) {
			return expanded
		}
		if lp, err := exec.LookPath(expanded); err == nil {
			return lp
		}
		return expanded
	}

	// Try in current process PATH first
	if lp, err := exec.LookPath(expanded); err == nil {
		return lp
	}

	// Not in PATH, search comprehensive candidate locations
	baseNames := []string{expanded}
	if expanded == "agy" {
		baseNames = append(baseNames, "antigravity")
	} else if expanded == "antigravity" {
		baseNames = append(baseNames, "agy")
	}

	if runtime.GOOS == "windows" {
		var winNames []string
		for _, name := range baseNames {
			winNames = append(winNames, name)
			if !strings.HasSuffix(strings.ToLower(name), ".exe") {
				winNames = append(winNames, name+".exe", name+".cmd", name+".bat")
			}
		}
		baseNames = winNames
	}

	for _, dir := range getBinarySearchDirectories() {
		for _, name := range baseNames {
			target := filepath.Join(dir, name)
			if isExecutableFile(target) {
				return target
			}
		}
	}

	return expanded
}

func validateBinaryPath(path string) error {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return fmt.Errorf("binary_path cannot be empty")
	}
	if strings.ContainsAny(trimmed, ";|&$`\n\r<>") {
		return fmt.Errorf("security violation: binary_path contains illegal shell metacharacters: %q", path)
	}

	expanded := expandHome(trimmed)
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(expanded, "\\", "/")))
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
	expanded := expandHome(trimmed)
	clean := filepath.Clean(expanded)
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
		if value == "" || value == "~" || value == "null" { // management clients may save empty fields; keep the default
			continue
		}

		switch key {
		case "binary_path":
			if value != "" {
				if err := validateBinaryPath(value); err != nil {
					return pluginConfig{}, fmt.Errorf("line %d: %w", lineNo+1, err)
				}
				cfg.BinaryPath = expandHome(value)
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
		case "limits_tracking":
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return pluginConfig{}, fmt.Errorf("line %d: limits_tracking must be true or false", lineNo+1)
			}
			cfg.LimitsTracking = parsed
		case "limits_refresh_interval":
			d, err := time.ParseDuration(value)
			if err != nil || d < 10*time.Second {
				return pluginConfig{}, fmt.Errorf("line %d: invalid limits_refresh_interval %q: must be a duration of at least 10s", lineNo+1, value)
			}
			cfg.LimitsRefreshInterval = value
		case "enforce_limits":
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return pluginConfig{}, fmt.Errorf("line %d: enforce_limits must be true or false", lineNo+1)
			}
			cfg.EnforceLimits = parsed
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
