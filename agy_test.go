package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func TestParseAgyModelLines(t *testing.T) {
	models, mapping := parseAgyModelLines(`
gemini-3.6-flash-high	Gemini 3.6 Flash (High)
gemini-3.6-flash-medium    Gemini 3.6 Flash (Medium)
* Claude Sonnet 4.6 (Thinking)
`)
	if len(models) != 4 {
		t.Fatalf("len(models) = %d, want 4: %#v", len(models), models)
	}
	if mapping["agy/gemini-3.6-flash-high"] != "gemini-3.6-flash-high" {
		t.Fatalf("unexpected mapping: %#v", mapping)
	}
	if mapping["agy/gemini-3.6-flash"] != "gemini-3.6-flash" {
		t.Fatalf("base model mapping missing: %#v", mapping)
	}
	if mapping["agy/claude-sonnet-4-6-thinking"] != "Claude Sonnet 4.6 (Thinking)" {
		t.Fatalf("legacy label mapping missing: %#v", mapping)
	}
}

func TestAgyArgs(t *testing.T) {
	cfg := defaultConfig()
	cfg.DangerouslySkipPermissions = true
	cfg.Sandbox = true
	opts := agyOptions{
		NativeModel:     "gemini-3.8-flash",
		OutputFormat:    "stream-json",
		ConversationID:  "test-conv-123",
		ReasoningEffort: "high",
	}
	got := agyArgs(cfg, opts)
	want := []string{
		"--output-format", "stream-json",
		"--print-timeout", "30m",
		"--dangerously-skip-permissions",
		"--sandbox",
		"--model", "gemini-3.8-flash",
		"--conversation", "test-conv-123",
		"--effort", "high",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("agyArgs() = %#v, want %#v", got, want)
	}
}

func TestAgyArgsResolvesConflict(t *testing.T) {
	cfg := defaultConfig()
	opts := agyOptions{
		NativeModel:     "gemini-3.8-flash-high",
		ReasoningEffort: "low",
	}
	got := agyArgs(cfg, opts)
	want := []string{
		"--output-format", "json",
		"--print-timeout", "30m",
		"--model", "gemini-3.8-flash",
		"--effort", "low",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("agyArgs() = %#v, want %#v", got, want)
	}
}

func TestAgyArgsClaudeOmitsEffort(t *testing.T) {
	cfg := defaultConfig()
	opts := agyOptions{
		NativeModel:     "claude-sonnet-4-6",
		ReasoningEffort: "high",
	}
	got := agyArgs(cfg, opts)
	want := []string{
		"--output-format", "json",
		"--print-timeout", "30m",
		"--model", "claude-sonnet-4-6",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("agyArgs() = %#v, want %#v", got, want)
	}
}

