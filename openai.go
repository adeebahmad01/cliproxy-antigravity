package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

type chatCompletionRequest struct {
	Model         string            `json:"model"`
	Messages      []chatMessage     `json:"messages"`
	Tools         []json.RawMessage `json:"tools"`
	Stream        bool              `json:"stream"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Name       string          `json:"name,omitempty"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
}

func decodeChatRequest(raw []byte) (chatCompletionRequest, error) {
	var req chatCompletionRequest
	if len(raw) == 0 {
		return req, pError("invalid_request", "chat-completions payload is empty", http.StatusBadRequest)
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, pError("invalid_request", "invalid chat-completions JSON: "+err.Error(), http.StatusBadRequest)
	}
	if len(req.Messages) == 0 {
		return req, pError("invalid_request", "messages must contain at least one message", http.StatusBadRequest)
	}
	return req, nil
}

func buildPrompt(raw []byte) (string, error) {
	req, err := decodeChatRequest(raw)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("You are answering a request forwarded through an OpenAI-compatible chat-completions bridge to the official Antigravity CLI.\n")
	b.WriteString("Follow the system and developer instructions in the conversation below, then answer the latest user request. Return only the assistant response.\n\n")

	for i, msg := range req.Messages {
		role := strings.TrimSpace(strings.ToLower(msg.Role))
		if role == "" {
			role = "user"
		}
		content, err := messageContentText(msg.Content)
		if err != nil {
			return "", pError("unsupported_content", fmt.Sprintf("message %d: %v", i, err), http.StatusBadRequest)
		}

		b.WriteString("--- message ")
		b.WriteString(fmt.Sprintf("%d", i+1))
		b.WriteString(" role=")
		b.WriteString(role)
		if msg.Name != "" {
			b.WriteString(" name=")
			b.WriteString(msg.Name)
		}
		if msg.ToolCallID != "" {
			b.WriteString(" tool_call_id=")
			b.WriteString(msg.ToolCallID)
		}
		b.WriteString(" ---\n")
		if content != "" {
			b.WriteString(content)
			if !strings.HasSuffix(content, "\n") {
				b.WriteByte('\n')
			}
		}
		if len(msg.ToolCalls) > 0 && string(msg.ToolCalls) != "null" && string(msg.ToolCalls) != "[]" {
			b.WriteString("Prior assistant tool calls (context only): ")
			b.Write(msg.ToolCalls)
			b.WriteByte('\n')
		}
	}

	if len(req.Tools) > 0 {
		b.WriteString("\nNote: client-provided OpenAI tool schemas are not bridged. Use Antigravity's own tools when appropriate and return a normal assistant response.\n")
	}
	return b.String(), nil
}

func messageContentText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}

	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("content must be a string or an array of text content blocks")
	}

	var parts []string
	for _, block := range blocks {
		kind, _ := block["type"].(string)
		switch kind {
		case "text", "input_text", "output_text", "":
			if text, ok := block["text"].(string); ok && text != "" {
				parts = append(parts, text)
			}
		case "image_url", "input_image", "image":
			return "", fmt.Errorf("image content is not supported in v0.1; pass text or a file path that agy can access")
		default:
			if text, ok := block["text"].(string); ok && text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n"), nil
}

func requestWantsUsage(raw []byte) bool {
	req, err := decodeChatRequest(raw)
	return err == nil && req.StreamOptions.IncludeUsage
}

func estimateTokens(text string) int {
	n := utf8.RuneCountInString(text)
	if n == 0 {
		return 0
	}
	// Deliberately labeled as an estimate: no local Antigravity tokenizer contract exists.
	return (n + 3) / 4
}

func makeChatCompletion(requestedModel string, result agyResult) ([]byte, error) {
	model := displayModel(requestedModel)
	payload := map[string]any{
		"id":      newCompletionID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": result.Response,
			},
			"finish_reason": "stop",
		}},
		"usage": openAIUsage(result.Usage),
	}
	return json.Marshal(payload)
}

func openAIUsage(usage agyUsage) map[string]any {
	total := usage.TotalTokens
	if total == 0 {
		total = usage.InputTokens + usage.OutputTokens
	}
	out := map[string]any{
		"prompt_tokens":     usage.InputTokens,
		"completion_tokens": usage.OutputTokens,
		"total_tokens":      total,
	}
	if usage.ThinkingTokens > 0 {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": usage.ThinkingTokens}
	}
	if usage.CacheReadTokens > 0 {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": usage.CacheReadTokens}
	}
	return out
}

func displayModel(model string) string {
	if strings.TrimSpace(model) == "" {
		return "agy/default"
	}
	return model
}

func newCompletionID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err == nil {
		return "chatcmpl-agy-" + hex.EncodeToString(buf)
	}
	return fmt.Sprintf("chatcmpl-agy-%d", time.Now().UnixNano())
}

func makeSSEChunk(id string, created int64, model string, delta map[string]any, finishReason *string, usage map[string]any) []byte {
	choice := map[string]any{
		"index": 0,
		"delta": delta,
	}
	if finishReason == nil {
		choice["finish_reason"] = nil
	} else {
		choice["finish_reason"] = *finishReason
	}
	body := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{choice},
	}
	if usage != nil {
		// OpenAI usage-only chunks normally have an empty choices array.
		body["choices"] = []any{}
		body["usage"] = usage
	}
	raw, _ := json.Marshal(body)
	return append(append([]byte("data: "), raw...), []byte("\n\n")...)
}
