package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const (
	pluginID      = "cliproxy-antigravity"
	providerID    = "agy"
	pluginVersion = "0.1.0"
	schemaVersion = 6
)

const (
	methodPluginRegister        = "plugin.register"
	methodPluginReconfigure     = "plugin.reconfigure"
	methodModelRegister         = "model.register"
	methodModelStatic           = "model.static"
	methodExecutorIdentifier    = "executor.identifier"
	methodExecutorExecute       = "executor.execute"
	methodExecutorExecuteStream = "executor.execute_stream"
	methodExecutorCountTokens   = "executor.count_tokens"
	methodExecutorHTTPRequest   = "executor.http_request"
	methodHostStreamEmit        = "host.stream.emit"
	methodHostStreamClose       = "host.stream.close"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type pluginError struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *pluginError) Error() string { return e.Message }

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
	EnumValues  []string
	Description string
}

type registrationCapability struct {
	ModelRegistrar        bool     `json:"model_registrar"`
	ModelProvider         bool     `json:"model_provider"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type modelRegistrationResponse struct {
	Provider string
	Models   []modelInfo
}

type modelResponse struct {
	Provider string
	Models   []modelInfo
}

type modelInfo struct {
	ID                         string
	Object                     string
	OwnedBy                    string
	DisplayName                string
	Name                       string
	Description                string
	SupportedGenerationMethods []string
	UserDefined                bool
}

type rpcExecutorRequest struct {
	AuthID          string
	AuthProvider    string
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

type executorResponse struct {
	Payload  []byte
	Headers  http.Header
	Metadata map[string]any
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
		return okEnvelope(pluginRegistration())

	case methodModelRegister:
		models := discoverModels(currentConfig())
		return okEnvelope(modelRegistrationResponse{Provider: providerID, Models: models})

	case methodModelStatic:
		models := discoverModels(currentConfig())
		return okEnvelope(modelResponse{Provider: providerID, Models: models})

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
		if _, err := buildPrompt(req.Payload); err != nil {
			return nil, err
		}
		go executeStream(req)
		return okEnvelope(executorStreamResponse{Headers: http.Header{"Content-Type": []string{"text/event-stream"}}})

	case methodExecutorCountTokens:
		var req rpcExecutorRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, pError("invalid_request", "could not decode count-tokens request: "+err.Error(), 400)
		}
		prompt, err := buildPrompt(req.Payload)
		if err != nil {
			return nil, err
		}
		estimated := estimateTokens(prompt)
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

	default:
		return nil, pError("unknown_method", fmt.Sprintf("unknown plugin method %q", method), 404)
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: schemaVersion,
		Metadata: metadata{
			Name:             "CLIProxy Antigravity",
			Version:          pluginVersion,
			Author:           "adeebahmad01 and contributors",
			GitHubRepository: "https://github.com/adeebahmad01/cliproxy-antigravity",
			ConfigFields: []configField{
				{Name: "binary_path", Type: "string", Description: "Path or command name for the official Antigravity CLI (default: agy)."},
				{Name: "workdir", Type: "string", Description: "Working directory used when agy executes requests."},
				{Name: "print_timeout", Type: "string", Description: "Antigravity print timeout, using Go/CLI duration syntax (default: 30m)."},
				{Name: "dangerously_skip_permissions", Type: "boolean", Description: "Pass --dangerously-skip-permissions to agy. Disabled by default."},
				{Name: "sandbox", Type: "boolean", Description: "Pass --sandbox to agy when supported by the installed CLI."},
			},
		},
		Capabilities: registrationCapability{
			ModelRegistrar:        true,
			ModelProvider:         true,
			Executor:              true,
			ExecutorModelScope:    "static",
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
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

func mustErrorEnvelope(code, message string, status int) []byte {
	raw, _ := json.Marshal(envelope{
		OK: false,
		Error: &envelopeError{
			Code:       code,
			Message:    message,
			HTTPStatus: status,
		},
	})
	return raw
}