func TestResolveExecutionOptions(t *testing.T) {
	cfg := defaultConfig()
	cfg.DefaultReasoningEffort = "low"

	// 1. From payload JSON
	payload, _ := json.Marshal(map[string]any{
		"model":            "agy/default",
		"reasoning_effort": "medium",
		"conversation_id":  "conv-from-payload",
	})
	req := rpcExecutorRequest{
		Model:   "agy/default",
		Payload: payload,
	}
	opts := resolveExecutionOptions(req, cfg)
	if opts.ReasoningEffort != "medium" {
		t.Fatalf("expected effort 'medium', got %q", opts.ReasoningEffort)
	}
	if opts.ConversationID != "conv-from-payload" {
		t.Fatalf("expected conversation 'conv-from-payload', got %q", opts.ConversationID)
	}

	// 2. From model suffix: e.g. agy/default:high
	req2 := rpcExecutorRequest{
		Model:   "agy/default:high",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}
	opts2 := resolveExecutionOptions(req2, cfg)
	if opts2.ReasoningEffort != "high" {
		t.Fatalf("expected effort 'high' from model suffix, got %q", opts2.ReasoningEffort)
	}

	// 3. From headers
	headers := http.Header{}
	headers.Set("X-AGY-Conversation-ID", "conv-from-header")
	headers.Set("X-AGY-Effort", "low")
	req3 := rpcExecutorRequest{
		Model:   "agy/default",
		Headers: headers,
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}
	opts3 := resolveExecutionOptions(req3, cfg)
	if opts3.ConversationID != "conv-from-header" {
		t.Fatalf("expected conversation 'conv-from-header', got %q", opts3.ConversationID)
	}
	if opts3.ReasoningEffort != "low" {
		t.Fatalf("expected effort 'low', got %q", opts3.ReasoningEffort)
	}

	// 4. Built-in alias: antigravity/gemini-3.8-flash-high splits suffix into high effort and base model gemini-3.8-flash
	req4 := rpcExecutorRequest{
		Model:   "antigravity/gemini-3.8-flash-high",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}
	opts4 := resolveExecutionOptions(req4, cfg)
	if opts4.ReasoningEffort != "high" {
		t.Fatalf("expected implicit high effort from gemini-3.8-flash-high, got %q", opts4.ReasoningEffort)
	}
	if opts4.NativeModel != "gemini-3.8-flash" {
		t.Fatalf("expected native model gemini-3.8-flash, got %q", opts4.NativeModel)
	}

	// 5. Explicit model alias (gemini-3.8-flash-high) splits into base model and high effort, ignoring payload low effort
	req5 := rpcExecutorRequest{
		Model:   "agy/gemini-3.8-flash-high",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`),
	}
	opts5 := resolveExecutionOptions(req5, cfg)
	if opts5.ReasoningEffort != "high" {
		t.Fatalf("expected model alias to take priority for effort, got %q", opts5.ReasoningEffort)
	}
	if opts5.NativeModel != "gemini-3.8-flash" {
		t.Fatalf("expected native model gemini-3.8-flash, got %q", opts5.NativeModel)
	}

	// 6. Colon override on model with effort suffix: gemini-3.8-flash-high:low overrides effort to low and strips -high
	req6 := rpcExecutorRequest{
		Model:   "agy/gemini-3.8-flash-high:low",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}
	opts6 := resolveExecutionOptions(req6, cfg)
	if opts6.ReasoningEffort != "low" {
		t.Fatalf("expected colon override to set effort to 'low', got %q", opts6.ReasoningEffort)
	}
	if opts6.NativeModel != "gemini-3.8-flash" {
		t.Fatalf("expected native model gemini-3.8-flash after stripping conflicting suffix, got %q", opts6.NativeModel)
	}

	// 7. Base model gemini-3.8-flash with payload reasoning_effort
	req7 := rpcExecutorRequest{
		Model:   "agy/gemini-3.8-flash",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`),
	}
	opts7 := resolveExecutionOptions(req7, cfg)
	if opts7.ReasoningEffort != "low" {
		t.Fatalf("expected effort 'low' from payload, got %q", opts7.ReasoningEffort)
	}
	if opts7.NativeModel != "gemini-3.8-flash" {
		t.Fatalf("expected native model gemini-3.8-flash, got %q", opts7.NativeModel)
	}

	// 8. Base model without any effort provided falls back to model default (high)
	req8 := rpcExecutorRequest{
		Model:   "agy/gemini-3.8-flash",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}
	opts8 := resolveExecutionOptions(req8, defaultConfig())
	if opts8.ReasoningEffort != "high" {
		t.Fatalf("expected fallback effort 'high' for base gemini model, got %q", opts8.ReasoningEffort)
	}
	if opts8.NativeModel != "gemini-3.8-flash" {
		t.Fatalf("expected native model gemini-3.8-flash, got %q", opts8.NativeModel)
	}

	// 9. Claude model does not support effort and effort is cleared
	req9 := rpcExecutorRequest{
		Model:   "agy/claude-sonnet-4-6",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`),
	}
	opts9 := resolveExecutionOptions(req9, cfg)
	if opts9.ReasoningEffort != "" {
		t.Fatalf("expected effort to be cleared for claude model, got %q", opts9.ReasoningEffort)
	}
	if opts9.NativeModel != "claude-sonnet-4-6" {
		t.Fatalf("expected native model claude-sonnet-4-6, got %q", opts9.NativeModel)
	}
}

func TestParseAgyJSONOutputUsesTerminalLine(t *testing.T) {
	got, err := parseAgyJSONOutput([]byte("diagnostic\n{\"status\":\"SUCCESS\",\"response\":\"ok\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "SUCCESS" || got.Response != "ok" {
		t.Fatalf("unexpected result: %#v", got)
	}
}
