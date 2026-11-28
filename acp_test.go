package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeACPAgy implements enough of `agy acp` JSON-RPC over stdin/stdout to exercise
// resident reuse, model selection, permission denial, session reuse, and crash recovery.
const fakeACPAgy = `#!/bin/sh
if [ "$1" != "acp" ]; then
  if [ "$1" = "models" ]; then
    printf 'gemini-4-hydrogen-xhigh\tGemini 4 Hydrogen (XHigh)\n'
    exit 0
  fi
  exit 1
fi
echo "$$" >> "$ACP_PID_LOG"
session_n=0
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  method=$(printf '%s' "$line" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  case "$method" in
    initialize)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1}}\n' "$id"
      ;;
    session/new)
      session_n=$((session_n + 1))
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"sess-%s","configOptions":[{"id":"model","category":"model","options":[{"value":"gemini-4-hydrogen","name":"Gemini 4 Hydrogen"}]}],"models":{"availableModels":[{"modelId":"gemini-4-hydrogen-xhigh","name":"Gemini 4 Hydrogen (XHigh)"},{"modelId":"gemini-4-hydrogen-max","name":"Gemini 4 Hydrogen (Max)"}]}}}\n' "$id" "$session_n"
      ;;
    session/set_model)
      model=$(printf '%s' "$line" | sed -n 's/.*"modelId":"\([^"]*\)".*/\1/p')
      printf '%s\n' "$model" >> "$ACP_MODEL_LOG"
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
    session/prompt)
      printf '%s\n' "$line" >> "$ACP_PROMPT_LOG"
      sid=$(printf '%s' "$line" | sed -n 's/.*"sessionId":"\([^"]*\)".*/\1/p')
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"%s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"acp ok (%s)"}}}}\n' "$sid" "$sid"
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn","usage":{"inputTokens":10,"outputTokens":4,"cachedReadTokens":5,"thoughtTokens":1}}}\n' "$id"
      ;;
    session/close)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
  esac
done
`

func setupFakeACP(t *testing.T) (pluginConfig, string, string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub agy acp is a shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "agy")
	if err := os.WriteFile(bin, []byte(fakeACPAgy), 0o755); err != nil {
		t.Fatal(err)
	}
	pidLog := filepath.Join(dir, "pids.log")
	modelLog := filepath.Join(dir, "models.log")
	promptLog := filepath.Join(dir, "prompts.log")
	t.Setenv("ACP_PID_LOG", pidLog)
	t.Setenv("ACP_MODEL_LOG", modelLog)
	t.Setenv("ACP_PROMPT_LOG", promptLog)

	cfg := defaultConfig()
	cfg.BinaryPath = bin
	cfg.Mode = modeACP
	cfg.Workdir = dir
	resetACPQuiesceForTest()
	setConfig(cfg)
	t.Cleanup(func() {
		closeAllACP()
		resetACPQuiesceForTest()
		setConfig(defaultConfig())
	})
	return cfg, pidLog, modelLog, promptLog
}

