package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	pluginID      = "cliproxy-antigravity"
	providerID    = "agy"
	schemaVersion = 6
)

// pluginVersion can be overridden with -X main.pluginVersion at build time.
var pluginVersion = "0.7.1"

const (
	methodPluginRegister        = "plugin.register"
	methodPluginReconfigure     = "plugin.reconfigure"
	methodModelRegister         = "model.register"
	methodModelStatic           = "model.static"
	methodModelForAuth          = "model.for_auth"
	methodModelRoute            = "model.route"
	methodSchedulerPick         = "scheduler.pick"
	methodInterceptBefore       = "request.intercept_before"
	methodInterceptAfter        = "request.intercept_after"
	methodExecutorIdentifier    = "executor.identifier"
	methodExecutorExecute       = "executor.execute"
	methodExecutorExecuteStream = "executor.execute_stream"
	methodExecutorCountTokens   = "executor.count_tokens"
	methodExecutorHTTPRequest   = "executor.http_request"
	methodManagementRegister    = "management.register"
	methodManagementHandle      = "management.handle"
	methodQuotaIdentifier       = "quota.identifier"
	methodQuotaDescribe         = "quota.describe"
	methodQuotaFetch            = "quota.fetch"
	methodQuotaReset            = "quota.reset"
	methodAuthIdentifier        = "auth.identifier"
	methodAuthParse             = "auth.parse"
	methodAuthLoginStart        = "auth.login.start"
	methodAuthLoginPoll         = "auth.login.poll"
	methodAuthRefresh           = "auth.refresh"
	methodHostStreamEmit        = "host.stream.emit"
	methodHostStreamClose       = "host.stream.close"
	methodPluginQuiesce         = "plugin.quiesce"
	methodPluginShutdown        = "plugin.shutdown"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

// Error codes CLIProxyAPI's routing and cooldown pipeline recognizes (pluginabi.ErrorCode*).
const (
	// errorCodeRequestScoped marks a failure caused by the request itself: no cooldown, no credential rotation.
	errorCodeRequestScoped = "request_scoped"
	// errorCodeModelNotFound cools only the requested model as unsupported.
	errorCodeModelNotFound = "model_not_found"
)

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
	// RetryAfterSeconds feeds CLIProxyAPI's cooldown and the client Retry-After header.
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
}

type pluginError struct {
	Code              string
	Message           string
	HTTPStatus        int
	Retryable         bool
	RetryAfterSeconds int
}

func (e *pluginError) Error() string { return e.Message }

func (e *pluginError) envelopeError() *envelopeError {
	return &envelopeError{
		Code:              e.Code,
		Message:           e.Message,
		Retryable:         e.Retryable,
		HTTPStatus:        e.HTTPStatus,
		RetryAfterSeconds: e.RetryAfterSeconds,
	}
}

func asPluginError(err error) *pluginError {
	if err == nil {
		return &pluginError{Code: "plugin_error", Message: "unknown error", HTTPStatus: 500}
	}
	var pe *pluginError
	if errors.As(err, &pe) {
		return pe
	}
	return &pluginError{Code: "plugin_error", Message: err.Error(), HTTPStatus: 500}
}

func pError(code, message string, status int) error {
	return &pluginError{Code: code, Message: message, HTTPStatus: status}
}

