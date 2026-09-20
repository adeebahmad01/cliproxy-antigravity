package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

type chatCompletionRequest struct {
	Model         string            `json:"model"`
	Messages      []chatMessage     `json:"messages"`
	Tools         []json.RawMessage `json:"tools,omitempty"`
	ToolChoice    any               `json:"tool_choice,omitempty"`
	Stream        bool              `json:"stream"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	ConversationID  string `json:"conversation_id,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Name       string          `json:"name,omitempty"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
}

type openAIToolCall struct {
	Index    int                    `json:"index,omitempty"`
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function openAIFunctionCallDesc `json:"function"`
}

type openAIFunctionCallDesc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolCallCandidate struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
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

func buildPrompt(raw []byte, conversationID string, workdir string) (string, error) {
	req, err := decodeChatRequest(raw)
	if err != nil {
		return "", err
	}

	convID := strings.TrimSpace(conversationID)
	if convID == "" {
		convID = strings.TrimSpace(req.ConversationID)
		if convID == "" {
			convID = strings.TrimSpace(req.SessionID)
		}
	}

	messages := req.Messages
	if convID != "" {
		// When resuming an existing conversation, only include messages after the last
		// assistant response to avoid duplicating history already stored in agy's session.
		lastAssistantIdx := -1
		for i := len(messages) - 1; i >= 0; i-- {
			if strings.EqualFold(strings.TrimSpace(messages[i].Role), "assistant") {
				lastAssistantIdx = i
				break
			}
		}
		if lastAssistantIdx >= 0 && lastAssistantIdx < len(messages)-1 {
			messages = messages[lastAssistantIdx+1:]
		}
	}

	// For a single user message in a resumed conversation without tools, pass the content directly.
	if convID != "" && len(messages) == 1 && (strings.TrimSpace(messages[0].Role) == "" || strings.EqualFold(strings.TrimSpace(messages[0].Role), "user")) && len(req.Tools) == 0 && messages[0].ToolCallID == "" && len(messages[0].ToolCalls) == 0 {
		content, err := messageContentText(messages[0].Content, workdir)
		if err != nil {
			return "", pError("unsupported_content", fmt.Sprintf("message: %v", err), http.StatusBadRequest)
		}
		return content, nil
	}

	var b strings.Builder
	if convID == "" {
		b.WriteString("You are answering a request forwarded through an OpenAI-compatible chat-completions bridge to the official Antigravity CLI.\n")
		b.WriteString("Follow the system and developer instructions in the conversation below, then answer the latest user request. Return only the assistant response.\n\n")
	}

	for i, msg := range messages {
		role := strings.TrimSpace(strings.ToLower(msg.Role))
		if role == "" {
			role = "user"
		}
		content, err := messageContentText(msg.Content, workdir)
		if err != nil {
			return "", pError("unsupported_content", fmt.Sprintf("message %d: %v", i+1, err), http.StatusBadRequest)
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
			b.WriteString("Prior assistant tool calls:\n")
			b.Write(msg.ToolCalls)
			b.WriteByte('\n')
		}
	}

	if len(req.Tools) > 0 {
		b.WriteString("\n[SYSTEM INSTRUCTION: CLIENT-SIDE TOOL CALLING]\n")
		b.WriteString("The client application has provided the following tools:\n")
		for _, tool := range req.Tools {
			b.Write(tool)
			b.WriteByte('\n')
		}
		b.WriteString("\nIf a client tool call is needed, reply ONLY with a JSON block in this exact markdown code fence:\n")
		b.WriteString("```tool_calls\n[\n  {\n    \"name\": \"tool_name\",\n    \"arguments\": { ... }\n  }\n]\n```\n")
		b.WriteString("If no tool call is needed, provide your regular assistant response without the ```tool_calls code block.\n")
		b.WriteString("Do not execute local commands for client tools; the client application will run them and return the result.\n")
	}
	return b.String(), nil
}

func stageImage(dataURLOrPath string, workdir string) (string, error) {
	dataURLOrPath = strings.TrimSpace(dataURLOrPath)
	if dataURLOrPath == "" {
		return "", fmt.Errorf("empty image URL or path")
	}

	if strings.HasPrefix(dataURLOrPath, "file://") {
		return strings.TrimPrefix(dataURLOrPath, "file://"), nil
	}
	if filepath.IsAbs(dataURLOrPath) {
		if _, err := os.Stat(dataURLOrPath); err == nil {
			return dataURLOrPath, nil
		}
	}

	if strings.HasPrefix(dataURLOrPath, "data:image/") {
		commaIdx := strings.IndexByte(dataURLOrPath, ',')
		if commaIdx < 0 {
			return "", fmt.Errorf("invalid data URL: missing comma")
		}
		header := dataURLOrPath[:commaIdx]
		encodedData := dataURLOrPath[commaIdx+1:]

		subtype := "png"
		if strings.Contains(header, "image/jpeg") || strings.Contains(header, "image/jpg") {
			subtype = "jpg"
		} else if strings.Contains(header, "image/webp") {
			subtype = "webp"
		} else if strings.Contains(header, "image/gif") {
			subtype = "gif"
		}

		rawBytes, err := base64.StdEncoding.DecodeString(encodedData)
		if err != nil {
			return "", fmt.Errorf("failed to decode base64 image: %w", err)
		}

		cacheDir := filepath.Join(os.TempDir(), "cliproxy_images")
		if workdir != "" {
			cacheDir = filepath.Join(workdir, ".cliproxy_cache", "images")
		}
		if err := os.MkdirAll(cacheDir, 0755); err != nil {
			return "", fmt.Errorf("failed to create image cache directory: %w", err)
		}

		hash := sha256.Sum256(rawBytes)
		filename := fmt.Sprintf("img_%s.%s", hex.EncodeToString(hash[:8]), subtype)
		targetPath := filepath.Join(cacheDir, filename)

		if _, err := os.Stat(targetPath); os.IsNotExist(err) {
			if err := os.WriteFile(targetPath, rawBytes, 0644); err != nil {
				return "", fmt.Errorf("failed to write cached image: %w", err)
			}
		}
		return targetPath, nil
	}

	return dataURLOrPath, nil
}

