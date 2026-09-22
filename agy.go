package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

type agyUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ThinkingTokens  int `json:"thinking_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
	TotalTokens     int `json:"total_tokens"`
}

type agyResult struct {
	ConversationID  string   `json:"conversation_id"`
	Status          string   `json:"status"`
	Response        string   `json:"response"`
	Error           string   `json:"error"`
	DurationSeconds float64  `json:"duration_seconds"`
	NumTurns        int      `json:"num_turns"`
	Usage           agyUsage `json:"usage"`
}

type agyStreamEvent struct {
	Event          string `json:"event"`
	Message        string `json:"message"`
	ConversationID string `json:"conversation_id,omitempty"`
	StepUpdate     struct {
		StepIndex int    `json:"step_index"`
		StepType  string `json:"step_type"`
		State     string `json:"state"`
		TextDelta string `json:"text_delta"`
	} `json:"step_update"`
	Result *agyResult `json:"result"`

	// Some older builds can emit a terminal result without an event wrapper.
	Status   string   `json:"status"`
	Response string   `json:"response"`
	Error    string   `json:"error"`
	Usage    agyUsage `json:"usage"`
}

type agyOptions struct {
	NativeModel     string
	OutputFormat    string
	ConversationID  string
	ReasoningEffort string
	HasTools        bool
}

type processRegistry struct {
	sync.Mutex
	cmds map[*exec.Cmd]struct{}
}

var activeProcesses = processRegistry{cmds: make(map[*exec.Cmd]struct{})}

func (r *processRegistry) add(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	r.Lock()
	r.cmds[cmd] = struct{}{}
	r.Unlock()
}

func (r *processRegistry) remove(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	r.Lock()
	delete(r.cmds, cmd)
	r.Unlock()
}

func (r *processRegistry) killAll() {
	r.Lock()
	cmds := make([]*exec.Cmd, 0, len(r.cmds))
	for cmd := range r.cmds {
		cmds = append(cmds, cmd)
	}
	r.Unlock()
	for _, cmd := range cmds {
		killProcessTree(cmd)
	}
}

type catalogState struct {
	sync.Mutex
	fetchedAt  time.Time
	binaryPath string
	models     []modelInfo
	nativeByID map[string]string
}

var modelCatalog = catalogState{}

func invalidateModels() {
	modelCatalog.Lock()
	modelCatalog.fetchedAt = time.Time{}
	modelCatalog.binaryPath = ""
	modelCatalog.models = nil
	modelCatalog.nativeByID = nil
	modelCatalog.Unlock()
}

func standardBuiltinModels() []modelInfo {
	return []modelInfo{
		{
			ID:                         "agy/default",
			Object:                     "model",
			OwnedBy:                    "google-antigravity-cli",
			DisplayName:                "Antigravity CLI (default model)",
			Name:                       "default",
			Description:                "Uses the default model selected by the installed official agy CLI.",
			SupportedGenerationMethods: []string{"chat"},
			UserDefined:                true,
		},
		{
			ID:                         "gemini-3.8-flash-high",
			Object:                     "model",
			OwnedBy:                    "google-antigravity-cli",
			DisplayName:                "Gemini 3.8 Flash (High)",
			Name:                       "gemini-3.8-flash-high",
			Description:                "Standard Gemini 3.8 Flash model with high thinking effort.",
			SupportedGenerationMethods: []string{"chat"},
			UserDefined:                true,
		},
		{
			ID:                         "gemini-3.8-flash-medium",
			Object:                     "model",
			OwnedBy:                    "google-antigravity-cli",
			DisplayName:                "Gemini 3.8 Flash (Medium)",
			Name:                       "gemini-3.8-flash-medium",
			Description:                "Standard Gemini 3.8 Flash model with medium thinking effort.",
			SupportedGenerationMethods: []string{"chat"},
			UserDefined:                true,
		},
		{
			ID:                         "gemini-3.8-flash-low",
			Object:                     "model",
			OwnedBy:                    "google-antigravity-cli",
			DisplayName:                "Gemini 3.8 Flash (Low)",
			Name:                       "gemini-3.8-flash-low",
			Description:                "Standard Gemini 3.8 Flash model with low thinking effort.",
			SupportedGenerationMethods: []string{"chat"},
			UserDefined:                true,
		},
		{
			ID:                         "claude-3-7-sonnet-thought",
			Object:                     "model",
			OwnedBy:                    "google-antigravity-cli",
			DisplayName:                "Claude 3.7 Sonnet (Thinking)",
			Name:                       "claude-3-7-sonnet-thought",
			Description:                "Claude 3.7 Sonnet Thinking model via Antigravity.",
			SupportedGenerationMethods: []string{"chat"},
			UserDefined:                true,
		},
	}
}

func discoverModels(cfg pluginConfig) []modelInfo {
	modelCatalog.Lock()
	defer modelCatalog.Unlock()

	if modelCatalog.binaryPath == cfg.BinaryPath && time.Since(modelCatalog.fetchedAt) < time.Minute && len(modelCatalog.models) > 0 {
		return cloneModels(modelCatalog.models)
	}

	builtin := standardBuiltinModels()
	models := append([]modelInfo(nil), builtin...)
	nativeByID := map[string]string{
		"agy/default":               "",
		"antigravity/default":       "",
		"default":                   "",
		"gemini-3.8-flash-high":     "gemini-3.8-flash-high",
		"gemini-3.8-flash-medium":   "gemini-3.8-flash-medium",
		"gemini-3.8-flash-low":      "gemini-3.8-flash-low",
		"claude-3-7-sonnet-thought": "claude-3-7-sonnet-thought",
	}

	if err := validateBinaryPath(cfg.BinaryPath); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, cfg.BinaryPath, "models")
		configureSysProcAttr(cmd)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if cfg.Workdir != "" {
			cmd.Dir = cfg.Workdir
		}

		if err := cmd.Start(); err == nil {
			activeProcesses.add(cmd)
			_ = cmd.Wait()
			activeProcesses.remove(cmd)
			discovered, mapping := parseAgyModelLines(stdout.String())
			for _, m := range discovered {
				models = append(models, m)
			}
			for id, native := range mapping {
				nativeByID[id] = native
				// Also map unprefixed and antigravity/ prefixes
				slug := strings.TrimPrefix(id, "agy/")
				nativeByID[slug] = native
				nativeByID["antigravity/"+slug] = native
			}
		}
	}

	modelCatalog.fetchedAt = time.Now()
	modelCatalog.binaryPath = cfg.BinaryPath
	modelCatalog.models = models
	modelCatalog.nativeByID = nativeByID
	return cloneModels(models)
}

func defaultModelInfo() modelInfo {
	return modelInfo{
		ID:                         "agy/default",
		Object:                     "model",
		OwnedBy:                    "google-antigravity-cli",
		DisplayName:                "Antigravity CLI (default model)",
		Name:                       "default",
		Description:                "Uses the default model selected by the installed official agy CLI.",
		SupportedGenerationMethods: []string{"chat"},
		UserDefined:                true,
	}
}

func cloneModels(in []modelInfo) []modelInfo {
	out := make([]modelInfo, len(in))
	copy(out, in)
	for i := range out {
		out[i].SupportedGenerationMethods = append([]string(nil), out[i].SupportedGenerationMethods...)
	}
	return out
}

func parseAgyModelLines(output string) ([]modelInfo, map[string]string) {
	byID := map[string]modelInfo{}
	nativeByID := map[string]string{}

	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		line = strings.TrimSpace(strings.TrimLeft(line, "*•-"))
		if line == "" {
			continue
		}

		native := ""
		label := ""
		slug := ""
		if parts := strings.SplitN(line, "\t", 2); len(parts) == 2 {
			slug = strings.TrimSpace(parts[0])
			label = strings.TrimSpace(parts[1])
			native = slug
		} else {
			fields := strings.Fields(line)
			if len(fields) >= 2 && looksLikeModelSlug(fields[0]) {
				slug = fields[0]
				label = strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
				native = slug
			} else {
				label = line
				slug = slugifyModelLabel(label)
				native = label
			}
		}
		if slug == "" || label == "" {
			continue
		}

		id := "agy/" + slug
		if _, exists := byID[id]; exists {
			continue
		}
		byID[id] = modelInfo{
			ID:                         id,
			Object:                     "model",
			OwnedBy:                    "google-antigravity-cli",
			DisplayName:                label,
			Name:                       native,
			Description:                "Discovered from the installed official agy CLI.",
			SupportedGenerationMethods: []string{"chat"},
			UserDefined:                true,
		}
		nativeByID[id] = native
	}

	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	models := make([]modelInfo, 0, len(ids))
	for _, id := range ids {
		models = append(models, byID[id])
	}
	return models, nativeByID
}

func looksLikeModelSlug(s string) bool {
	if !strings.Contains(s, "-") || strings.ContainsAny(s, "()[]{}") {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func slugifyModelLabel(label string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(label) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func resolveNativeModel(model string, cfg pluginConfig) string {
	cleanModel := strings.TrimSpace(model)
	cleanModel = strings.TrimPrefix(cleanModel, "antigravity/")
	cleanModel = strings.TrimPrefix(cleanModel, "agy/")

	if cleanModel == "" || cleanModel == "default" {
		return ""
	}

	_ = discoverModels(cfg)
	modelCatalog.Lock()
	defer modelCatalog.Unlock()

	if native, ok := modelCatalog.nativeByID[cleanModel]; ok && native != "" {
		return native
	}
	if native, ok := modelCatalog.nativeByID["agy/"+cleanModel]; ok && native != "" {
		return native
	}
	return cleanModel
}

func resolveExecutionOptions(req rpcExecutorRequest, cfg pluginConfig) agyOptions {
	var chatReq chatCompletionRequest
	_ = json.Unmarshal(req.Payload, &chatReq)

	requestedModel := strings.TrimSpace(req.Model)
	if requestedModel == "" {
		requestedModel = strings.TrimSpace(chatReq.Model)
	}

	effort := ""
	// 1. Check explicit model suffix :low, :medium, :high
	for _, suffix := range []string{":low", ":medium", ":high"} {
		if strings.HasSuffix(strings.ToLower(requestedModel), suffix) {
			effort = strings.TrimPrefix(suffix, ":")
			requestedModel = requestedModel[:len(requestedModel)-len(suffix)]
			break
		}
	}

	// 2. Check chatReq.ReasoningEffort
	if effort == "" && chatReq.ReasoningEffort != "" {
		effort = normalizeEffort(chatReq.ReasoningEffort)
	}

	// 3. Check headers
	if effort == "" && req.Headers != nil {
		if val := req.Headers.Get("X-AGY-Effort"); val != "" {
			effort = normalizeEffort(val)
		} else if val := req.Headers.Get("X-Reasoning-Effort"); val != "" {
			effort = normalizeEffort(val)
		}
	}

	// 4. Check metadata
	if effort == "" && req.Metadata != nil {
		if val, ok := req.Metadata["reasoning_effort"].(string); ok && val != "" {
			effort = normalizeEffort(val)
		} else if val, ok := req.Metadata["effort"].(string); ok && val != "" {
			effort = normalizeEffort(val)
		}
	}

	// 5. Implicit effort from model names like gemini-3.8-flash-low / medium / high
	if effort == "" {
		lowerModel := strings.ToLower(requestedModel)
		switch {
		case strings.HasSuffix(lowerModel, "-low"):
			effort = "low"
		case strings.HasSuffix(lowerModel, "-medium"):
			effort = "medium"
		case strings.HasSuffix(lowerModel, "-high"):
			effort = "high"
		}
	}

	// 6. Fall back to config default if specified
	if effort == "" && cfg.DefaultReasoningEffort != "" {
		effort = cfg.DefaultReasoningEffort
	}

	// Conversation ID
	convID := strings.TrimSpace(chatReq.ConversationID)
	if convID == "" {
		convID = strings.TrimSpace(chatReq.SessionID)
	}
	if convID == "" && req.Headers != nil {
		if val := req.Headers.Get("X-AGY-Conversation-ID"); val != "" {
			convID = strings.TrimSpace(val)
		} else if val := req.Headers.Get("X-Conversation-ID"); val != "" {
			convID = strings.TrimSpace(val)
		} else if val := req.Headers.Get("Conversation-ID"); val != "" {
			convID = strings.TrimSpace(val)
		}
	}
	if convID == "" && req.Metadata != nil {
		if val, ok := req.Metadata["conversation_id"].(string); ok && val != "" {
			convID = strings.TrimSpace(val)
		} else if val, ok := req.Metadata["agy_conversation_id"].(string); ok && val != "" {
			convID = strings.TrimSpace(val)
		} else if val, ok := req.Metadata["session_id"].(string); ok && val != "" {
			convID = strings.TrimSpace(val)
		}
	}

	nativeModel := resolveNativeModel(requestedModel, cfg)

	return agyOptions{
		NativeModel:     nativeModel,
		ConversationID:  convID,
		ReasoningEffort: effort,
		HasTools:        len(chatReq.Tools) > 0,
	}
}

func normalizeEffort(s string) string {
	lower := strings.ToLower(strings.TrimSpace(s))
	switch lower {
	case "low", "medium", "high":
		return lower
	default:
		return ""
	}
}

func agyArgs(cfg pluginConfig, opts agyOptions) []string {
	outputFormat := opts.OutputFormat
	if outputFormat == "" {
		outputFormat = "json"
	}
	args := []string{"--output-format", outputFormat, "--print-timeout", cfg.PrintTimeout}
	if cfg.DangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	if cfg.Sandbox {
		args = append(args, "--sandbox")
	}
	if strings.TrimSpace(opts.NativeModel) != "" {
		args = append(args, "--model", opts.NativeModel)
	}
	if strings.TrimSpace(opts.ConversationID) != "" {
		args = append(args, "--conversation", strings.TrimSpace(opts.ConversationID))
	}
	if strings.TrimSpace(opts.ReasoningEffort) != "" {
		args = append(args, "--effort", strings.TrimSpace(opts.ReasoningEffort))
	}
	return args
}

func commandTimeout(cfg pluginConfig) time.Duration {
	d, err := time.ParseDuration(cfg.PrintTimeout)
	if err != nil || d <= 0 {
		d = 30 * time.Minute
	}
	return d + 30*time.Second
}

func newAgyCommand(ctx context.Context, cfg pluginConfig, opts agyOptions, prompt string) (*exec.Cmd, error) {
	if err := validateBinaryPath(cfg.BinaryPath); err != nil {
		return nil, pError("invalid_binary", err.Error(), http.StatusBadRequest)
	}
	cmd := exec.CommandContext(ctx, cfg.BinaryPath, agyArgs(cfg, opts)...)
	if cfg.Workdir != "" {
		cmd.Dir = cfg.Workdir
	}
	configureSysProcAttr(cmd)
	cmd.Stdin = strings.NewReader(prompt)
	return cmd, nil
}

func runAgyJSON(cfg pluginConfig, opts agyOptions, prompt string) (agyResult, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout(cfg))
	defer cancel()

	opts.OutputFormat = "json"
	cmd, err := newAgyCommand(ctx, cfg, opts, prompt)
	if err != nil {
		return agyResult{}, "", err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return agyResult{}, stderr.String(), classifyAgyError(err, stderr.String())
	}
	activeProcesses.add(cmd)
	errWait := cmd.Wait()
	activeProcesses.remove(cmd)

	result, errParse := parseAgyJSONOutput(stdout.Bytes())
	if errParse != nil {
		if errWait != nil {
			return agyResult{}, stderr.String(), classifyAgyError(errWait, stderr.String())
		}
		return agyResult{}, stderr.String(), pError("invalid_upstream_response", "agy returned invalid JSON: "+errParse.Error(), http.StatusBadGateway)
	}

	if !strings.EqualFold(result.Status, "SUCCESS") {
		msg := strings.TrimSpace(result.Error)
		if msg == "" {
			msg = strings.TrimSpace(stderr.String())
		}
		if msg == "" {
			msg = "Antigravity CLI ended with status " + result.Status
		}
		return result, stderr.String(), classifyAgyError(errors.New(msg), stderr.String())
	}
	if errWait != nil {
		return result, stderr.String(), classifyAgyError(errWait, stderr.String())
	}
	return result, stderr.String(), nil
}

func parseAgyJSONOutput(raw []byte) (agyResult, error) {
	trimmed := bytes.TrimSpace(raw)
	var result agyResult
	if len(trimmed) == 0 {
		return result, fmt.Errorf("empty stdout")
	}
	if json.Unmarshal(trimmed, &result) == nil && result.Status != "" {
		return result, nil
	}

	lines := bytes.Split(trimmed, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		result = agyResult{}
		if err := json.Unmarshal(line, &result); err == nil && result.Status != "" {
			return result, nil
		}
	}
	return agyResult{}, fmt.Errorf("no terminal result object found")
}

func classifyAgyError(err error, stderr string) error {
	msg := strings.TrimSpace(strings.Join([]string{errorString(err), stderr}, "\n"))
	lower := strings.ToLower(msg)
	switch {
	case errors.Is(err, exec.ErrNotFound), strings.Contains(lower, "executable file not found"), strings.Contains(lower, "no such file or directory") && strings.Contains(lower, "agy"):
		return pError("agy_not_found", "official Antigravity CLI executable was not found; install agy or set binary_path", http.StatusServiceUnavailable)
	case strings.Contains(lower, "sign in"), strings.Contains(lower, "login"), strings.Contains(lower, "authenticate"), strings.Contains(lower, "credential"):
		return pError("agy_authentication_error", nonEmpty(msg, "Antigravity CLI is not authenticated"), http.StatusUnauthorized)
	case strings.Contains(lower, "quota"), strings.Contains(lower, "rate limit"), strings.Contains(lower, "too many requests"):
		return pError("agy_rate_limit", nonEmpty(msg, "Antigravity quota or rate limit reached"), http.StatusTooManyRequests)
	case strings.Contains(lower, "model") && (strings.Contains(lower, "not found") || strings.Contains(lower, "unknown")):
		return pError("agy_model_not_found", nonEmpty(msg, "Antigravity model not found"), http.StatusNotFound)
	default:
		return pError("agy_execution_error", nonEmpty(msg, "Antigravity CLI execution failed"), http.StatusBadGateway)
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func executeNonStream(req rpcExecutorRequest) (executorResponse, error) {
	cfg := currentConfig()
	opts := resolveExecutionOptions(req, cfg)
	prompt, err := buildPrompt(req.Payload, opts.ConversationID, cfg.Workdir)
	if err != nil {
		return executorResponse{}, err
	}
	result, _, err := runAgyJSON(cfg, opts, prompt)
	if err != nil {
		return executorResponse{}, err
	}

	payload, err := makeChatCompletion(req.Model, result)
	if err != nil {
		return executorResponse{}, pError("response_encoding_error", err.Error(), http.StatusInternalServerError)
	}
	headers := http.Header{"Content-Type": []string{"application/json"}}
	if result.ConversationID != "" {
		headers.Set("X-AGY-Conversation-ID", result.ConversationID)
	}
	return executorResponse{
		Payload: payload,
		Headers: headers,
		Metadata: map[string]any{
			"agy_conversation_id": result.ConversationID,
			"agy_status":          result.Status,
		},
	}, nil
}

func executeStream(req rpcExecutorRequest) {
	streamID := req.StreamID
	cfg := currentConfig()
	opts := resolveExecutionOptions(req, cfg)
	opts.OutputFormat = "stream-json"

	prompt, err := buildPrompt(req.Payload, opts.ConversationID, cfg.Workdir)
	if err != nil {
		closePluginStream(streamID, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout(cfg))
	defer cancel()
	cmd, err := newAgyCommand(ctx, cfg, opts, prompt)
	if err != nil {
		closePluginStream(streamID, err.Error())
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		closePluginStream(streamID, err.Error())
		return
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		closePluginStream(streamID, classifyAgyError(err, stderr.String()).Error())
		return
	}
	activeProcesses.add(cmd)
	defer activeProcesses.remove(cmd)

	completionID := newCompletionID()
	created := time.Now().Unix()
	model := displayModel(req.Model)
	conversationID := opts.ConversationID

	if err := emitPluginStreamChunk(streamID, makeSSEChunk(completionID, created, model, map[string]any{"role": "assistant"}, nil, nil, conversationID)); err != nil {
		killProcessTree(cmd)
		_ = cmd.Wait()
		return
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var terminal *agyResult
	var streamError string
	var streamedText strings.Builder
	isToolCallStream := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event agyStreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		switch event.Event {
		case "init":
			if event.ConversationID != "" {
				conversationID = event.ConversationID
			}
		case "step_update":
			if event.StepUpdate.StepType == "agent_response" && event.StepUpdate.TextDelta != "" {
				streamedText.WriteString(event.StepUpdate.TextDelta)
				trimmedSoFar := strings.TrimSpace(streamedText.String())

				// If tools are configured and response looks like a tool call, buffer instead of streaming raw text
				if opts.HasTools && (strings.HasPrefix(trimmedSoFar, "```tool_calls") || strings.HasPrefix(trimmedSoFar, "```json") || strings.HasPrefix(trimmedSoFar, "[")) {
					isToolCallStream = true
					continue
				}

				if !isToolCallStream {
					payload := makeSSEChunk(completionID, created, model, map[string]any{"content": event.StepUpdate.TextDelta}, nil, nil, conversationID)
					if err := emitPluginStreamChunk(streamID, payload); err != nil {
						killProcessTree(cmd)
						_ = cmd.Wait()
						return
					}
				}
			}
		case "result":
			if event.Result != nil {
				copyResult := *event.Result
				terminal = &copyResult
				if copyResult.ConversationID != "" {
					conversationID = copyResult.ConversationID
				}
			}
		case "error":
			streamError = strings.TrimSpace(event.Message)
			if streamError == "" {
				streamError = "Antigravity stream failed"
			}
		default:
			if event.Status != "" {
				terminal = &agyResult{Status: event.Status, Response: event.Response, Error: event.Error, Usage: event.Usage, ConversationID: event.ConversationID}
				if event.ConversationID != "" {
					conversationID = event.ConversationID
				}
			}
		}
	}

	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	if scanErr != nil {
		closePluginStream(streamID, "reading agy stream: "+scanErr.Error())
		return
	}
	if streamError != "" {
		closePluginStream(streamID, streamError)
		return
	}
	if terminal == nil {
		if waitErr != nil {
			closePluginStream(streamID, classifyAgyError(waitErr, stderr.String()).Error())
		} else {
			closePluginStream(streamID, "agy stream ended without a result event")
		}
		return
	}
	if !strings.EqualFold(terminal.Status, "SUCCESS") {
		msg := nonEmpty(terminal.Error, stderr.String())
		if msg == "" {
			msg = "Antigravity CLI ended with status " + terminal.Status
		}
		closePluginStream(streamID, msg)
		return
	}
	if waitErr != nil {
		closePluginStream(streamID, classifyAgyError(waitErr, stderr.String()).Error())
		return
	}

	fullResponse := terminal.Response
	if fullResponse == "" {
		fullResponse = streamedText.String()
	}

	toolCalls, isToolCall := extractToolCalls(fullResponse)
	if opts.HasTools && isToolCall {
		// Emit tool_calls delta chunk
		toolDelta := makeSSEChunk(completionID, created, model, map[string]any{
			"tool_calls": toolCalls,
		}, nil, nil, conversationID)
		_ = emitPluginStreamChunk(streamID, toolDelta)

		finishReason := "tool_calls"
		var finalUsage map[string]any
		if requestWantsUsage(req.Payload) {
			finalUsage = openAIUsage(terminal.Usage)
		}
		finalPayload := makeSSEChunk(completionID, created, model, map[string]any{}, &finishReason, finalUsage, conversationID)
		_ = emitPluginStreamChunk(streamID, finalPayload)
		_ = emitPluginStreamChunk(streamID, []byte("data: [DONE]\n\n"))
		closePluginStream(streamID, "")
		return
	}

	// If it was buffered thinking it was a tool call but turned out not to be
	if isToolCallStream && streamedText.Len() > 0 {
		payload := makeSSEChunk(completionID, created, model, map[string]any{"content": streamedText.String()}, nil, nil, conversationID)
		_ = emitPluginStreamChunk(streamID, payload)
	} else if streamedText.Len() == 0 && terminal.Response != "" {
		if err := emitPluginStreamChunk(streamID, makeSSEChunk(completionID, created, model, map[string]any{"content": terminal.Response}, nil, nil, conversationID)); err != nil {
			return
		}
	}

	finish := "stop"
	if err := emitPluginStreamChunk(streamID, makeSSEChunk(completionID, created, model, map[string]any{}, &finish, nil, conversationID)); err != nil {
		return
	}
	if requestWantsUsage(req.Payload) {
		if err := emitPluginStreamChunk(streamID, makeSSEChunk(completionID, created, model, map[string]any{}, nil, openAIUsage(terminal.Usage), conversationID)); err != nil {
			return
		}
	}
	if err := emitPluginStreamChunk(streamID, []byte("data: [DONE]\n\n")); err != nil {
		return
	}
	closePluginStream(streamID, "")
}

func emitPluginStreamChunk(streamID string, payload []byte) error {
	if strings.TrimSpace(streamID) == "" {
		return fmt.Errorf("plugin stream id is required")
	}
	_, err := callHost(methodHostStreamEmit, streamEmitRequest{StreamID: streamID, Payload: payload})
	return err
}

func closePluginStream(streamID, errMsg string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	_, _ = callHost(methodHostStreamClose, streamCloseRequest{StreamID: streamID, Error: strings.TrimSpace(errMsg)})
}
