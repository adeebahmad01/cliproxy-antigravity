package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ensureAuthFile ensures an auth file for the agy provider is present in CLIProxyAPI's
// auth-dir so the credential and quota card are displayed in CLIProxyAPI's management UI.
// With accounts_dir set, every signed-in account home in it gets its own auth file,
// so CLIProxyAPI schedules the accounts like any other multi-credential provider.
func ensureAuthFile(configYAML []byte, cfg pluginConfig) {
	authDir := resolveAuthDir(configYAML)
	if authDir == "" {
		return
	}
	if info, err := os.Stat(authDir); err != nil || !info.IsDir() {
		return
	}

	// Check which agy auth files already exist.
	entries, err := os.ReadDir(authDir)
	if err != nil {
		return
	}

	var emptyCandidate string
	hasAgyAuth := false
	knownHomes := map[string]bool{}
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
			Home string `json:"home"`
		}
		if errParse := json.Unmarshal(data, &meta); errParse == nil && strings.EqualFold(meta.Type, "agy") {
			hasAgyAuth = true
			if home, errHome := validateWorkdir(meta.Home); errHome == nil && home != "" {
				knownHomes[home] = true
			}
		}
	}

	if cfg.AccountsDir != "" {
		syncAccountAuthFiles(authDir, cfg.AccountsDir, knownHomes)
	}

	// The process HOME account is only added when no agy auth file exists yet. With accounts_dir
	// set it must also be signed in, so an unused default home does not join the rotation.
	if hasAgyAuth {
		return
	}
	if cfg.AccountsDir != "" {
		home, errHome := os.UserHomeDir()
		if errHome != nil || !accountSignedIn(home) {
			return
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
	writeAgyAuthFile(targetPath, email, "")
}

// syncAccountAuthFiles adds an auth file for every signed-in account home under accountsDir
// that no existing auth file points at. Accounts are signed in with HOME=<accountsDir>/<name> agy.
func syncAccountAuthFiles(authDir, accountsDir string, knownHomes map[string]bool) {
	entries, err := os.ReadDir(accountsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		home := filepath.Join(accountsDir, entry.Name())
		if knownHomes[home] || !accountSignedIn(home) {
			continue
		}
		email := emailFromHome(home)
		if email == "" {
			email = entry.Name()
		}
		targetPath := filepath.Join(authDir, "antigravity-"+sanitizeEmailForFilename(email)+".json")
		if _, errStat := os.Stat(targetPath); errStat == nil {
			targetPath = filepath.Join(authDir, "antigravity-"+sanitizeEmailForFilename(email)+"-"+sanitizeEmailForFilename(entry.Name())+".json")
			if _, errStat := os.Stat(targetPath); errStat == nil {
				continue
			}
		}
		writeAgyAuthFile(targetPath, email, home)
		knownHomes[home] = true
	}
}

func writeAgyAuthFile(path, email, home string) {
	content := map[string]any{
		"type":     "agy",
		"email":    email,
		"label":    email,
		"disabled": false,
	}
	if home != "" {
		content["home"] = home
	}
	encoded, errMarshal := json.MarshalIndent(content, "", "  ")
	if errMarshal != nil {
		return
	}
	_ = os.WriteFile(path, encoded, 0o600)
}

func agyStateDir(home string) string {
	return filepath.Join(home, ".gemini", "antigravity-cli")
}

// accountSignedIn reports whether agy has stored a login under home.
func accountSignedIn(home string) bool {
	info, err := os.Stat(filepath.Join(agyStateDir(home), "antigravity-oauth-token"))
	return err == nil && !info.IsDir() && info.Size() > 0
}

var authDirState struct {
	sync.RWMutex
	dir string
}

// setAuthDir remembers CLIProxyAPI's auth-dir from the last plugin configuration.
func setAuthDir(dir string) {
	authDirState.Lock()
	authDirState.dir = dir
	authDirState.Unlock()
}

func currentAuthDir() string {
	authDirState.RLock()
	defer authDirState.RUnlock()
	return authDirState.dir
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
	return emailFromHome(home)
}

// emailFromHome reads the signed-in account's email from the agy state under home.
func emailFromHome(home string) string {
	tokenPath := filepath.Join(agyStateDir(home), "antigravity-oauth-token")
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

	settingsPath := filepath.Join(agyStateDir(home), "settings.json")
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