func TestConfigModeParsingAndDefault(t *testing.T) {
	cfg, err := parsePluginConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != modePrint {
		t.Fatalf("default Mode = %q, want %q", cfg.Mode, modePrint)
	}
	cfg, err = parsePluginConfig([]byte("mode: acp\nconv_debug_file: ~/conv.jsonl\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != modeACP || !strings.HasSuffix(cfg.ConvDebugFile, "conv.jsonl") {
		t.Fatalf("parsed config = %+v", cfg)
	}
	if _, err := parsePluginConfig([]byte("mode: invalid\n")); err == nil {
		t.Fatal("expected error for invalid mode")
	}
}

func TestACPModelChoiceSupportsExtendedEfforts(t *testing.T) {
	models := acpModelSet{
		Bases: []acpModelOption{{ID: "gemini-4-hydrogen"}},
		Variants: []acpModelOption{
			{ID: "gemini-4-hydrogen-xhigh"},
			{ID: "gemini-4-hydrogen-max"},
		},
	}
	if got := acpModelChoice(agyOptions{NativeModel: "gemini-4-hydrogen", ReasoningEffort: "xhigh"}, models); got != "gemini-4-hydrogen-xhigh" {
		t.Fatalf("got %q, want gemini-4-hydrogen-xhigh", got)
	}
	if got := acpModelChoice(agyOptions{NativeModel: "gemini-4-hydrogen", ReasoningEffort: "max"}, models); got != "gemini-4-hydrogen-max" {
		t.Fatalf("got %q, want gemini-4-hydrogen-max", got)
	}
	if got := acpModelChoice(agyOptions{NativeModel: "gemini-4-hydrogen", ReasoningEffort: "low"}, models); got != "gemini-4-hydrogen" {
		t.Fatalf("missing variant should fall back to base model, got %q", got)
	}
}

func TestACPResidentExecutionAndSessionReuse(t *testing.T) {
	useTempModelCache(t)
	_, pidLog, modelLog, promptLog := setupFakeACP(t)
	acctHome := t.TempDir()
	signInAccount(t, acctHome, "acp@example.com")
	authMeta := map[string]any{"type": providerID, "home": acctHome}

	// Turn 1: starts one resident agy acp (shared by discovery + execution) and opens sess-1.
	req1 := rpcExecutorRequest{
		Model:        "agy/gemini-4-hydrogen:xhigh",
		AuthMetadata: authMeta,
		Payload:      []byte(`{"model":"agy/gemini-4-hydrogen:xhigh","messages":[{"role":"user","content":"hello"}]}`),
	}
	resp1, err := executeNonStream(req1)
	if err != nil {
		t.Fatalf("turn 1 failed: %v", err)
	}
	var body1 map[string]any
	if err := json.Unmarshal(resp1.Payload, &body1); err != nil {
		t.Fatal(err)
	}
	content1, _ := body1["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if content1 != "acp ok (sess-2)" {
		t.Fatalf("unexpected turn 1 response: %s", resp1.Payload)
	}
	if _, exists := body1["conversation_id"]; exists {
		t.Fatalf("ACP response must not expose raw session ID as conversation_id: %s", resp1.Payload)
	}
	usage := body1["usage"].(map[string]any)
	// inputTokens(10) + cachedReadTokens(5) = 15 prompt_tokens
	if usage["prompt_tokens"] != float64(15) {
		t.Fatalf("prompt_tokens = %v, want 15", usage["prompt_tokens"])
	}

	// Turn 2: repeats turn 1's transcript -> reuses sess-2 on the same resident PID!
	req2 := rpcExecutorRequest{
		Model:        "agy/gemini-4-hydrogen:xhigh",
		AuthMetadata: authMeta,
		Payload: []byte(`{"model":"agy/gemini-4-hydrogen:xhigh","messages":[
			{"role":"user","content":"hello"},
			{"role":"assistant","content":"` + content1 + `"},
			{"role":"user","content":"follow up"}
		]}`),
	}
	resp2, err := executeNonStream(req2)
	if err != nil {
		t.Fatalf("turn 2 failed: %v", err)
	}
	var body2 map[string]any
	_ = json.Unmarshal(resp2.Payload, &body2)
	if body2["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != content1 {
		t.Fatalf("expected %s reuse, got: %s", content1, resp2.Payload)
	}

	pidsRaw, _ := os.ReadFile(pidLog)
	if lines := strings.Fields(string(pidsRaw)); len(lines) != 1 {
		t.Fatalf("expected 1 resident process across turns, got %d (%v)", len(lines), lines)
	}
	modelsRaw, _ := os.ReadFile(modelLog)
	if !strings.Contains(string(modelsRaw), "gemini-4-hydrogen-xhigh") {
		t.Fatalf("expected xhigh model variant selected, got %q", string(modelsRaw))
	}
	promptsRaw, _ := os.ReadFile(promptLog)
	promptLines := strings.Split(strings.TrimSpace(string(promptsRaw)), "\n")
	if len(promptLines) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(promptLines))
	}
	if strings.Contains(promptLines[1], "hello") {
		t.Fatalf("reused session prompt should only send follow-up turn, got %s", promptLines[1])
	}
}

func TestUseACPFallsBackToPrintForConversationID(t *testing.T) {
	cfg := defaultConfig()
	cfg.Mode = modeACP
	if !useACP(cfg, agyOptions{}) {
		t.Fatal("expected ACP when mode=acp and no conversation_id")
	}
	if useACP(cfg, agyOptions{ConversationID: "agy.v1.abc"}) {
		t.Fatal("requests with conversation_id must stay on print mode")
	}
	cfg.Mode = modePrint
	if useACP(cfg, agyOptions{}) {
		t.Fatal("mode=print must not use ACP")
	}
}

