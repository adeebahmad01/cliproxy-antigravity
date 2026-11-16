package main

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Adding an account from CLIProxyAPI runs agy's own interactive sign-in under a fresh account
// home. agy hands its Google sign-in URL to $BROWSER; Google redirects to
// antigravity.google/oauth-callback, and the user pastes that URL (or the code it shows) into
// CLIProxyAPI. The code is typed back into agy, which exchanges and stores the tokens itself.

const (
	loginStartTimeout = 45 * time.Second
	loginTTL          = 15 * time.Minute
	loginCodeTimeout  = 2 * time.Minute
	loginOutputLimit  = 256 << 10
)

// Auth provider wire types (CLIProxyAPI's pluginapi.Auth* structs; no json tags, so Go field names).
type authData struct {
	Provider         string
	ID               string `json:",omitempty"`
	FileName         string
	Label            string
	Disabled         bool
	StorageJSON      []byte
	Metadata         map[string]any
	NextRefreshAfter time.Time `json:",omitempty"`
}

type hostConfigSummary struct {
	AuthDir string
}

type authParseRequest struct {
	Provider string
	FileName string
	RawJSON  []byte
}

type authParseResponse struct {
	Handled bool
	Auth    authData
}

type authLoginStartRequest struct {
	Host     hostConfigSummary
	Metadata map[string]any
}

type authLoginStartResponse struct {
	Provider  string
	URL       string
	State     string
	ExpiresAt time.Time
}

type authLoginPollRequest struct {
	State string
	Host  hostConfigSummary
}

type authLoginPollResponse struct {
	Status  string
	Message string `json:",omitempty"`
	Auth    authData
}

type authRefreshRequest struct {
	StorageJSON []byte
	Metadata    map[string]any
}

type authRefreshResponse struct {
	Auth             authData
	NextRefreshAfter time.Time
}

// parseAgyAuth hands agy auth files back to the host unchanged, so their fields (such as home)
// keep reaching the executor as auth metadata.
func parseAgyAuth(req authParseRequest) authParseResponse {
	var meta map[string]any
	if err := json.Unmarshal(req.RawJSON, &meta); err != nil || meta == nil {
		return authParseResponse{}
	}
	if typ, _ := meta["type"].(string); !strings.EqualFold(typ, providerID) {
		return authParseResponse{}
	}
	return authParseResponse{Handled: true, Auth: agyAuthData(req.FileName, meta)}
}

func agyAuthData(fileName string, meta map[string]any) authData {
	raw, _ := json.Marshal(meta)
	label, _ := meta["label"].(string)
	if label == "" {
		label, _ = meta["email"].(string)
	}
	disabled, _ := meta["disabled"].(bool)
	return authData{
		Provider:    providerID,
		FileName:    fileName,
		Label:       label,
		Disabled:    disabled,
		StorageJSON: raw,
		Metadata:    meta,
		// agy refreshes its own tokens; there is nothing for the host to refresh.
		NextRefreshAfter: time.Now().Add(24 * time.Hour).UTC(),
	}
}

func refreshAgyAuth(req authRefreshRequest) authRefreshResponse {
	meta := req.Metadata
	if meta == nil {
		_ = json.Unmarshal(req.StorageJSON, &meta)
	}
	if meta == nil {
		meta = map[string]any{"type": providerID}
	}
	auth := agyAuthData("", meta)
	return authRefreshResponse{Auth: auth, NextRefreshAfter: auth.NextRefreshAfter}
}

type loginSession struct {
	mu        sync.Mutex
	state     string
	home      string
	name      string
	helperDir string
	cmd       *exec.Cmd
	pty       *os.File
	output    []byte
	answered  bool
	codeSent  time.Time
	exited    chan struct{}
	expiresAt time.Time
}

var loginSessions = struct {
	sync.Mutex
	byState map[string]*loginSession
}{byState: map[string]*loginSession{}}

