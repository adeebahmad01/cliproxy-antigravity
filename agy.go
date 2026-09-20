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
	Event      string `json:"event"`
	Message    string `json:"message"`
	StepUpdate struct {
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
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
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

func discoverModels(cfg pluginConfig) []modelInfo {
	modelCatalog.Lock()
	defer modelCatalog.Unlock()

	if modelCatalog.binaryPath == cfg.BinaryPath && time.Since(modelCatalog.fetchedAt) < time.Minute && len(modelCatalog.models) > 0 {
		return cloneModels(modelCatalog.models)
	}

	models := []modelInfo{defaultModelInfo()}
	nativeByID := map[string]string{"agy/default": ""}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.BinaryPath, "models")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if cfg.Workdir != "" {
		cmd.Dir = cfg.Workdir
	}

	if err := cmd.Run(); err == nil {
		discovered, mapping := parseAgyModelLines(stdout.String())
		models = append(models, discovered...)
		for id, native := range mapping {
			nativeByID[id] = native
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
	model = strings.TrimSpace(model)
	if model == "" || model == "agy/default" || model == "default" {
		return ""
	}

	// Refreshes the cached map if needed. Discovery is deliberately best-effort.
	_ = discoverModels(cfg)
	modelCatalog.Lock()
	native := modelCatalog.nativeByID[model]
	modelCatalog.Unlock()
	if native != "" {
		return native
	}
	return strings.TrimPrefix(model, "agy/")
}

func agyArgs(cfg pluginConfig, nativeModel, outputFormat, prompt string) []string {
	args := []string{"--output-format", outputFormat, "--print-timeout", cfg.PrintTimeout}
	if cfg.DangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	if cfg.Sandbox {
		args = append(args, "--sandbox")
	}
	if strings.TrimSpace(nativeModel) != "" {
		args = append(args, "--model", nativeModel)
	}
	args = append(args, "-p", prompt)
	return args
}

func commandTimeout(cfg pluginConfig) time.Duration {
	d, err := time.ParseDuration(cfg.PrintTimeout)
	if err != nil || d <= 0 {
		d = 30 * time.Minute
	}
	return d + 30*time.Second
}

func newAgyCommand(ctx context.Context, cfg pluginConfig, nativeModel, outputFormat, prompt string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, cfg.BinaryPath, agyArgs(cfg, nativeModel, outputFormat, prompt)...)
	if cfg.Workdir != "" {
		cmd.Dir = cfg.Workdir
	}
	return cmd
}

func runAgyJSON(cfg pluginConfig, nativeModel, prompt string) (agyResult, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout(cfg))
	defer cancel()

	cmd := newAgyCommand(ctx, cfg, nativeModel, "json", prompt)
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
	prompt, err := buildPrompt(req.Payload)
	if err != nil {
		return executorResponse{}, err
	}
	cfg := currentConfig()
	nativeModel := resolveNativeModel(req.Model, cfg)
	result, _, err := runAgyJSON(cfg, nativeModel, prompt)
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
	prompt, err := buildPrompt(req.Payload)
	if err != nil {
		closePluginStream(streamID, err.Error())
		return
	}
	cfg := currentConfig()
	nativeModel := resolveNativeModel(req.Model, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout(cfg))
	defer cancel()
	cmd := newAgyCommand(ctx, cfg, nativeModel, "stream-json", prompt)
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
	if err := emitPluginStreamChunk(streamID, makeSSEChunk(completionID, created, model, map[string]any{"role": "assistant"}, nil, nil)); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var terminal *agyResult
	var streamError string
	var streamedText strings.Builder

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
		case "step_update":
			if event.StepUpdate.StepType == "agent_response" && event.StepUpdate.TextDelta != "" {
				streamedText.WriteString(event.StepUpdate.TextDelta)
				payload := makeSSEChunk(completionID, created, model, map[string]any{"content": event.StepUpdate.TextDelta}, nil, nil)
				if err := emitPluginStreamChunk(streamID, payload); err != nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					return
				}
			}
		case "result":
			if event.Result != nil {
				copyResult := *event.Result
				terminal = &copyResult
			}
		case "error":
			streamError = strings.TrimSpace(event.Message)
			if streamError == "" {
				streamError = "Antigravity stream failed"
			}
		default:
			if event.Status != "" {
				terminal = &agyResult{Status: event.Status, Response: event.Response, Error: event.Error, Usage: event.Usage}
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

	if streamedText.Len() == 0 && terminal.Response != "" {
		if err := emitPluginStreamChunk(streamID, makeSSEChunk(completionID, created, model, map[string]any{"content": terminal.Response}, nil, nil)); err != nil {
			return
		}
	}

	finish := "stop"
	if err := emitPluginStreamChunk(streamID, makeSSEChunk(completionID, created, model, map[string]any{}, &finish, nil)); err != nil {
		return
	}
	if requestWantsUsage(req.Payload) {
		if err := emitPluginStreamChunk(streamID, makeSSEChunk(completionID, created, model, map[string]any{}, nil, openAIUsage(terminal.Usage))); err != nil {
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