func TestACPFollowUpPromptGuardsSlashCommands(t *testing.T) {
	msg := []chatMessage{{Role: "user", Content: json.RawMessage(`"/usage"`)}}
	out, err := buildFollowUpPrompt(msg, false, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if startsWithSlash(out) {
		t.Fatalf("slash-command follow-up must be wrapped, got %q", out)
	}
	if !strings.Contains(out, "--- message role=user ---") || !strings.Contains(out, "/usage") {
		t.Fatalf("unexpected wrapped follow-up: %q", out)
	}

	normal := []chatMessage{{Role: "user", Content: json.RawMessage(`"normal question"`)}}
	out2, err := buildFollowUpPrompt(normal, false, "", nil)
	if err != nil || out2 != "normal question" {
		t.Fatalf("normal follow-up = %q, %v", out2, err)
	}
}

func TestACPQuiesceAndShutdown(t *testing.T) {
	_, pidLog, _, _ := setupFakeACP(t)
	c, err := acquireACP(currentConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !c.alive() {
		t.Fatal("resident should be alive")
	}
	if _, err := os.Stat(pidLog); err != nil {
		t.Fatal(err)
	}
	if _, err := handleMethod(methodPluginShutdown, nil); err != nil {
		t.Fatal(err)
	}
	if c.alive() {
		t.Fatal("plugin.shutdown must close resident")
	}
	if err := beginACPWork(); err == nil {
		t.Fatal("expected ACP work to be rejected after quiesce/shutdown")
	}
}

func TestACPRetiresDisabledAndLastAccountWithoutKillingBusyTurns(t *testing.T) {
	env := newDiscoveryEnv(t)
	a := env.account(t, "acct-a", true)
	b := env.account(t, "acct-b", true)
	env.authFile(t, "a.json", `{"type":"agy","home":"`+a+`"}`)
	env.authFile(t, "b.json", `{"type":"agy","home":"`+b+`"}`)
	mustForAuth(t, "a", a)
	mustForAuth(t, "b", b)

	fakeRes := func(home string) *acpClient {
		return &acpClient{key: "fake:" + home, home: home, cmd: &exec.Cmd{}, in: nopWriteCloser{}, dead: make(chan struct{})}
	}
	residentA, residentB := fakeRes(a), fakeRes(b)
	residentB.busy.Store(1)
	acpPool.Lock()
	acpPool.clients = map[string]*acpClient{residentA.key: residentA, residentB.key: residentB}
	acpPool.Unlock()
	t.Cleanup(func() {
		acpPool.Lock()
		acpPool.clients = map[string]*acpClient{}
		acpPool.Unlock()
	})

	env.authFile(t, "b.json", `{"type":"agy","disabled":true,"home":"`+b+`"}`)
	reconcileAccounts()

	time.Sleep(150 * time.Millisecond)
	if !residentB.alive() {
		t.Fatal("busy resident was killed mid-turn")
	}
	if !residentA.alive() {
		t.Fatal("enabled resident was closed")
	}
	residentB.busy.Store(0)
	deadline := time.Now().Add(3 * time.Second)
	for residentB.alive() && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if residentB.alive() {
		t.Fatal("idle disabled resident was not closed")
	}

	// Disable the last remaining account: residentA must also be retired.
	env.authFile(t, "a.json", `{"type":"agy","disabled":true,"home":"`+a+`"}`)
	reconcileAccounts()
	deadline = time.Now().Add(3 * time.Second)
	for residentA.alive() && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if residentA.alive() {
		t.Fatal("last disabled account resident was not retired")
	}
}

func TestACPPermissionRequestDenialAndAllow(t *testing.T) {
	var buf bytes.Buffer
	denyClient := &acpClient{in: nopWriteCloserBuf{&buf}, skipPermissions: false}
	denyClient.answerAgentRequest(acpMessage{
		ID:     json.RawMessage("1"),
		Method: "session/request_permission",
		Params: json.RawMessage(`{"options":[{"optionId":"allow-once","kind":"allow_once"}]}`),
	})
	if !strings.Contains(buf.String(), `"outcome":"cancelled"`) {
		t.Fatalf("expected cancelled permission outcome, got %s", buf.String())
	}

	buf.Reset()
	allowClient := &acpClient{in: nopWriteCloserBuf{&buf}, skipPermissions: true}
	allowClient.answerAgentRequest(acpMessage{
		ID:     json.RawMessage("2"),
		Method: "session/request_permission",
		Params: json.RawMessage(`{"options":[{"optionId":"allow-once","kind":"allow_once"}]}`),
	})
	if !strings.Contains(buf.String(), `"optionId":"allow-once"`) {
		t.Fatalf("expected selected allow option, got %s", buf.String())
	}
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

type nopWriteCloserBuf struct{ *bytes.Buffer }

func (nopWriteCloserBuf) Close() error { return nil }

func TestSSRFBlocksLoopbackAndPrivateURLs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secret"))
	}))
	defer srv.Close()

	if _, err := acpImageBlock(srv.URL + "/img.png"); err == nil {
		t.Fatal("expected loopback httptest URL to be blocked by SSRF check")
	}
	for _, blocked := range []string{
		"http://example.com/cleartext.png",
		"http://169.254.169.254/latest/meta-data/",
		"https://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/internal.png",
		"http://192.168.1.1/router.png",
		"http://localhost/local.png",
		"http://[::1]/v6.png",
		"https://user:secret@example.com/private.png",
		"file:///etc/passwd",
	} {
		if _, err := acpImageBlock(blocked); err == nil {
			t.Errorf("expected %q to be blocked", blocked)
		}
	}
	if !isDisallowedIP(net.ParseIP("100.64.0.1")) {
		t.Fatal("expected CGNAT IP to be disallowed")
	}

	// Verify CheckRedirect strips Referer before forwarding to a redirect target and blocks HTTP downgrade.
	client := newSSRFSafeHTTPClient()
	redirectReq, _ := http.NewRequest(http.MethodGet, "https://cdn.example.com/target.png", nil)
	redirectReq.Header.Set("Referer", "https://signed.example.com/secret?token=abc")
	prevReq, _ := http.NewRequest(http.MethodGet, "https://signed.example.com/secret?token=abc", nil)
	if err := client.CheckRedirect(redirectReq, []*http.Request{prevReq}); err != nil {
		t.Fatalf("valid public redirect failed: %v", err)
	}
	if got := redirectReq.Header.Get("Referer"); got != "" {
		t.Fatalf("expected Referer to be stripped on redirect, got %q", got)
	}
	httpRedirectReq, _ := http.NewRequest(http.MethodGet, "http://cdn.example.com/downgrade.png", nil)
	if err := client.CheckRedirect(httpRedirectReq, []*http.Request{prevReq}); err == nil {
		t.Fatal("expected redirect to HTTP URL to be rejected")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRemoteAttachmentFetchValidPublicHost(t *testing.T) {
	prev := remoteFetchClientOverride
	remoteFetchClientOverride = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(strings.NewReader("png-bytes")),
			}, nil
		}),
	}
	t.Cleanup(func() { remoteFetchClientOverride = prev })

	block, err := acpImageBlock("https://example.com/diagram.png")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if block["type"] != "image" || block["mimeType"] != "image/png" {
		t.Fatalf("unexpected block: %+v", block)
	}
}