var (
	ansiPattern     = regexp.MustCompile(`\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[()][A-Za-z0-9]|\x1b[=>]`)
	loginURLPattern = regexp.MustCompile(`https://accounts\.google\.com/o/oauth2/[^\s"'<>]+`)
	oauthStateChars = regexp.MustCompile(`^[A-Za-z0-9._-]{1,256}$`)
)

func screenText(raw []byte) string {
	return ansiPattern.ReplaceAllString(string(raw), "")
}

// screenTail returns the last meaningful lines agy printed, for error messages.
func (s *loginSession) screenTail() string {
	s.mu.Lock()
	text := screenText(s.output)
	s.mu.Unlock()
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.ContainsAny(line, "▀▄") {
			lines = append(lines, line)
		}
	}
	if len(lines) > 4 {
		lines = lines[len(lines)-4:]
	}
	return strings.Join(lines, " / ")
}

func accountsDirFor(cfg pluginConfig) (string, error) {
	if cfg.AccountsDir != "" {
		return cfg.AccountsDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("cannot determine a directory for account homes; set accounts_dir")
	}
	return filepath.Join(home, ".agy-accounts"), nil
}

func startAgyLogin(cfg pluginConfig) (authLoginStartResponse, error) {
	if err := validateBinaryPath(cfg.BinaryPath); err != nil {
		return authLoginStartResponse{}, err
	}
	accountsDir, err := accountsDirFor(cfg)
	if err != nil {
		return authLoginStartResponse{}, err
	}
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		return authLoginStartResponse{}, fmt.Errorf("create accounts_dir: %w", err)
	}
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	name := "account-" + time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(suffix)
	home := filepath.Join(accountsDir, name)
	if err := os.Mkdir(home, 0o700); err != nil {
		return authLoginStartResponse{}, fmt.Errorf("create account home: %w", err)
	}
	helperDir, err := os.MkdirTemp("", "agy-login-")
	if err != nil {
		os.RemoveAll(home)
		return authLoginStartResponse{}, err
	}
	s := &loginSession{home: home, name: name, helperDir: helperDir, exited: make(chan struct{})}

	// agy opens the sign-in URL with $BROWSER; capture it instead of opening a browser on the server.
	urlFile := filepath.Join(helperDir, "url")
	browser := filepath.Join(helperDir, "browser")
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" > '" + urlFile + "'\n"
	if err := os.WriteFile(browser, []byte(script), 0o700); err != nil {
		s.cleanup(true)
		return authLoginStartResponse{}, err
	}

	cmd := exec.Command(resolveBinaryPath(cfg.BinaryPath))
	cmd.Dir = home
	cmd.Env = append(os.Environ(),
		"HOME="+home, "USERPROFILE="+home, "BROWSER="+browser,
		"DISPLAY=", "WAYLAND_DISPLAY=", "TERM=xterm-256color")
	pty, err := startInPTY(cmd)
	if err != nil {
		s.cleanup(true)
		return authLoginStartResponse{}, fmt.Errorf("start agy sign-in: %w", err)
	}
	s.cmd, s.pty = cmd, pty
	activeProcesses.add(cmd)
	go s.readOutput()
	go func() {
		_ = cmd.Wait()
		activeProcesses.remove(cmd)
		close(s.exited)
	}()

	loginURL, err := s.waitForURL(urlFile)
	if err != nil {
		s.cleanup(true)
		return authLoginStartResponse{}, err
	}
	parsed, err := url.Parse(loginURL)
	state := ""
	if err == nil {
		state = parsed.Query().Get("state")
	}
	if !oauthStateChars.MatchString(state) {
		s.cleanup(true)
		return authLoginStartResponse{}, errors.New("agy sign-in URL has no usable state parameter")
	}
	s.state = state
	s.expiresAt = time.Now().Add(loginTTL)

	loginSessions.Lock()
	loginSessions.byState[state] = s
	loginSessions.Unlock()
	time.AfterFunc(loginTTL, func() { s.finish(false) })

	return authLoginStartResponse{Provider: providerID, URL: loginURL, State: state, ExpiresAt: s.expiresAt.UTC()}, nil
}

