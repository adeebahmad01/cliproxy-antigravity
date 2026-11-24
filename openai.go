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
	conversationFields
	ReplaySummary string `json:"agy_replay_summary,omitempty"`
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
		return req, pError(errorCodeRequestScoped, "chat-completions payload is empty", http.StatusBadRequest)
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, pError(errorCodeRequestScoped, "invalid chat-completions JSON: "+err.Error(), http.StatusBadRequest)
	}
	if len(req.Messages) == 0 {
		return req, pError(errorCodeRequestScoped, "messages must contain at least one message", http.StatusBadRequest)
	}
	return req, nil
}

func addHandoffContext(req *chatCompletionRequest) error {
	if summary := strings.TrimSpace(req.ReplaySummary); summary != "" {
		content, _ := json.Marshal("Summary of the prior conversation:\n" + summary)
		req.Messages = append([]chatMessage{{Role: "user", Content: content}}, req.Messages...)
		return nil
	}
	for _, msg := range req.Messages[:len(req.Messages)-1] {
		if strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") {
			return nil
		}
	}
	return pError(errorCodeRequestScoped, "Continuing on another account requires earlier chat messages or an agy_replay_summary, not just the latest turn.", http.StatusBadRequest)
}

func buildChatPrompt(req chatCompletionRequest, convID, workdir string) (string, error) {
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
			return "", pError(errorCodeRequestScoped, fmt.Sprintf("message: %v", err), http.StatusBadRequest)
		}
		return content, nil
	}

	var b strings.Builder
	if convID == "" {
		b.WriteString("You are answering a request forwarded through an OpenAI-compatible chat-completions bridge to the official Antigravity CLI.\n")
		b.WriteString("Follow the system and developer instructions in the conversation below, then answer the latest user request. Return only the assistant response.\n")
		b.WriteString("You have no filesystem, shell, network, or editor tools for this request: do not read files, run commands, or inspect the environment. Answer from the conversation text alone, and never leave the reply empty.\n\n")
	}

	// The client-tools contract goes first: agy caps the inline prompt and truncates
	// from the tail, so a trailing instruction block is the first thing to be lost.
	if len(req.Tools) > 0 {
		writeToolInstruction(&b, req.Tools)
	}

	for i, msg := range messages {
		role := strings.TrimSpace(strings.ToLower(msg.Role))
		if role == "" {
			role = "user"
		}
		content, err := messageContentText(msg.Content, workdir)
		if err != nil {
			return "", pError(errorCodeRequestScoped, fmt.Sprintf("message %d: %v", i+1, err), http.StatusBadRequest)
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

	return b.String(), nil
}

// writeToolInstruction renders the client-side tool calling contract.
func writeToolInstruction(b *strings.Builder, tools []json.RawMessage) {
	b.WriteString("[SYSTEM INSTRUCTION: CLIENT-SIDE TOOL CALLING]\n")
	b.WriteString("The client application has provided the following tools:\n")
	for _, tool := range tools {
		b.Write(tool)
		b.WriteByte('\n')
	}
	if names := toolNamesFromSpecs(tools); len(names) > 0 {
		b.WriteString("\nCall tools by these exact names (case-sensitive), never a paraphrase such as \"Agent\" for \"subagent\": ")
		b.WriteString(strings.Join(names, ", "))
		b.WriteString("\n")
	}
	b.WriteString("\nIf a client tool call is needed, reply ONLY with a JSON block in this exact markdown code fence:\n")
	b.WriteString("```tool_calls\n[\n  {\n    \"name\": \"tool_name\",\n    \"arguments\": { ... }\n  }\n]\n```\n")
	b.WriteString("If no tool call is needed, provide your regular assistant response without the ```tool_calls code block.\n")
	b.WriteString("Do not execute local commands for client tools; the client application will run them and return the result.\n\n")
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
		if err := os.MkdirAll(cacheDir, 0700); err != nil {
			return "", fmt.Errorf("failed to create image cache directory: %w", err)
		}

		hash := sha256.Sum256(rawBytes)
		filename := fmt.Sprintf("img_%s.%s", hex.EncodeToString(hash[:8]), subtype)
		targetPath := filepath.Join(cacheDir, filename)

		if _, err := os.Stat(targetPath); os.IsNotExist(err) {
			if err := os.WriteFile(targetPath, rawBytes, 0600); err != nil {
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

// declaredToolNames lists the tool names the client advertised. Bare JSON tool
// calls are only recovered when the request actually declared tools, so ordinary
// prose that happens to start with "[" is never mistaken for a tool call.
func declaredToolNames(payload []byte) []string {
	var req struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil
	}
	return toolNamesFromSpecs(req.Tools)
}

// toolNamesFromSpecs extracts the advertised function names from tools[].
func toolNamesFromSpecs(tools []json.RawMessage) []string {
	names := make([]string, 0, len(tools))
	for _, raw := range tools {
		var tool struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &tool); err == nil && strings.TrimSpace(tool.Function.Name) != "" {
			names = append(names, tool.Function.Name)
		}
	}
	return names
}

// jsonValueEnd returns the index just past the JSON object or array that starts
// at start, honouring strings and escapes. Tool arguments often embed markdown
// fences or braces, so the first "```" after the opening fence is not a safe
// terminator.
func jsonValueEnd(s string, start int) int {
	if start < 0 || start >= len(s) {
		return -1
	}
	var open, close byte
	switch s[start] {
	case '{':
		open, close = '{', '}'
	case '[':
		open, close = '[', ']'
	default:
		return -1
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// payloadAfterFence returns the JSON payload that starts inside the fence whose
// body begins at start. A fence that closes before any JSON container means the
// block has no payload ("```json null```" followed by prose), so the search stops
// there instead of picking up whatever follows.
func payloadAfterFence(text string, start int) string {
	body := text[start:]
	brace := strings.IndexAny(body, "[{")
	fence := strings.Index(body, "```")
	if fence >= 0 && (brace < 0 || fence < brace) {
		return ""
	}
	if brace >= 0 {
		if end := jsonValueEnd(body, brace); end > 0 {
			return strings.TrimSpace(body[brace:end])
		}
	}
	return ""
}

// fencedToolCallPayloads returns fenced candidates in priority order: a leading
// ```json fence first, then a ```tool_calls block. A ```tool_calls marker inside
// a string argument of the leading payload would otherwise become the start
// point, while a leading example block followed by a genuine tool_calls fence
// still gets its call recovered by the second candidate.
func fencedToolCallPayloads(text string) []string {
	var out []string
	add := func(payload string) {
		payload = strings.TrimSpace(payload)
		if payload == "" {
			return
		}
		for _, existing := range out {
			if existing == payload {
				return
			}
		}
		out = append(out, payload)
	}
	if strings.HasPrefix(text, "```json") {
		add(payloadAfterFence(text, len("```json")))
	}
	if idx := strings.Index(text, "```tool_calls"); idx >= 0 {
		add(payloadAfterFence(text, idx+len("```tool_calls")))
	}
	return out
}

// normalizeArguments renders tool arguments as the JSON text clients expect. Some
// models double-encode them as a JSON string ({"command":"ls"} quoted), which
// makes clients parse a string instead of an object and fail the call.
func normalizeArguments(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "{}"
	}
	if strings.HasPrefix(trimmed, "\"") {
		var inner string
		if err := json.Unmarshal(raw, &inner); err == nil {
			unquoted := strings.TrimSpace(inner)
			if unquoted == "" || unquoted == "null" {
				return "{}"
			}
			if json.Valid([]byte(unquoted)) {
				return unquoted
			}
		}
	}
	return trimmed
}

func hasNamedToolCandidate(candidates []toolCallCandidate) bool {
	for _, c := range candidates {
		if strings.TrimSpace(c.Name) != "" {
			return true
		}
	}
	return false
}

// toolNameAliases maps the human names models reach for onto the tool names
// clients advertise. Keep this list explicit: guessing from substrings silently
// rewrites calls the model meant as something else.
var toolNameAliases = map[string]string{
	"agent":     "subagent",
	"task":      "subagent",
	"subagent":  "subagent",
	"delegate":  "subagent",
	"command":   "bash",
	"shell":     "bash",
	"terminal":  "bash",
	"readfile":  "read",
	"writefile": "write",
}

func lastToolNameSegment(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return ""
	}
	return strings.ToLower(trimmed[strings.LastIndexAny(trimmed, ":/.-")+1:])
}

// resolveToolName snaps a model-emitted name onto one of the advertised tools.
// Clients drop calls whose name they cannot resolve, so an unmatched "Agent" for
// an advertised "subagent" silently loses the turn. Matching is deliberately
// strict: exact, then case-insensitive, then an explicit alias, then an equal
// trailing namespace segment ("tools:subagent" -> "subagent").
func resolveToolName(name string, declared []string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || len(declared) == 0 {
		return trimmed
	}
	for _, candidate := range declared {
		if candidate == trimmed {
			return candidate
		}
	}
	lowered := strings.ToLower(trimmed)
	for _, candidate := range declared {
		if strings.ToLower(candidate) == lowered {
			return candidate
		}
	}
	if alias, ok := toolNameAliases[lowered]; ok {
		for _, candidate := range declared {
			if strings.ToLower(candidate) == alias {
				return candidate
			}
		}
	}
	segment := lastToolNameSegment(trimmed)
	if segment != "" {
		matched := ""
		for _, candidate := range declared {
			if lastToolNameSegment(candidate) == segment {
				if matched != "" {
					matched = "" // ambiguous, do not guess
					break
				}
				matched = candidate
			}
		}
		if matched != "" {
			return matched
		}
	}
	return trimmed
}

func normalizeToolCalls(calls []openAIToolCall, declared []string) []openAIToolCall {
	if len(declared) == 0 {
		return calls
	}
	for i := range calls {
		calls[i].Function.Name = resolveToolName(calls[i].Function.Name, declared)
	}
	return calls
}

func extractToolCalls(response string, allowBare bool) ([]openAIToolCall, bool) {
	trimmed := strings.TrimSpace(response)

	// Candidates are tried in order. A bare payload leads when tools were declared
	// and the response opens with a JSON container: its arguments may contain a
	// fence marker ("echo ```json"), which would otherwise be mistaken for the
	// payload terminator. The fenced form stays as fallback so a fenced block after
	// prose is still recovered.
	var blocks []string
	if allowBare && (strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{")) {
		if end := jsonValueEnd(trimmed, 0); end > 0 {
			blocks = append(blocks, strings.TrimSpace(trimmed[:end]))
		} else {
			blocks = append(blocks, trimmed)
		}
	}
	blocks = append(blocks, fencedToolCallPayloads(trimmed)...)

	var candidates []toolCallCandidate
	for _, block := range blocks {
		if block == "" {
			continue
		}
		var arr []toolCallCandidate
		if err := json.Unmarshal([]byte(block), &arr); err == nil && hasNamedToolCandidate(arr) {
			candidates = arr
			break
		}
		var single toolCallCandidate
		if errSingle := json.Unmarshal([]byte(block), &single); errSingle == nil && strings.TrimSpace(single.Name) != "" {
			candidates = []toolCallCandidate{single}
			break
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
		args := normalizeArguments(c.Arguments)
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

// emptyResponseError marks a turn where agy finished successfully without any
// assistant text. The CLI usually spent that turn running its own built-in
// tools, so the bridge has nothing to forward and an empty completion would make
// clients retry the identical request in a loop. Retryable: a fresh attempt, or
// a fallback model, can still answer.
func emptyResponseError() error {
	return pRetryableError(
		"empty_assistant_response",
		"agy returned no assistant text: it likely executed its own tools instead of answering. Retry the request or switch models.",
		http.StatusBadGateway,
	)
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

func makeChatCompletion(requestedModel string, declared []string, result agyResult) ([]byte, error) {
	model := displayModel(requestedModel)
	toolCalls, isToolCall := extractToolCalls(result.Response, len(declared) > 0)
	toolCalls = normalizeToolCalls(toolCalls, declared)
	if !isToolCall && strings.TrimSpace(result.Response) == "" {
		return nil, emptyResponseError()
	}

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

func makeChatChunk(id string, created int64, model string, delta map[string]any, finishReason *string, usage map[string]any, conversationID string) []byte {
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
	return raw
}