func TestACPMediaSniffingAndStaging(t *testing.T) {
	workdir := t.TempDir()
	keep := map[string]bool{}
	t.Cleanup(func() {
		var paths []string
		for p := range keep {
			paths = append(paths, p)
		}
		releaseMediaRefs(paths)
	})

	// PCM16 wraps as WAV; m4a labeled as mp3 sniffs as audio/mp4.
	pcmBlock, label, err := acpMediaBlock(map[string]any{
		"type":        "input_audio",
		"input_audio": map[string]any{"data": base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4}), "format": "pcm16"},
	}, workdir, keep)
	if err != nil || label != "audio" || pcmBlock["mimeType"] != "audio/wav" {
		t.Fatalf("pcm16: block=%+v label=%q err=%v", pcmBlock, label, err)
	}
	wavPath := strings.TrimPrefix(pcmBlock["uri"].(string), "file://")
	wavBytes, err := os.ReadFile(wavPath)
	if err != nil || !bytes.HasPrefix(wavBytes, []byte("RIFF")) {
		t.Fatalf("staged WAV missing RIFF header: %v", err)
	}

	m4aHeader := append([]byte{0, 0, 0, 24}, []byte("ftypM4A ")...)
	m4aBlock, _, err := acpMediaBlock(map[string]any{
		"type":        "input_audio",
		"input_audio": map[string]any{"data": base64.StdEncoding.EncodeToString(m4aHeader), "format": "mp3"},
	}, workdir, keep)
	if err != nil || m4aBlock["mimeType"] != "audio/mp4" {
		t.Fatalf("m4a sniff failed: block=%+v err=%v", m4aBlock, err)
	}

	pdfBlock, label, err := acpMediaBlock(map[string]any{
		"type": "file",
		"file": map[string]any{"filename": "doc.pdf", "file_data": base64.StdEncoding.EncodeToString([]byte("%PDF-1.4\n"))},
	}, workdir, keep)
	if err != nil || label != "file" || pdfBlock["mimeType"] != "application/pdf" {
		t.Fatalf("pdf: block=%+v label=%q err=%v", pdfBlock, label, err)
	}
}

