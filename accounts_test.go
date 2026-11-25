package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func signInAccount(t *testing.T, home, email string) {
	t.Helper()
	dir := agyStateDir(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	token, _ := json.Marshal(map[string]string{"id_token_email": email})
	if err := os.WriteFile(filepath.Join(dir, "antigravity-oauth-token"), token, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readAgyAuthFiles(t *testing.T, authDir string) map[string]map[string]any {
	t.Helper()
	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(authDir, e.Name()))
		var meta map[string]any
		if json.Unmarshal(data, &meta) == nil {
			out[e.Name()] = meta
		}
	}
	return out
}

func TestParsePluginConfigAccountsDir(t *testing.T) {
	dir := t.TempDir()
	cfg, err := parsePluginConfig([]byte("accounts_dir: " + dir + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccountsDir != dir {
		t.Fatalf("AccountsDir = %q, want %q", cfg.AccountsDir, dir)
	}

	file := filepath.Join(dir, "file")
	_ = os.WriteFile(file, nil, 0o600)
	if _, err := parsePluginConfig([]byte("accounts_dir: " + file + "\n")); err == nil {
		t.Fatal("expected error for accounts_dir pointing at a file")
	}
}

func TestWithAccount(t *testing.T) {
	base := defaultConfig()

	cfg, err := withAccount(base, nil)
	if err != nil || cfg.AccountHome != "" {
		t.Fatalf("no metadata: cfg.AccountHome=%q err=%v", cfg.AccountHome, err)
	}

	home := t.TempDir()
	cfg, err = withAccount(base, map[string]any{"home": home})
	if err != nil || cfg.AccountHome != home {
		t.Fatalf("home metadata: cfg.AccountHome=%q err=%v", cfg.AccountHome, err)
	}

	_, err = withAccount(base, map[string]any{"home": filepath.Join(home, "missing")})
	if pe := asPluginError(err); err == nil || pe.Code != "agy_account_home_missing" || pe.HTTPStatus != 503 {
		t.Fatalf("missing home: err=%v", err)
	}
}

func TestApplyAccountEnv(t *testing.T) {
	cmd := exec.Command("agy")
	applyAccountEnv(cmd, defaultConfig())
	if cmd.Env != nil {
		t.Fatalf("default account should inherit the environment, got %v", cmd.Env)
	}

	cfg := defaultConfig()
	cfg.AccountHome = "/accounts/work"
	applyAccountEnv(cmd, cfg)
	var home, dbusAddr, sshConn string
	for _, kv := range cmd.Env {
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			home = v // last one wins, as in exec.Cmd
		}
		if v, ok := strings.CutPrefix(kv, "DBUS_SESSION_BUS_ADDRESS="); ok {
			dbusAddr = v
		}
		if v, ok := strings.CutPrefix(kv, "SSH_CONNECTION="); ok {
			sshConn = v
		}
	}
	if home != "/accounts/work" {
		t.Fatalf("HOME = %q", home)
	}
	if runtime.GOOS == "linux" {
		if dbusAddr != "/dev/null" {
			t.Fatalf("DBUS_SESSION_BUS_ADDRESS = %q, want /dev/null", dbusAddr)
		}
		if sshConn == "" {
			t.Fatal("expected SSH_CONNECTION to be set on Linux for file-based token isolation")
		}
	}
}

func TestExecutorRequestDecodesAuthMetadata(t *testing.T) {
	var req rpcExecutorRequest
	if err := json.Unmarshal([]byte(`{"AuthID":"antigravity-a.json","AuthMetadata":{"type":"agy","home":"/accounts/a"}}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.AuthMetadata["home"] != "/accounts/a" {
		t.Fatalf("AuthMetadata = %#v", req.AuthMetadata)
	}
}

func TestEnsureAuthFileSyncsAccountsDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // default home is not signed in
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	authDir := t.TempDir()
	accountsDir := t.TempDir()
	signInAccount(t, filepath.Join(accountsDir, "work"), "work@example.com")
	signInAccount(t, filepath.Join(accountsDir, "personal"), "me@example.com")
	_ = os.MkdirAll(filepath.Join(accountsDir, "not-signed-in"), 0o755)

	cfg := defaultConfig()
	cfg.AccountsDir = accountsDir
	configYAML := []byte("auth-dir: " + authDir + "\n")
	ensureAuthFile(configYAML, cfg)

	files := readAgyAuthFiles(t, authDir)
	if len(files) != 2 {
		t.Fatalf("expected 2 auth files (no default, no unsigned account), got %v", files)
	}
	work := files["antigravity-work@example.com.json"]
	if work == nil || work["type"] != "agy" || work["home"] != filepath.Join(accountsDir, "work") {
		t.Fatalf("work auth file = %#v", work)
	}

	// A second sync must not duplicate accounts that already have an auth file.
	ensureAuthFile(configYAML, cfg)
	if got := len(readAgyAuthFiles(t, authDir)); got != 2 {
		t.Fatalf("resync created duplicates: %d files", got)
	}

	// Deleting a synced auth file must not recreate it on the next sync while its home stays marked.
	if err := os.Remove(filepath.Join(authDir, "antigravity-work@example.com.json")); err != nil {
		t.Fatal(err)
	}
	ensureAuthFile(configYAML, cfg)
	files = readAgyAuthFiles(t, authDir)
	if len(files) != 1 || files["antigravity-work@example.com.json"] != nil {
		t.Fatalf("deleted auth file was recreated: %v", files)
	}
	if !authFileIssued(filepath.Join(accountsDir, "work")) {
		t.Fatal("expected issued marker to remain in account home")
	}
}

func TestSyncAccountAuthFilesSkipsWhenMarkerCannotBeWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permissions")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	authDir := t.TempDir()
	accountsDir := t.TempDir()
	roHome := filepath.Join(accountsDir, "readonly")
	signInAccount(t, roHome, "ro@example.com")
	if err := os.Chmod(roHome, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roHome, 0o755) })

	cfg := defaultConfig()
	cfg.AccountsDir = accountsDir
	ensureAuthFile([]byte("auth-dir: "+authDir+"\n"), cfg)

	if files := readAgyAuthFiles(t, authDir); len(files) != 0 {
		t.Fatalf("expected no auth file issued when marker cannot be written, got %v", files)
	}
}

func TestEnsureAuthFileAccountsDirKeepsSignedInDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("ANTIGRAVITY_EMAIL", "")
	t.Setenv("AGY_EMAIL", "")
	signInAccount(t, home, "default@example.com")
	authDir := t.TempDir()
	accountsDir := t.TempDir()
	signInAccount(t, filepath.Join(accountsDir, "work"), "work@example.com")

	cfg := defaultConfig()
	cfg.AccountsDir = accountsDir
	ensureAuthFile([]byte("auth-dir: "+authDir+"\n"), cfg)

	files := readAgyAuthFiles(t, authDir)
	def := files["antigravity-default@example.com.json"]
	if def == nil || def["home"] != nil {
		t.Fatalf("expected a default auth file without home, got %v", files)
	}
	if files["antigravity-work@example.com.json"] == nil {
		t.Fatalf("expected the work account auth file, got %v", files)
	}
}

func TestLimitsCachedPerAccount(t *testing.T) {
	var homes []string
	prev := fetchLimitsFunc
	fetchLimitsFunc = func(cfg pluginConfig) (agyLimits, error) {
		homes = append(homes, cfg.AccountHome)
		return fixtureLimits(t), nil
	}
	invalidateLimits()
	t.Cleanup(func() {
		fetchLimitsFunc = prev
		invalidateLimits()
	})

	a, b := defaultConfig(), defaultConfig()
	a.AccountHome, b.AccountHome = "/accounts/a", "/accounts/b"
	_, _, _ = currentLimits(a)
	_, _, _ = currentLimits(b)
	_, _, _ = currentLimits(a)
	if len(homes) != 2 || homes[0] != "/accounts/a" || homes[1] != "/accounts/b" {
		t.Fatalf("expected one fetch per account, got %v", homes)
	}
}

func TestQuotaFetchUsesAccountHome(t *testing.T) {
	home := t.TempDir()
	var got string
	prev := fetchLimitsFunc
	fetchLimitsFunc = func(cfg pluginConfig) (agyLimits, error) {
		got = cfg.AccountHome
		return fixtureLimits(t), nil
	}
	invalidateLimits()
	t.Cleanup(func() {
		fetchLimitsFunc = prev
		invalidateLimits()
	})

	req, _ := json.Marshal(map[string]any{"auth_index": "abc", "metadata": map[string]any{"type": "agy", "home": home}})
	if _, err := handleMethod(methodQuotaFetch, req); err != nil {
		t.Fatal(err)
	}
	if got != home {
		t.Fatalf("quota fetch used home %q, want %q", got, home)
	}
}