func pRetryableError(code, message string, status int) error {
	return &pluginError{Code: code, Message: message, HTTPStatus: status, Retryable: true}
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      metadata               `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type metadata struct {
	Name             string
	Version          string
	Author           string
	GitHubRepository string
	Logo             string
	ConfigFields     []configField
}

type configField struct {
	Name        string
	Type        string
	Default     any
	EnumValues  []string
	Description string
}

type registrationCapability struct {
	Scheduler                 bool     `json:"scheduler"`
	SchedulerAcrossPriorities bool     `json:"scheduler_across_priorities"`
	RequestInterceptor        bool     `json:"request_interceptor"`
	ModelRegistrar            bool     `json:"model_registrar"`
	ModelProvider             bool     `json:"model_provider"`
	ModelRouter               bool     `json:"model_router"`
	Executor                  bool     `json:"executor"`
	ExecutorModelScope        string   `json:"executor_model_scope"`
	ExecutorInputFormats      []string `json:"executor_input_formats"`
	ExecutorOutputFormats     []string `json:"executor_output_formats"`
	ManagementAPI             bool     `json:"management_api"`
	QuotaProvider             bool     `json:"quota_provider"`
	AuthProvider              bool     `json:"auth_provider"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type modelRegistrationResponse struct {
	Provider string      `json:"Provider"`
	Models   []modelInfo `json:"Models"`
}

type modelResponse struct {
	Provider string      `json:"Provider"`
	Models   []modelInfo `json:"Models"`
}

type authModelRequest struct {
	AuthID       string
	AuthProvider string
	Metadata     map[string]any
	Host         hostConfigSummary
}

type modelInfo struct {
	ID                         string   `json:"ID"`
	Object                     string   `json:"Object"`
	OwnedBy                    string   `json:"OwnedBy"`
	DisplayName                string   `json:"DisplayName"`
	Name                       string   `json:"Name"`
	Description                string   `json:"Description"`
	SupportedGenerationMethods []string `json:"SupportedGenerationMethods"`
	UserDefined                bool     `json:"UserDefined"`
}

type rpcExecutorRequest struct {
	AuthID          string
	AuthProvider    string
	AuthMetadata    map[string]any
	Model           string
	Format          string
	Stream          bool
	Alt             string
	Headers         http.Header
	OriginalRequest []byte
	SourceFormat    string
	Payload         []byte
	Metadata        map[string]any
	StreamID        string `json:"stream_id,omitempty"`
	HostCallbackID  string `json:"host_callback_id,omitempty"`
}

// modelRouteRequest is the subset of pluginapi.ModelRouteRequest the router needs.
type modelRouteRequest struct {
	RequestedModel     string
	AvailableProviders []string
	Headers            http.Header
	Body               []byte
	Metadata           map[string]any
}

type modelRouteResponse struct {
	Handled    bool
	TargetKind string
	Target     string `json:",omitempty"`
	Reason     string
}

type executorResponse struct {
	Payload  []byte         `json:"Payload"`
	Headers  http.Header    `json:"Headers,omitempty"`
	Metadata map[string]any `json:"Metadata,omitempty"`
}

type executorStreamResponse struct {
	Headers http.Header   `json:"headers,omitempty"`
	Chunks  []streamChunk `json:"chunks,omitempty"`
}

type streamChunk struct {
	Payload []byte
	Err     error
}

type executorHTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

type streamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type streamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
	// ErrorDetail keeps the HTTP status, code and retry-after that Error alone cannot carry.
	// Hosts that predate it fall back to Error.
	ErrorDetail *envelopeError `json:"error_detail,omitempty"`
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		var req lifecycleRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, pError("invalid_config", "could not decode plugin configuration: "+err.Error(), 400)
			}
		}
		cfg, err := parsePluginConfig(req.ConfigYAML)
		if err != nil {
			return nil, pError("invalid_config", err.Error(), 400)
		}
		setConfig(cfg)
		// The host only sends the plugin's own config section, so this is a best guess until a
		// request carrying the host's auth-dir arrives (see rememberAuthDir).
		if currentAuthDir() == "" {
			setAuthDir(resolveAuthDir(req.ConfigYAML))
		}
		ensureAuthFile(req.ConfigYAML, cfg)
		return okEnvelope(pluginRegistration())

	case methodModelRegister:
		return okEnvelope(modelRegistrationResponse{Provider: providerID, Models: cachedEnabledModels(currentConfig())})

	case methodModelStatic:
		return okEnvelope(modelResponse{Provider: providerID, Models: cachedEnabledModels(currentConfig())})

	case methodModelForAuth:
		var req authModelRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, pError("invalid_request", "could not decode auth model request: "+err.Error(), 400)
		}
		rememberAuthDir(req.Host.AuthDir)
		cfg, err := withAccount(currentConfig(), req.Metadata)
		if err != nil {
			return nil, err
		}
		models, _, ok := loadCatalog(cfg)
		if !ok {
			return nil, pRetryableError("agy_model_discovery_failed", "agy listed no models for "+nonEmpty(cfg.AccountHome, "the process home"), http.StatusServiceUnavailable)
		}
		reconcileAccounts()
		return okEnvelope(modelResponse{Provider: providerID, Models: cloneModels(models)})

	case methodModelRoute:
		return okEnvelope(modelRouteResponse{})

	case methodSchedulerPick:
		var req schedulerRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, pError("invalid_request", "could not decode scheduler request: "+err.Error(), 400)
		}
		pick, err := pickConversationAccount(req)
		if err != nil {
			return nil, err
		}
		return okEnvelope(pick)

	case methodInterceptBefore:
		var req struct{ Body []byte }
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, pError("invalid_request", "could not decode interceptor request: "+err.Error(), 400)
		}
		return okEnvelope(struct {
			Headers http.Header `json:",omitempty"`
		}{conversationRoutingHeaders(req.Body)})

	case methodInterceptAfter:
		return okEnvelope(struct{}{})

	case methodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerID})

	case methodExecutorExecute:
		var req rpcExecutorRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, pError("invalid_request", "could not decode executor request: "+err.Error(), 400)
		}
		resp, err := executeNonStream(req)
		if err != nil {
			return nil, err
		}
		return okEnvelope(resp)

	case methodExecutorExecuteStream:
		var req rpcExecutorRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, pError("invalid_request", "could not decode streaming executor request: "+err.Error(), 400)
		}
		if strings.TrimSpace(req.StreamID) == "" {
			return nil, pError("stream_unavailable", "CLIProxyAPI did not provide a plugin stream ID", 500)
		}
		cfg, err := withAccount(currentConfig(), req.AuthMetadata)
		if err != nil {
			return nil, err
		}
		opts, acp := resolveExecutionOptionsForRequest(req, cfg)
		if acp {
			if err := checkModelLimits(cfg, opts.NativeModel); err != nil {
				return nil, err
			}
			if err := beginACPWork(); err != nil {
				return nil, err
			}
			safeGo("executeStreamACP", func() {
				defer endACPWork()
				executeStreamACP(req, cfg, opts)
			})
			headers := http.Header{"Content-Type": []string{"text/event-stream"}}
			mergeHeaders(headers, limitHeaders(cfg, opts.NativeModel))
			return okEnvelope(executorStreamResponse{Headers: headers})
		}
		run, err := prepareAgyExecution(req)
		if err != nil {
			return nil, err
		}
		if err := checkModelLimits(run.cfg, run.opts.NativeModel); err != nil {
			if run.cleanup != nil {
				run.cleanup()
			}
			return nil, err
		}
		run.opts.OutputFormat = "stream-json"
		safeGo("executeStream", func() { executeStream(req, run) })
		headers := http.Header{"Content-Type": []string{"text/event-stream"}}
		if run.session.NativeID != "" {
			if id := run.session.clientID(); id != "" {
				headers.Set(conversationHeader, id)
			}
		}
		mergeHeaders(headers, limitHeaders(run.cfg, run.opts.NativeModel))
		if run.promptBytes > 0 {
			headers.Set(promptBytesHeader, fmt.Sprintf("%d", run.promptBytes))
		}
		// Observable, not silent: the turn was staged to a file agy reads in full.
		if run.stagedBytes > 0 {
			headers.Set(promptStagedHeader, fmt.Sprintf("%d", run.stagedBytes))
		}
		return okEnvelope(executorStreamResponse{Headers: headers})

	case methodExecutorCountTokens:
		var req rpcExecutorRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, pError("invalid_request", "could not decode count-tokens request: "+err.Error(), 400)
		}
		// Never stage, reject on max_prompt_bytes, or discover models for count_tokens:
		// clients call this specifically to measure oversized conversations before compacting.
		run, err := prepareAgyExecutionWithOptions(req, false, false)
		if err != nil {
			return nil, err
		}
		if run.cleanup != nil {
			defer run.cleanup()
		}
		// Token counting must reflect the conversation, not the short handoff that
		// replaced an oversized prompt.
		estimated := estimateTokens(promptForTokenCount(req, run))
		payload, _ := json.Marshal(map[string]int{"total_tokens": estimated})
		return okEnvelope(executorResponse{Payload: payload, Headers: http.Header{"Content-Type": []string{"application/json"}}})

	case methodExecutorHTTPRequest:
		body, _ := json.Marshal(map[string]any{
			"error": map[string]string{
				"message": "raw executor HTTP bridging is not supported by cliproxy-antigravity",
				"type":    "unsupported_operation",
			},
		})
		return okEnvelope(executorHTTPResponse{
			StatusCode: http.StatusNotImplemented,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       body,
		})

	case methodManagementRegister:
		return okEnvelope(managementRegistrationResponse{
			Routes: []managementRoute{{
				Method:      http.MethodGet,
				Path:        limitsManagementPath,
				Description: "Antigravity usage limits (weekly and 5-hour quota per model group). Add ?refresh=1 to bypass the cache.",
			}, {
				Method:      http.MethodGet,
				Path:        accountsManagementPath,
				Description: "Antigravity accounts with their usage limits. Add ?refresh=1 to bypass the limits cache.",
			}},
			Resources: []resourceRoute{{
				Path:        accountsResourcePath,
				Menu:        "Antigravity accounts",
				Description: "Add, disable and remove Antigravity CLI accounts and see their usage limits.",
			}},
		})

	case methodManagementHandle:
		var req managementRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, pError("invalid_request", "could not decode management request: "+err.Error(), 400)
		}
		return okEnvelope(handleManagement(req))

	case methodQuotaIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerID})

	case methodQuotaDescribe:
		return okEnvelope(quotaDescribeResponse{SupportedProviders: []string{providerID, "antigravity"}, DisplayName: "Antigravity CLI"})

	case methodQuotaFetch:
		var req quotaFetchRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, pError("invalid_request", "could not decode quota fetch request: "+err.Error(), 400)
			}
		}
		cfg, err := withAccount(currentConfig(), req.Metadata)
		if err != nil {
			return nil, err
		}
		if !cfg.LimitsTracking {
			return nil, pError("limits_tracking_disabled", "limits tracking is disabled in the cliproxy-antigravity configuration", http.StatusServiceUnavailable)
		}
		limits, _, err := currentLimits(cfg)
		if limits == nil {
			if err == nil {
				err = fmt.Errorf("Antigravity usage limits are unavailable")
			}
			return nil, err
		}
		return okEnvelope(quotaFetchFromLimits(limits))

	case methodAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerID})

	case methodAuthParse:
		var req authParseRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, pError("invalid_request", "could not decode auth parse request: "+err.Error(), 400)
		}
		rememberAuthDir(req.Host.AuthDir)
		reconcileAccounts()
		return okEnvelope(parseAgyAuth(req))

	case methodAuthLoginStart:
		var req authLoginStartRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, pError("invalid_request", "could not decode login start request: "+err.Error(), 400)
			}
		}
		rememberAuthDir(req.Host.AuthDir)
		resp, err := startAgyLogin(currentConfig())
		if err != nil {
			return nil, pError("agy_login_failed", err.Error(), http.StatusBadGateway)
		}
		return okEnvelope(resp)

	case methodAuthLoginPoll:
		var req authLoginPollRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, pError("invalid_request", "could not decode login poll request: "+err.Error(), 400)
		}
		rememberAuthDir(req.Host.AuthDir)
		return okEnvelope(pollAgyLogin(req.State, currentAuthDir()))

	case methodAuthRefresh:
		var req authRefreshRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, pError("invalid_request", "could not decode auth refresh request: "+err.Error(), 400)
			}
		}
		return okEnvelope(refreshAgyAuth(req))

	case methodQuotaReset:
		return okEnvelope(quotaResetResponse{Success: false, Message: "Antigravity quota cannot be reset by the plugin; it refreshes on the schedule reported by agy"})

	case methodPluginQuiesce:
		// CLIProxyAPI is replacing this instance (hot reload): release the resident agy processes
		// once in-flight turns finish, or they outlive the retired library.
		drainACP(10 * time.Minute)
		return okEnvelope(struct{}{})

	case methodPluginShutdown:
		activeProcesses.killAll()
		retireAllACP()
		return okEnvelope(struct{}{})

	default:
		return nil, pError("unknown_method", fmt.Sprintf("unknown plugin method %q", method), 404)
	}
}