func TestACPMediaCacheEviction(t *testing.T) {
	dir := t.TempDir()
	mediaDir := filepath.Join(dir, acpMediaDir)
	if err := os.MkdirAll(mediaDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Expired file (> TTL) and stale staging media-*.tmp file are evicted when staging a new file.
	expired := filepath.Join(mediaDir, "old.pdf")
	staleTmp := filepath.Join(mediaDir, "media-orphan.tmp")
	finalTmp := filepath.Join(mediaDir, "payload.tmp")
	_ = os.WriteFile(expired, []byte("old"), 0o600)
	_ = os.WriteFile(staleTmp, []byte("tmp"), 0o600)
	_ = os.WriteFile(finalTmp, []byte("final-tmp"), 0o600)
	oldTime := time.Now().Add(-acpMediaCacheTTL - time.Hour)
	_ = os.Chtimes(expired, oldTime, oldTime)
	_ = os.Chtimes(staleTmp, oldTime, oldTime)
	recentOld := time.Now().Add(-time.Hour)
	_ = os.Chtimes(finalTmp, recentOld, recentOld)

	// Create acpMediaCacheMaxFiles extra files so count eviction triggers too.
	for i := 0; i < acpMediaCacheMaxFiles+2; i++ {
		p := filepath.Join(mediaDir, fmt.Sprintf("extra-%03d.bin", i))
		_ = os.WriteFile(p, []byte("x"), 0o600)
		ts := time.Now().Add(-time.Duration(i+1) * time.Second)
		_ = os.Chtimes(p, ts, ts)
	}

	kept, err := stageMedia([]byte("%PDF-1.7\nfresh"), "application/pdf", "fresh.pdf", dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseMediaRefs([]string{kept})
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("kept file missing: %v", err)
	}
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatal("expired file was not evicted")
	}
	if _, err := os.Stat(staleTmp); !os.IsNotExist(err) {
		t.Fatal("stale media-*.tmp file was not evicted")
	}
	if _, err := os.Stat(finalTmp); !os.IsNotExist(err) {
		t.Fatal("oldest non-staging .tmp file should be counted and evicted cleanly")
	}
	entries, _ := os.ReadDir(mediaDir)
	if len(entries) > acpMediaCacheMaxFiles {
		t.Fatalf("expected <= %d files after eviction, got %d", acpMediaCacheMaxFiles, len(entries))
	}
}

func TestACPPlanSessionReuseSkipsHistoricalAttachmentFetch(t *testing.T) {
	setupFakeACP(t)
	cfg := currentConfig()
	opts := agyOptions{NativeModel: "gemini-4-hydrogen", ReasoningEffort: "xhigh"}

	turn1Payload := []byte(`{"model":"agy/gemini-4-hydrogen:xhigh","messages":[
		{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://expired.example.com/gone.png"}}]}
	]}`)
	chat1, err := decodeChatRequest(turn1Payload)
	if err != nil {
		t.Fatal(err)
	}
	fakeClient := &acpClient{dead: make(chan struct{})}
	plan1 := &acpPlan{scope: convScope(cfg, opts), tools: chat1.Tools, messages: chat1.Messages, chatReq: chat1, opts: opts, cfg: cfg}
	plan1.retainConv(&acpConv{client: fakeClient, sessionID: "sess-hist"}, "saw it", nil, false)
	t.Cleanup(dropAllConvs)

	// Subsequent turn includes the old expired URL in history: planACP must hit reuse without fetching it.
	prev := remoteFetchClientOverride
	remoteFetchClientOverride = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("unexpected historical fetch for %s", r.URL.String())
		}),
	}
	t.Cleanup(func() { remoteFetchClientOverride = prev })

	turn2Req := rpcExecutorRequest{
		Model: "agy/gemini-4-hydrogen:xhigh",
		Payload: []byte(`{"model":"agy/gemini-4-hydrogen:xhigh","messages":[
			{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://expired.example.com/gone.png"}}]},
			{"role":"assistant","content":"saw it"},
			{"role":"user","content":"follow up"}
		]}`),
	}
	plan2, err := planACP(turn2Req, opts, cfg)
	if err != nil {
		t.Fatalf("planACP failed on cached session with expired historical URL: %v", err)
	}
	if plan2.reuse == nil || plan2.reuse.sessionID != "sess-hist" {
		t.Fatalf("expected retained session reuse, got %+v", plan2.reuse)
	}
}

