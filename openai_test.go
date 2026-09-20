package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestBuildPromptPreservesConversationRoles(t *testing.T) {
	payload := []byte(`{
      "model":"agy/default",
      "messages":[
        {"role":"system","content":"Be concise."},
        {"role":"user","content":"Hello"},
        {"role":"assistant","content":"Hi"},
        {"role":"user","content":[{"type":"text","text":"Explain Go."}]}
      ]
    }`)
	prompt, err := buildPrompt(payload, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"role=system", "Be concise.", "role=user", "Explain Go."} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildPromptResumedConversation(t *testing.T) {
	payload := []byte(`{
      "model":"agy/default",
      "conversation_id":"conv-abc",
      "messages":[
        {"role":"user","content":"First turn"},
        {"role":"assistant","content":"First answer"},
        {"role":"user","content":"Second turn"}
      ]
    }`)
	prompt, err := buildPrompt(payload, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "First turn") {
		t.Fatalf("resumed prompt should not duplicate prior user message:\n%s", prompt)
	}
	if strings.Contains(prompt, "First answer") {
		t.Fatalf("resumed prompt should not duplicate prior assistant message:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Second turn") {
		t.Fatalf("resumed prompt missing latest user turn:\n%s", prompt)
	}
}

func TestBuildPromptStagesBase64Image(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cliproxy-img-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	fakePNG := base64.StdEncoding.EncodeToString([]byte("fake-png-content-data"))
	payload := []byte(`{
      "model":"agy/default",
      "messages":[
        {
          "role":"user",
          "content":[
            {"type":"text","text":"Look at this image"},
            {"type":"image_url","image_url":{"url":"data:image/png;base64,` + fakePNG + `"}}
          ]
        }
      ]
    }`)

	prompt, err := buildPrompt(payload, "", tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "[Attached image:") || !strings.Contains(prompt, ".png") {
		t.Fatalf("expected staged image tag in prompt, got:\n%s", prompt)
	}
}

func TestBuildPromptInjectsClientToolsInstruction(t *testing.T) {
	payload := []byte(`{
      "model":"agy/default",
      "messages":[{"role":"user","content":"What is the weather?"}],
      "tools":[
        {
          "type":"function",
          "function":{"name":"get_weather","description":"Get current weather"}
        }
      ]
    }`)

	prompt, err := buildPrompt(payload, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "[SYSTEM INSTRUCTION: CLIENT-SIDE TOOL CALLING]") {
		t.Fatalf("expected tool calling instructions in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "get_weather") {
		t.Fatalf("expected tool schema in prompt:\n%s", prompt)
	}
}

func TestExtractToolCalls(t *testing.T) {
	textWithFence := "Here is the tool call:\n```tool_calls\n[\n  {\n    \"name\": \"calc\",\n    \"arguments\": {\"expr\": \"2+2\"}\n  }\n]\n```"
	calls, ok := extractToolCalls(textWithFence)
	if !ok || len(calls) != 1 {
		t.Fatalf("failed to extract fenced tool calls: ok=%v, len=%d", ok, len(calls))
	}
	if calls[0].Function.Name != "calc" {
		t.Fatalf("call name = %q, want 'calc'", calls[0].Function.Name)
	}
	if !strings.Contains(calls[0].Function.Arguments, "2+2") {
		t.Fatalf("call args = %q", calls[0].Function.Arguments)
	}
}

func TestMakeChatCompletionMapsToolCalls(t *testing.T) {
	raw, err := makeChatCompletion("gemini-3.8-flash-high", agyResult{
		Status:   "SUCCESS",
		Response: "```tool_calls\n[{\"name\":\"search\",\"arguments\":{\"query\":\"golang\"}}]\n```",
	})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	choices := body["choices"].([]any)
	choice0 := choices[0].(map[string]any)
	if choice0["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v, want 'tool_calls'", choice0["finish_reason"])
	}
	msg := choice0["message"].(map[string]any)
	if msg["content"] != nil {
		t.Fatalf("content should be nil for tool_calls, got %v", msg["content"])
	}
	toolCalls := msg["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
}

func TestMakeChatCompletionMapsUsageAndConversation(t *testing.T) {
	raw, err := makeChatCompletion("agy/test", agyResult{
		ConversationID: "conv-xyz-99",
		Status:         "SUCCESS",
		Response:       "done",
		Usage: agyUsage{
			InputTokens: 10, OutputTokens: 5, ThinkingTokens: 2, CacheReadTokens: 3, TotalTokens: 15,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "agy/test" {
		t.Fatalf("model = %#v", body["model"])
	}
	if body["conversation_id"] != "conv-xyz-99" {
		t.Fatalf("conversation_id = %#v, want 'conv-xyz-99'", body["conversation_id"])
	}
	choices := body["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "done" {
		t.Fatalf("content = %#v", message["content"])
	}
}

func TestMakeSSEChunkIncludesConversationID(t *testing.T) {
	raw := makeSSEChunk("id", 1, "agy/test", map[string]any{"content": "hello"}, nil, nil, "conv-stream-1")
	str := string(raw)
	if !strings.Contains(str, `"conversation_id":"conv-stream-1"`) {
		t.Fatalf("expected conversation_id in SSE chunk: %s", str)
	}
}

func TestMakeSSEUsageChunkHasEmptyChoices(t *testing.T) {
	raw := makeSSEChunk("id", 1, "agy/test", map[string]any{}, nil, map[string]any{"total_tokens": 1}, "")
	if !strings.Contains(string(raw), `"choices":[]`) || !strings.HasPrefix(string(raw), "data: ") {
		t.Fatalf("unexpected SSE: %s", raw)
	}
}
