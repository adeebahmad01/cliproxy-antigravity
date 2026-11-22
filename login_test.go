//go:build !windows

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAgyLogin mimics agy's sign-in screen: pick a login method, open $BROWSER, read the code.
const fakeAgyLogin = `#!/bin/sh
echo "Welcome to the Antigravity CLI. You are currently not signed in."
echo "Select login method:"
read choice
"$BROWSER" "https://accounts.google.com/o/oauth2/auth?client_id=x&redirect_uri=https%3A%2F%2Fantigravity.google%2Foauth-callback&state=Abc123State"
echo "paste the authorization code below:"
read code
if [ "$code" = "good-code" ]; then
  mkdir -p "$HOME/.gemini/antigravity-cli"
  printf '{"id_token_email":"new@example.com"}' > "$HOME/.gemini/antigravity-cli/antigravity-oauth-token"
  sleep 30
else
  echo "Invalid authorization code"
fi
`

func setupFakeLogin(t *testing.T) (pluginConfig, string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(bin, []byte(fakeAgyLogin), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.BinaryPath = bin
	cfg.AccountsDir = t.TempDir()
	return cfg, t.TempDir()
}

func writeCallback(t *testing.T, authDir, state, code, errMsg string) {
	t.Helper()
	data, _ := json.Marshal(map[string]string{"code": code, "state": state, "error": errMsg})
	if err := os.WriteFile(filepath.Join(authDir, ".oauth-agy-"+state+".oauth"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func pollUntilDone(t *testing.T, state, authDir string) authLoginPollResponse {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp := pollAgyLogin(state, authDir)
		if resp.Status != "pending" {
			return resp
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("login did not finish")
	return authLoginPollResponse{}
}

func TestAgyLoginFlow(t *testing.T) {
	cfg, authDir := setupFakeLogin(t)

	start, err := startAgyLogin(cfg)
	if err != nil {
		t.Fatalf("startAgyLogin: %v", err)
	}
	if start.State != "Abc123State" || !strings.HasPrefix(start.URL, "https://accounts.google.com/o/oauth2/auth?") {
		t.Fatalf("unexpected start response: %#v", start)
	}
	if resp := pollAgyLogin(start.State, authDir); resp.Status != "pending" {
		t.Fatalf("before the code is pasted, status = %#v", resp)
	}

	writeCallback(t, authDir, start.State, "good-code", "")
	resp := pollUntilDone(t, start.State, authDir)
	if resp.Status != "success" {
		t.Fatalf("status = %#v", resp)
	}
	home, _ := resp.Auth.Metadata["home"].(string)
	if resp.Auth.Provider != "agy" || resp.Auth.FileName != "antigravity-new@example.com.json" || resp.Auth.Label != "new@example.com" {
		t.Fatalf("auth = %#v", resp.Auth)
	}
	if filepath.Dir(home) != cfg.AccountsDir || !accountSignedIn(home) {
		t.Fatalf("account home %q is not a signed-in home under %q", home, cfg.AccountsDir)
	}
	if _, err := os.Stat(filepath.Join(authDir, ".oauth-agy-"+start.State+".oauth")); !os.IsNotExist(err) {
		t.Fatal("callback file should be consumed")
	}
	if resp := pollAgyLogin(start.State, authDir); resp.Status != "error" {
		t.Fatalf("a finished session must not be reusable, got %#v", resp)
	}
}

func TestAgyLoginBadCodeRemovesHome(t *testing.T) {
	cfg, authDir := setupFakeLogin(t)
	start, err := startAgyLogin(cfg)
	if err != nil {
		t.Fatalf("startAgyLogin: %v", err)
	}
	writeCallback(t, authDir, start.State, "wrong", "")
	resp := pollUntilDone(t, start.State, authDir)
	if resp.Status != "error" || !strings.Contains(resp.Message, "Invalid authorization code") {
		t.Fatalf("status = %#v", resp)
	}
	if entries, _ := os.ReadDir(cfg.AccountsDir); len(entries) != 0 {
		t.Fatalf("failed sign-in should leave no account home, found %d", len(entries))
	}
}

func TestAgyLoginGoogleError(t *testing.T) {
	cfg, authDir := setupFakeLogin(t)
	start, err := startAgyLogin(cfg)
	if err != nil {
		t.Fatalf("startAgyLogin: %v", err)
	}
	writeCallback(t, authDir, start.State, "", "access_denied")
	if resp := pollAgyLogin(start.State, authDir); resp.Status != "error" || !strings.Contains(resp.Message, "access_denied") {
		t.Fatalf("status = %#v", resp)
	}
}

func TestParseAgyAuth(t *testing.T) {
	resp := parseAgyAuth(authParseRequest{FileName: "antigravity-a.json", RawJSON: []byte(`{"type":"agy","email":"a@example.com","home":"/accounts/a","disabled":true}`)})
	if !resp.Handled || resp.Auth.Label != "a@example.com" || !resp.Auth.Disabled || resp.Auth.Metadata["home"] != "/accounts/a" {
		t.Fatalf("parse = %#v", resp)
	}
	if resp := parseAgyAuth(authParseRequest{RawJSON: []byte(`{"type":"codex"}`)}); resp.Handled {
		t.Fatal("non-agy auth files must not be handled")
	}
}

func TestAccountsManagementRoutes(t *testing.T) {
	authDir := t.TempDir()
	setAuthDir(authDir)
	t.Cleanup(func() { setAuthDir("") })
	home := t.TempDir()
	signInAccount(t, home, "a@example.com")
	raw, _ := json.Marshal(map[string]any{"type": "agy", "email": "a@example.com", "home": home})
	_ = os.WriteFile(filepath.Join(authDir, "antigravity-a@example.com.json"), raw, 0o600)
	_ = os.WriteFile(filepath.Join(authDir, "codex-b.json"), []byte(`{"type":"codex"}`), 0o600)
	stubLimits(t, fixtureLimits(t), nil)

	resp := handleManagement(managementRequest{Method: "GET", Path: "/v0/management/agy/accounts"})
	var report accountsReport
	if err := json.Unmarshal(resp.Body, &report); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d err=%v body=%s", resp.StatusCode, err, resp.Body)
	}
	if len(report.Accounts) != 1 || !report.Accounts[0].SignedIn || len(report.Accounts[0].Groups) != 2 {
		t.Fatalf("report = %#v", report)
	}
	if resp.Headers.Get("Cache-Control") != "no-store" {
		t.Fatal("account details must not be stored by HTTP caches")
	}

	page := handleManagement(managementRequest{Method: "GET", Path: "/v0/resource/plugins/cliproxy-antigravity/accounts"})
	if page.StatusCode != http.StatusOK || !strings.HasPrefix(page.Headers.Get("Content-Type"), "text/html") {
		t.Fatalf("page status=%d headers=%v", page.StatusCode, page.Headers)
	}
	for _, private := range []string{"a@example.com", home, `type="password"`} {
		if strings.Contains(string(page.Body), private) {
			t.Fatalf("public page contained %q", private)
		}
	}
	// Account data must never be reachable through the unauthenticated resource prefix.
	if resp := handleManagement(managementRequest{Method: "GET", Path: "/v0/resource/plugins/cliproxy-antigravity/agy/accounts"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("resource prefix leaked account data: status %d", resp.StatusCode)
	}
}

func TestRegistrationAdvertisesAuthProvider(t *testing.T) {
	// Without this flag CLIProxyAPI never routes agy logins to the plugin ("provider_not_found").
	if !pluginRegistration().Capabilities.AuthProvider {
		t.Fatal("plugin registration must advertise the auth_provider capability")
	}
}

func accountsFromRoute(t *testing.T) []accountReport {
	t.Helper()
	resp := handleManagement(managementRequest{Method: "GET", Path: "/v0/management/agy/accounts"})
	var report accountsReport
	if err := json.Unmarshal(resp.Body, &report); err != nil {
		t.Fatalf("decode accounts: %v (%s)", err, resp.Body)
	}
	return report.Accounts
}

// The accounts page must find auth files after a normal registration, and must follow the
// auth-dir the host reports once an auth request carries it.
func TestAccountsRouteUsesHostAuthDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	setAuthDir("")
	t.Cleanup(func() { setAuthDir("") })
	stubLimits(t, fixtureLimits(t), nil)

	defaultDir := filepath.Join(home, ".cli-proxy-api")
	_ = os.MkdirAll(defaultDir, 0o700)
	_ = os.WriteFile(filepath.Join(defaultDir, "antigravity-a.json"), []byte(`{"type":"agy","email":"a@example.com"}`), 0o600)
	reg, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte("enabled: true\n"), SchemaVersion: schemaVersion})
	if _, err := handleMethod(methodPluginRegister, reg); err != nil {
		t.Fatal(err)
	}
	if got := accountsFromRoute(t); len(got) != 1 || got[0].Email != "a@example.com" {
		t.Fatalf("after register, accounts = %#v", got)
	}

	hostDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(hostDir, "antigravity-b.json"), []byte(`{"type":"agy","email":"b@example.com"}`), 0o600)
	parse, _ := json.Marshal(map[string]any{"FileName": "antigravity-b.json", "RawJSON": []byte(`{"type":"agy"}`), "Host": map[string]any{"AuthDir": hostDir}})
	if _, err := handleMethod(methodAuthParse, parse); err != nil {
		t.Fatal(err)
	}
	if got := accountsFromRoute(t); len(got) != 1 || got[0].Email != "b@example.com" {
		t.Fatalf("after host reported its auth-dir, accounts = %#v", got)
	}
}

// On macOS agy ignores $BROWSER and wraps the visible link; only the OSC 8 target is complete.
func TestFindLoginURLPrefersHyperlinkTarget(t *testing.T) {
	full := "https://accounts.google.com/o/oauth2/auth?client_id=x&redirect_uri=https%3A%2F%2Fantigravity.google%2Foauth-callback&state=Abc123State"
	raw := "Your browser should open automatically. If not:\r\n" +
		"\x1b]8;id=1;" + full + "\x1b\\" + full[:78] + "\r\n" + full[78:] + "\x1b]8;;\x1b\\\r\n"
	if got := findLoginURL([]byte(raw)); got != full {
		t.Fatalf("findLoginURL = %q, want %q", got, full)
	}
	if got := findLoginURL([]byte("open " + full + "\n")); got != full {
		t.Fatalf("findLoginURL without hyperlink = %q, want %q", got, full)
	}
}
