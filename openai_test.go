package main

import (
	"encoding/json"
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
	prompt, err := buildPrompt(payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"role=system", "Be concise.", "role=user", "Explain Go."} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildPromptRejectsImageContent(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]}]}`)
	if _, err := buildPrompt(payload); err == nil {
		t.Fatal("expected image-content error")
	}
}

func TestMakeChatCompletionMapsUsage(t *testing.T) {
	raw, err := makeChatCompletion("agy/test", agyResult{
		Status:   "SUCCESS",
		Response: "done",
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
	choices := body["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "done" {
		t.Fatalf("content = %#v", message["content"])
	}
}

func TestMakeSSEUsageChunkHasEmptyChoices(t *testing.T) {
	raw := makeSSEChunk("id", 1, "agy/test", map[string]any{}, nil, map[string]any{"total_tokens": 1})
	if !strings.Contains(string(raw), `"choices":[]`) || !strings.HasPrefix(string(raw), "data: ") {
		t.Fatalf("unexpected SSE: %s", raw)
	}
}