// readOutput records the terminal output and picks "Google OAuth" when agy asks for a login method.
func (s *loginSession) readOutput() {
	buf := make([]byte, 32<<10)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.output = append(s.output, buf[:n]...)
			if len(s.output) > loginOutputLimit {
				s.output = append([]byte(nil), s.output[len(s.output)-loginOutputLimit:]...)
			}
			answer := !s.answered && strings.Contains(screenText(s.output), "Select login method")
			if answer {
				s.answered = true
			}
			s.mu.Unlock()
			if answer {
				_, _ = s.pty.Write([]byte("\r"))
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *loginSession) waitForURL(urlFile string) (string, error) {
	deadline := time.After(loginStartTimeout)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if data, err := os.ReadFile(urlFile); err == nil && loginURLPattern.Match(data) {
			return loginURLPattern.FindString(string(data)), nil
		}
		s.mu.Lock()
		found := loginURLPattern.FindString(screenText(s.output))
		s.mu.Unlock()
		if found != "" {
			return found, nil
		}
		select {
		case <-s.exited:
			return "", fmt.Errorf("agy exited before showing a sign-in link: %s", s.screenTail())
		case <-deadline:
			return "", fmt.Errorf("agy did not show a sign-in link within %s: %s", loginStartTimeout, s.screenTail())
		case <-tick.C:
		}
	}
}

// finish stops agy and forgets the session. Unless the account signed in, its home is removed.
func (s *loginSession) finish(keepHome bool) {
	loginSessions.Lock()
	if loginSessions.byState[s.state] == s {
		delete(loginSessions.byState, s.state)
	}
	loginSessions.Unlock()
	s.cleanup(!keepHome)
}

func (s *loginSession) cleanup(removeHome bool) {
	if s.cmd != nil {
		killProcessTree(s.cmd)
		select {
		case <-s.exited:
		case <-time.After(5 * time.Second):
		}
	}
	if s.pty != nil {
		s.pty.Close()
	}
	os.RemoveAll(s.helperDir)
	if removeHome && !accountSignedIn(s.home) {
		os.RemoveAll(s.home)
	}
}

func pollAgyLogin(state, authDir string) authLoginPollResponse {
	loginSessions.Lock()
	s := loginSessions.byState[state]
	loginSessions.Unlock()
	if s == nil {
		return authLoginPollResponse{Status: "error", Message: "this Antigravity sign-in expired or was already used; start again"}
	}

	if accountSignedIn(s.home) {
		s.finish(true)
		return authLoginPollResponse{Status: "success", Message: "Antigravity account added", Auth: newAccountAuth(s, authDir)}
	}
	select {
	case <-s.exited:
		msg := "agy sign-in ended without signing in"
		if tail := s.screenTail(); tail != "" {
			msg += ": " + tail
		}
		s.finish(false)
		return authLoginPollResponse{Status: "error", Message: msg}
	default:
	}

	s.mu.Lock()
	codeSent := s.codeSent
	s.mu.Unlock()
	if codeSent.IsZero() {
		code, errMsg, ok := readOAuthCallback(authDir, state)
		if ok {
			if errMsg != "" {
				s.finish(false)
				return authLoginPollResponse{Status: "error", Message: "Google sign-in failed: " + errMsg}
			}
			s.mu.Lock()
			s.codeSent = time.Now()
			s.mu.Unlock()
			_, _ = s.pty.Write([]byte(code + "\r"))
		}
	} else if time.Since(codeSent) > loginCodeTimeout {
		msg := "agy did not finish signing in with the pasted code"
		if tail := s.screenTail(); tail != "" {
			msg += ": " + tail
		}
		s.finish(false)
		return authLoginPollResponse{Status: "error", Message: msg}
	}
	return authLoginPollResponse{Status: "pending"}
}

// readOAuthCallback reads (and consumes) the callback CLIProxyAPI stores when the user pastes
// the redirect URL or code into the management UI.
func readOAuthCallback(authDir, state string) (code, errMsg string, ok bool) {
	if authDir == "" {
		return "", "", false
	}
	path := filepath.Join(authDir, ".oauth-"+providerID+"-"+state+".oauth")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	var payload struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return "", "", false
	}
	_ = os.Remove(path)
	code = strings.TrimSpace(payload.Code)
	errMsg = strings.TrimSpace(payload.Error)
	if code == "" && errMsg == "" {
		return "", "", false
	}
	return code, errMsg, true
}