func TestACPResolveSessionWorkdirAndSetModelErrorClassification(t *testing.T) {
	wd := resolveSessionWorkdir("")
	if !filepath.IsAbs(wd) {
		t.Fatalf("resolveSessionWorkdir(\"\") = %q, want absolute path", wd)
	}
	if !isACPModelRejection(&acpRPCError{Code: -32602, Message: "invalid params"}) {
		t.Fatal("expected -32602 to classify as model rejection")
	}
	if isACPModelRejection(&acpRPCError{Code: -32601, Message: "method not found"}) {
		t.Fatal("-32601 method not found must not classify as model_not_found")
	}
	if isACPModelRejection(&acpRPCError{Code: -32603, Message: "rate limit exceeded"}) {
		t.Fatal("-32603 rate limit must not classify as model_not_found")
	}
}

func TestACPFollowUpMediaFailureKeepsRetainedSessionAndWorkdirScope(t *testing.T) {
	setupFakeACP(t)
	cfgA := currentConfig()
	cfgA.Workdir = t.TempDir()
	cfgB := cfgA
	cfgB.Workdir = t.TempDir()
	opts := agyOptions{NativeModel: "gemini-4-hydrogen", ReasoningEffort: "xhigh"}

	if convScope(cfgA, opts) == convScope(cfgB, opts) {
		t.Fatal("expected convScope to isolate conversations across different workdirs")
	}

	turn1Payload := []byte(`{"model":"agy/gemini-4-hydrogen:xhigh","messages":[
		{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://expired.example.com/gone.png"}}]}
	]}`)
	chat1, err := decodeChatRequest(turn1Payload)
	if err != nil {
		t.Fatal(err)
	}
	var closeBuf bytes.Buffer
	fakeClient := &acpClient{dead: make(chan struct{}), in: nopWriteCloserBuf{&closeBuf}}
	plan1 := &acpPlan{scope: convScope(cfgA, opts), tools: chat1.Tools, messages: chat1.Messages, chatReq: chat1, opts: opts, cfg: cfgA}
	plan1.retainConv(&acpConv{client: fakeClient, sessionID: "sess-retain"}, "saw it", nil, false)
	t.Cleanup(dropAllConvs)

	// Under cfgB (different workdir), lookup must miss.
	turn2DifferentWorkdir := rpcExecutorRequest{
		Model: "agy/gemini-4-hydrogen:xhigh",
		Payload: []byte(`{"model":"agy/gemini-4-hydrogen:xhigh","messages":[
			{"role":"user","content":"look"},
			{"role":"assistant","content":"saw it"},
			{"role":"user","content":"follow up"}
		]}`),
	}
	if p, err := planACP(turn2DifferentWorkdir, opts, cfgB); err != nil || p.reuse != nil {
		t.Fatalf("expected workdir miss without reuse, got p=%+v err=%v", p, err)
	}

	// Follow-up media fetch fails (503) under cfgA: must NOT close sess-retain or re-fetch expired historical URL.
	prev := remoteFetchClientOverride
	allowFollowUp := false
	remoteFetchClientOverride = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Host, "expired.example.com") {
				return nil, fmt.Errorf("unexpected historical fetch for %s", r.URL.String())
			}
			if !allowFollowUp {
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Header:     http.Header{},
					Body:       io.NopCloser(strings.NewReader("unavailable")),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(strings.NewReader("ok-img")),
			}, nil
		}),
	}
	t.Cleanup(func() { remoteFetchClientOverride = prev })

	turn2Req := rpcExecutorRequest{
		Model: "agy/gemini-4-hydrogen:xhigh",
		Payload: []byte(`{"model":"agy/gemini-4-hydrogen:xhigh","messages":[
			{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://expired.example.com/gone.png"}}]},
			{"role":"assistant","content":"saw it"},
			{"role":"user","content":[{"type":"text","text":"now this"},{"type":"image_url","image_url":{"url":"https://cdn.example.com/new.png"}}]}
		]}`),
	}
	if _, err := planACP(turn2Req, opts, cfgA); err == nil {
		t.Fatal("expected follow-up attachment failure")
	}
	if closeBuf.Len() != 0 {
		t.Fatalf("retained session must stay open on follow-up failure, got close write: %q", closeBuf.String())
	}

	// Subsequent retry when attachment becomes available must still reuse sess-retain.
	allowFollowUp = true
	plan2, err := planACP(turn2Req, opts, cfgA)
	if err != nil {
		t.Fatalf("expected retry to succeed: %v", err)
	}
	if plan2.reuse == nil || plan2.reuse.sessionID != "sess-retain" {
		t.Fatalf("expected retained session sess-retain on retry, got %+v", plan2.reuse)
	}
}

