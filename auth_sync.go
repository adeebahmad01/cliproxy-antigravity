package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ensureAuthFile ensures an auth file for the agy provider is present in CLIProxyAPI's
// auth-dir so the credential and quota card are displayed in CLIProxyAPI's management UI.
func ensureAuthFile(configYAML []byte) {
	authDir := resolveAuthDir(configYAML)
	if authDir == "" {
		return
	}
	if info, err := os.Stat(authDir); err != nil || !info.IsDir() {
		return
	}

	// Check if a valid, non-empty agy auth file already exists.
	entries, err := os.ReadDir(authDir)
	if err != nil {
		return
	}

	var emptyCandidate string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		filePath := filepath.Join(authDir, entry.Name())
		data, errRead := os.ReadFile(filePath)
		if errRead != nil {
			continue
		}
		if len(strings.TrimSpace(string(data))) == 0 {
			if strings.HasPrefix(strings.ToLower(entry.Name()), "antigravity") || strings.HasPrefix(strings.ToLower(entry.Name()), "agy") {
				emptyCandidate = filePath
			}
			continue
		}
		var meta struct {
			Type string `json:"type"`
		}
		if errParse := json.Unmarshal(data, &meta); errParse == nil {
			if strings.EqualFold(meta.Type, "agy") {
				// Already present and valid
				return
			}
		}
	}

	// Determine account identifier / email from official agy CLI config
	email := detectAntigravityEmail()
	if email == "" {
		email = "antigravity-cli"
	}

	targetPath := emptyCandidate
	if targetPath == "" {
		safeEmail := sanitizeEmailForFilename(email)
		targetPath = filepath.Join(authDir, "antigravity-"+safeEmail+".json")
	}

	content := map[string]any{
		"type":     "agy",
		"email":    email,
		"label":    email,
		"disabled": false,
	}
	encoded, errMarshal := json.MarshalIndent(content, "", "  ")
	if errMarshal != nil {
		return
	}
	_ = os.WriteFile(targetPath, encoded, 0o600)
}

func resolveAuthDir(configYAML []byte) string {
	for _, line := range strings.Split(string(configYAML), "\n") {
		line = strings.TrimSpace(stripYAMLComment(line))
		if line == "" {
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue
		}
		key := normalizeConfigKey(strings.TrimSpace(line[:idx]))
		if key == "auth_dir" {
			val := unquoteYAMLScalar(strings.TrimSpace(line[idx+1:]))
			if val != "" {
				return expandHome(val)
			}
		}
	}

	// Default fallback: ~/.cli-proxy-api
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".cli-proxy-api")
	}
	return ""
}

func detectAntigravityEmail() string {
	if email := strings.TrimSpace(os.Getenv("ANTIGRAVITY_EMAIL")); email != "" {
		return email
	}
	if email := strings.TrimSpace(os.Getenv("AGY_EMAIL")); email != "" {
		return email
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}

	tokenPath := filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	if data, errRead := os.ReadFile(tokenPath); errRead == nil && len(data) > 0 {
		var tokenData struct {
			IDTokenEmail string `json:"id_token_email"`
			Email        string `json:"email"`
		}
		if errJSON := json.Unmarshal(data, &tokenData); errJSON == nil {
			if strings.TrimSpace(tokenData.IDTokenEmail) != "" {
				return strings.TrimSpace(tokenData.IDTokenEmail)
			}
			if strings.TrimSpace(tokenData.Email) != "" {
				return strings.TrimSpace(tokenData.Email)
			}
		}
	}

	settingsPath := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	if data, errRead := os.ReadFile(settingsPath); errRead == nil && len(data) > 0 {
		var settingsData struct {
			Email string `json:"email"`
			User  string `json:"user"`
		}
		if errJSON := json.Unmarshal(data, &settingsData); errJSON == nil {
			if strings.TrimSpace(settingsData.Email) != "" {
				return strings.TrimSpace(settingsData.Email)
			}
			if strings.TrimSpace(settingsData.User) != "" {
				return strings.TrimSpace(settingsData.User)
			}
		}
	}

	return ""
}

func sanitizeEmailForFilename(email string) string {
	var b strings.Builder
	for _, r := range email {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '@' || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	res := b.String()
	if res == "" {
		return "account"
	}
	return res
}

func expandHome(path string) string {
	trimmed := strings.TrimSpace(path)
	if strings.HasPrefix(trimmed, "~/") || strings.HasPrefix(trimmed, `~\`) || trimmed == "~" {
		home, err := os.UserHomeDir()
		if err == nil && home != "" {
			if trimmed == "~" {
				return home
			}
			return filepath.Join(home, trimmed[2:])
		}
	}
	return trimmed
}
