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
	if len(models) != 3 {
		t.Fatalf("len(models) = %d, want 3: %#v", len(models), models)
	}
	if mapping["agy/gemini-3.6-flash-high"] != "gemini-3.6-flash-high" {
		t.Fatalf("unexpected mapping: %#v", mapping)
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
		NativeModel:     "gemini-x-high",
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
		"--model", "gemini-x-high",
		"--conversation", "test-conv-123",
		"--effort", "high",
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

	// 4. Built-in alias: antigravity/gemini-3.8-flash-high strips prefix and extracts high effort
	req4 := rpcExecutorRequest{
		Model:   "antigravity/gemini-3.8-flash-high",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}
	opts4 := resolveExecutionOptions(req4, cfg)
	if opts4.ReasoningEffort != "high" {
		t.Fatalf("expected implicit high effort from gemini-3.8-flash-high, got %q", opts4.ReasoningEffort)
	}
	if opts4.NativeModel != "gemini-3.8-flash-high" {
		t.Fatalf("expected native model gemini-3.8-flash-high, got %q", opts4.NativeModel)
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