func TestACPMessageContentReleasesEarlierMediaOnInvalidBlockTypeAndVideoStringURL(t *testing.T) {
	workdir := t.TempDir()
	keep := map[string]bool{}
	videoBlock, label, err := acpMediaBlock(map[string]any{
		"type":      "input_video",
		"video_url": "data:video/mp4;base64," + base64.StdEncoding.EncodeToString([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'm', 'p', '4', '2'}),
	}, workdir, keep)
	if err != nil || label != "video" || videoBlock["type"] != "resource_link" {
		t.Fatalf("expected string video_url to stage as resource_link, got block=%+v label=%q err=%v", videoBlock, label, err)
	}
	releaseMediaRefs(mediaPathsFromBlocks([]acpPromptBlock{videoBlock}))

	// Invalid content in message 2 must release media staged in message 1.
	msgs := []chatMessage{
		{Role: "user", Content: json.RawMessage(fmt.Sprintf(`[{"type":"input_audio","input_audio":{"data":%q,"format":"wav"}}]`, base64.StdEncoding.EncodeToString([]byte("RIFF\x00\x00\x00\x00WAVEfmt "))))},
		{Role: "user", Content: json.RawMessage(`{"unexpected":"object"}`)},
	}
	var attachments []acpPromptBlock
	_, err = buildFollowUpPrompt(msgs, false, workdir, &attachments)
	if err == nil {
		t.Fatal("expected error for object content")
	}
	mediaCacheMu.Lock()
	refs := len(activeMediaRef)
	mediaCacheMu.Unlock()
	if refs != 0 {
		t.Fatalf("expected all active media refs released on error, got %d remaining", refs)
	}
}