func messageContentText(raw json.RawMessage, workdir string) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}

	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("content must be a string or an array of content blocks")
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
			imgURL := ""
			if imgMap, ok := block["image_url"].(map[string]any); ok {
				imgURL, _ = imgMap["url"].(string)
			} else if srcMap, ok := block["source"].(map[string]any); ok {
				if b64Data, ok := srcMap["data"].(string); ok {
					mediaType, _ := srcMap["media_type"].(string)
					if mediaType == "" {
						mediaType = "image/png"
					}
					imgURL = fmt.Sprintf("data:%s;base64,%s", mediaType, b64Data)
				}
			} else if strURL, ok := block["url"].(string); ok {
				imgURL = strURL
			}

			if imgURL != "" {
				stagedPath, err := stageImage(imgURL, workdir)
				if err != nil {
					return "", fmt.Errorf("staging image: %w", err)
				}
				parts = append(parts, fmt.Sprintf("[Attached image: %s (inspect this image file to answer the request)]", stagedPath))
			}
		default:
			if text, ok := block["text"].(string); ok && text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n"), nil
}

func extractToolCalls(response string) ([]openAIToolCall, bool) {
	trimmed := strings.TrimSpace(response)
	jsonBlock := ""

	// Check for ```tool_calls ... ```
	const fenceStart = "```tool_calls"
	if idx := strings.Index(trimmed, fenceStart); idx >= 0 {
		rest := trimmed[idx+len(fenceStart):]
		if endIdx := strings.Index(rest, "```"); endIdx >= 0 {
			jsonBlock = strings.TrimSpace(rest[:endIdx])
		}
	} else if strings.HasPrefix(trimmed, "```json") {
		rest := trimmed[7:]
		if endIdx := strings.Index(rest, "```"); endIdx >= 0 {
			jsonBlock = strings.TrimSpace(rest[:endIdx])
		}
	} else if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") {
		jsonBlock = trimmed
	}

	if jsonBlock == "" {
		return nil, false
	}

	var candidates []toolCallCandidate
	if err := json.Unmarshal([]byte(jsonBlock), &candidates); err != nil {
		var single toolCallCandidate
		if errSingle := json.Unmarshal([]byte(jsonBlock), &single); errSingle == nil && single.Name != "" {
			candidates = []toolCallCandidate{single}
		} else {
			return nil, false
		}
	}

	if len(candidates) == 0 {
		return nil, false
	}

	var calls []openAIToolCall
	for i, c := range candidates {
		if strings.TrimSpace(c.Name) == "" {
			continue
		}
		args := string(c.Arguments)
		if strings.TrimSpace(args) == "" || args == "null" {
			args = "{}"
		}
		calls = append(calls, openAIToolCall{
			Index: i,
			ID:    fmt.Sprintf("call_%s_%d", newCallID(), i),
			Type:  "function",
			Function: openAIFunctionCallDesc{
				Name:      c.Name,
				Arguments: args,
			},
		})
	}

	if len(calls) == 0 {
		return nil, false
	}
	return calls, true
}

func newCallID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err == nil {
		return hex.EncodeToString(buf)
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
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
	return (n + 3) / 4
}

func makeChatCompletion(requestedModel string, result agyResult) ([]byte, error) {
	model := displayModel(requestedModel)
	toolCalls, isToolCall := extractToolCalls(result.Response)

	var message map[string]any
	var finishReason string
	if isToolCall {
		finishReason = "tool_calls"
		message = map[string]any{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": toolCalls,
		}
	} else {
		finishReason = "stop"
		message = map[string]any{
			"role":    "assistant",
			"content": result.Response,
		}
	}

	payload := map[string]any{
		"id":      newCompletionID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": openAIUsage(result.Usage),
	}
	if result.ConversationID != "" {
		payload["conversation_id"] = result.ConversationID
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

func makeSSEChunk(id string, created int64, model string, delta map[string]any, finishReason *string, usage map[string]any, conversationID string) []byte {
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
	if strings.TrimSpace(conversationID) != "" {
		body["conversation_id"] = strings.TrimSpace(conversationID)
	}
	if usage != nil {
		body["choices"] = []any{}
		body["usage"] = usage
	}
	raw, _ := json.Marshal(body)
	return append(append([]byte("data: "), raw...), []byte("\n\n")...)
}