func newAccountAuth(s *loginSession, authDir string) authData {
	email := emailFromHome(s.home)
	if email == "" {
		email = s.name
	}
	fileName := "antigravity-" + sanitizeEmailForFilename(email) + ".json"
	if authDir != "" {
		if _, err := os.Stat(filepath.Join(authDir, fileName)); err == nil {
			fileName = "antigravity-" + sanitizeEmailForFilename(email) + "-" + s.name + ".json"
		}
	}
	meta := map[string]any{"type": providerID, "email": email, "label": email, "home": s.home, "disabled": false}
	auth := agyAuthData(fileName, meta)
	auth.ID = fileName
	return auth
}

// Accounts overview for the plugin's management page.

//go:embed accounts_page.html
var accountsPageHTML []byte

const accountsManagementPath = "/agy/accounts"
const accountsResourcePath = "/accounts"

type accountReport struct {
	File      string          `json:"file"`
	Email     string          `json:"email"`
	Home      string          `json:"home,omitempty"`
	Disabled  bool            `json:"disabled"`
	SignedIn  bool            `json:"signed_in"`
	FetchedAt string          `json:"fetched_at,omitempty"`
	Groups    []agyLimitGroup `json:"groups"`
	Error     string          `json:"error,omitempty"`
}

type accountsReport struct {
	AccountsDir string          `json:"accounts_dir"`
	Tracking    bool            `json:"tracking"`
	Accounts    []accountReport `json:"accounts"`
}

func listAccounts(cfg pluginConfig, authDir string, refresh bool) accountsReport {
	report := accountsReport{Tracking: cfg.LimitsTracking, Accounts: []accountReport{}}
	report.AccountsDir, _ = accountsDirFor(cfg)
	entries, _ := os.ReadDir(authDir)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(authDir, name))
		if err != nil {
			continue
		}
		var meta map[string]any
		if json.Unmarshal(data, &meta) != nil {
			continue
		}
		if typ, _ := meta["type"].(string); !strings.EqualFold(typ, providerID) {
			continue
		}
		acc := accountReport{File: name, Groups: []agyLimitGroup{}}
		acc.Email, _ = meta["email"].(string)
		acc.Home, _ = meta["home"].(string)
		acc.Disabled, _ = meta["disabled"].(bool)
		accCfg, err := withAccount(cfg, meta)
		if err != nil {
			acc.Error = err.Error()
			report.Accounts = append(report.Accounts, acc)
			continue
		}
		home := accCfg.AccountHome
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		acc.SignedIn = accountSignedIn(home)
		report.Accounts = append(report.Accounts, acc)
	}

	if !cfg.LimitsTracking {
		return report
	}
	var wg sync.WaitGroup
	for i := range report.Accounts {
		acc := &report.Accounts[i]
		if acc.Error != "" || !acc.SignedIn || acc.Disabled {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			accCfg, _ := withAccount(cfg, map[string]any{"home": acc.Home})
			if refresh {
				markAccountLimitsStale(accCfg)
			}
			limits, fetchedAt, err := currentLimits(accCfg)
			if limits != nil {
				acc.Groups = limits.Groups
			}
			if !fetchedAt.IsZero() {
				acc.FetchedAt = fetchedAt.UTC().Format(time.RFC3339)
			}
			if err != nil {
				acc.Error = err.Error()
			}
		}()
	}
	wg.Wait()
	return report
}

func markAccountLimitsStale(cfg pluginConfig) {
	c := limitsCacheFor(cfg)
	c.Lock()
	c.stale = true
	c.Unlock()
}

func accountsPageResponse() managementResponse {
	return managementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":  []string{"text/html; charset=utf-8"},
			"Cache-Control": []string{"no-store"},
		},
		Body: bytes.Clone(accountsPageHTML),
	}
}