func TestACPProtectedMediaCountLimitAndWriteCancellationAndTempShutdown(t *testing.T) {
	dir := t.TempDir()
	requestKeep := map[string]bool{}
	t.Cleanup(func() {
		var paths []string
		for p := range requestKeep {
			paths = append(paths, p)
		}
		releaseMediaRefs(paths)
	})
	for i := 0; i < acpMediaCacheMaxFiles; i++ {
		p, err := stageMedia([]byte(fmt.Sprintf("file-%d", i)), "text/plain", fmt.Sprintf("f-%d.txt", i), dir, requestKeep)
		if err != nil {
			t.Fatalf("unexpected error staging file %d: %v", i, err)
		}
		requestKeep[p] = true
	}
	_, err := stageMedia([]byte("overflow-file"), "text/plain", "overflow.txt", dir, requestKeep)
	var pe *pluginError
	if !errors.As(err, &pe) || pe.HTTPStatus != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 RequestEntityTooLarge for >acpMediaCacheMaxFiles protected files, got %v", err)
	}

	// Pre-enqueue ctx cancellation must NOT shut down the resident client.
	client := &acpClient{
		writeCh:  make(chan acpWriteReq),
		pending:  map[int64]chan acpReply{},
		sessions: map[string]*acpSink{},
		dead:     make(chan struct{}),
		in:       nopWriteCloser{},
		cmd:      &exec.Cmd{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.call(ctx, "session/prompt", map[string]any{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if !client.alive() {
		t.Fatal("client must stay alive when ctx expires before enqueue")
	}

	// Temporary discovery clients tracked in acpPool.temps must be closed by retireAllACP.
	resetACPQuiesceForTest()
	t.Cleanup(resetACPQuiesceForTest)
	temp := &acpClient{
		dead: make(chan struct{}),
		in:   nopWriteCloser{},
		cmd:  &exec.Cmd{},
	}
	acpPool.Lock()
	acpPool.temps[temp] = struct{}{}
	acpPool.Unlock()
	retireAllACP()
	if temp.alive() {
		t.Fatal("expected retireAllACP to shut down tracked temporary discovery client")
	}
}

func TestACPMaxPromptActionErrorReturns413AndRestoresSession(t *testing.T) {
	setConfig(pluginConfig{BinaryPath: "agy", Mode: modeACP, MaxPromptBytes: 20000, MaxPromptAction: promptActionError})
	t.Cleanup(func() {
		dropAllConvs()
		setConfig(defaultConfig())
	})
	cfg := currentConfig()
	opts := agyOptions{NativeModel: "gemini-4-hydrogen"}

	// 1. Fresh request exceeding MaxPromptBytes rejects with 413 request_scoped.
	blob := strings.Repeat("x", 30000)
	fullReq := rpcExecutorRequest{
		Model:   "agy/gemini-4-hydrogen",
		Payload: []byte(`{"model":"agy/gemini-4-hydrogen","messages":[{"role":"user","content":"` + blob + `"}]}`),
	}
	_, err := planACP(fullReq, opts, cfg)
	var pe *pluginError
	if !errors.As(err, &pe) || pe.HTTPStatus != http.StatusRequestEntityTooLarge || pe.Code != errorCodeRequestScoped {
		t.Fatalf("expected 413 %s for oversized ACP full prompt, got %v", errorCodeRequestScoped, err)
	}

	// 2. Reused session with oversized follow-up restores the session and rejects with 413.
	priorMsgs := []chatMessage{
		{Role: "user", Content: json.RawMessage(`"hi"`)},
		{Role: "assistant", Content: json.RawMessage(`"hello"`)},
	}
	client := &acpClient{
		dead: make(chan struct{}),
		in:   nopWriteCloser{},
		cmd:  &exec.Cmd{},
	}
	key := convKey(convScope(cfg, opts), nil, priorMsgs)
	putConv(key, &acpConv{client: client, sessionID: "sess-1"})

	followReq := rpcExecutorRequest{
		Model:   "agy/gemini-4-hydrogen",
		Payload: []byte(`{"model":"agy/gemini-4-hydrogen","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"` + blob + `"}]}`),
	}
	_, err = planACP(followReq, opts, cfg)
	if !errors.As(err, &pe) || pe.HTTPStatus != http.StatusRequestEntityTooLarge || pe.Code != errorCodeRequestScoped {
		t.Fatalf("expected 413 %s for oversized ACP follow-up, got %v", errorCodeRequestScoped, err)
	}
	if restored := takeConv(key); restored == nil || restored.sessionID != "sess-1" {
		t.Fatalf("expected cached ACP session to be restored after 413 error, got %+v", restored)
	}
}

func TestACPSessionTTLsFollowConfig(t *testing.T) {
	setupFakeACP(t)
	t.Cleanup(dropAllConvs)
	cfg := currentConfig()
	cfg.ACPSessionIdleTTL = 12 * time.Hour
	cfg.ACPOneShotSessionTTL = 2 * time.Minute
	opts := agyOptions{NativeModel: "gemini-3.8-flash"}
	var buf bytes.Buffer
	client := &acpClient{dead: make(chan struct{}), in: nopWriteCloserBuf{&buf}}

	oneShot, err := decodeChatRequest([]byte(`{"model":"agy/gemini-3.8-flash","messages":[{"role":"user","content":"rate this"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	c1 := &acpConv{client: client, sessionID: "sess-oneshot"}
	(&acpPlan{scope: convScope(cfg, opts), messages: oneShot.Messages, chatReq: oneShot, opts: opts, cfg: cfg}).retainConv(c1, "ok", nil, false)
	if c1.ttl() != 2*time.Minute {
		t.Fatalf("tool-less turn kept for %v, want 2m", c1.ttl())
	}

	agent, err := decodeChatRequest([]byte(`{"model":"agy/gemini-3.8-flash","messages":[{"role":"user","content":"work"}],
		"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	c2 := &acpConv{client: client, sessionID: "sess-agent"}
	(&acpPlan{scope: convScope(cfg, opts), tools: agent.Tools, messages: agent.Messages, chatReq: agent, opts: opts, cfg: cfg}).retainConv(c2, "done", nil, false)
	if c2.ttl() != 12*time.Hour {
		t.Fatalf("tool turn kept for %v, want 12h", c2.ttl())
	}
}