func pluginRegistration() registration {
	defaults := defaultConfig()
	return registration{
		SchemaVersion: schemaVersion,
		Metadata: metadata{
			Name:             "CLIProxy Antigravity",
			Version:          pluginVersion,
			Author:           "adeebahmad01 and contributors",
			GitHubRepository: "https://github.com/adeebahmad01/cliproxy-antigravity",
			ConfigFields: []configField{
				{Name: "binary_path", Type: "string", Default: defaults.BinaryPath, Description: "Path or command name for the official Antigravity CLI (default: agy)."},
				{Name: "workdir", Type: "string", Default: defaults.Workdir, Description: "Working directory used when agy executes requests."},
				{Name: "print_timeout", Type: "string", Default: defaults.PrintTimeout, Description: "Antigravity print timeout, using Go/CLI duration syntax (default: 30m)."},
				{Name: "dangerously_skip_permissions", Type: "boolean", Default: defaults.DangerouslySkipPermissions, Description: "Pass --dangerously-skip-permissions to agy. Disabled by default."},
				{Name: "sandbox", Type: "boolean", Default: defaults.Sandbox, Description: "Pass --sandbox to agy when supported by the installed CLI."},
				{Name: "reasoning_effort", Type: "enum", Default: defaults.DefaultReasoningEffort, EnumValues: []string{"low", "medium", "high", "xhigh", "max"}, Description: "Default reasoning effort for agy sessions (low, medium, high, xhigh, max). Empty uses the per-model default."},
				{Name: "limits_tracking", Type: "boolean", Default: defaults.LimitsTracking, Description: "Track Antigravity usage limits via agy /usage (default: true)."},
				{Name: "limits_refresh_interval", Type: "string", Default: defaults.LimitsRefreshInterval, Description: "How long cached usage limits stay fresh, as a duration (default: 1m, minimum 10s)."},
				{Name: "enforce_limits", Type: "boolean", Default: defaults.EnforceLimits, Description: "Reject requests with HTTP 429 when the quota group of the model is exhausted, without launching agy (default: false)."},
				{Name: "max_prompt_bytes", Type: "integer", Default: defaults.MaxPromptBytes, Description: "Apply max_prompt_action to requests larger than this many bytes, instead of letting agy truncate the prompt tail (default: 150000; 0 disables)."},
				{Name: "max_prompt_action", Type: "enum", Default: defaults.MaxPromptAction, EnumValues: []string{promptActionStage, promptActionError}, Description: "Action when a built prompt exceeds max_prompt_bytes: stage (default, write to a private temp file that agy reads in full in mode: print) or error (reject with HTTP 413 request_scoped so the client can compact). Ignored when max_prompt_bytes is 0."},
				{Name: "stream_keepalive_interval", Type: "string", Default: defaults.StreamKeepaliveInterval, Description: "Emit no-op streaming keepalive deltas at this interval (default: 15s, minimum 15s, 0s disables) after agy initializes a turn, keeping proxies from dropping silent staged turns."},
				{Name: "mode", Type: "enum", Default: defaults.Mode, EnumValues: []string{modePrint, modeACP}, Description: "Execution mode: print (default, runs agy -p per request) or acp (keeps a resident agy acp server per account home with session reuse; conversation_id requests stay on print)."},
				{Name: "conv_debug_file", Type: "string", Default: defaults.ConvDebugFile, Description: "Optional JSONL log path for ACP session reuse lookup/store troubleshooting (contains message excerpts)."},
				{Name: "accounts_dir", Type: "string", Default: defaults.AccountsDir, Description: "Directory with one agy home per account. Accounts added from the panel are created here (default ~/.agy-accounts), and every signed-in home in it gets its own CLIProxyAPI auth file. CLIProxyAPI rotates between the accounts."},
			},
		},
		Capabilities: registrationCapability{
			Scheduler:                 true,
			SchedulerAcrossPriorities: true,
			RequestInterceptor:        true,
			ModelRegistrar:            true,
			ModelProvider:             true,
			ModelRouter:               false,
			Executor:                  true,
			ExecutorModelScope:        "oauth",
			ExecutorInputFormats:      []string{"chat-completions"},
			ExecutorOutputFormats:     []string{"chat-completions"},
			ManagementAPI:             true,
			QuotaProvider:             true,
			AuthProvider:              true,
		},
	}
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func mustErrorEnvelope(pe *pluginError) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: pe.envelopeError()})
	return raw
}

// promptForTokenCount returns the full conversation prompt a request represents,
// without staged handoff wrapping or native-resume history trimming.
func promptForTokenCount(req rpcExecutorRequest, run agyExecution) string {
	if run.opts.ConversationID != "" {
		if chatReq, err := decodeChatRequest(req.Payload); err == nil {
			if full, err := buildChatPrompt(chatReq, "", run.cfg.Workdir); err == nil {
				return full
			}
		}
	}
	if run.fullPrompt != "" {
		return run.fullPrompt
	}
	if run.stagedBytes == 0 {
		return run.prompt
	}
	chatReq, err := decodeChatRequest(req.Payload)
	if err != nil {
		return run.prompt
	}
	full, err := buildChatPrompt(chatReq, "", run.cfg.Workdir)
	if err != nil {
		return run.prompt
	}
	return full
}
