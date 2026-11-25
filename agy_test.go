package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestParseAgyModelLines(t *testing.T) {
	models, mapping := parseAgyModelLines(`
gemini-3.6-flash-high	Gemini 3.6 Flash (High)
gemini-3.6-flash-medium    Gemini 3.6 Flash (Medium)
gemini-4-hydrogen-xhigh	Gemini 4 Hydrogen (XHigh)
* Claude Sonnet 4.6 (Thinking)
`)
	if len(models) != 6 {
		t.Fatalf("len(models) = %d, want 6: %#v", len(models), models)
	}
	if mapping["agy/gemini-3.6-flash-high"] != "gemini-3.6-flash-high" {
		t.Fatalf("unexpected mapping: %#v", mapping)
	}
	if mapping["agy/gemini-3.6-flash"] != "gemini-3.6-flash" {
		t.Fatalf("base model mapping missing: %#v", mapping)
	}
	if mapping["agy/gemini-4-hydrogen-xhigh"] != "gemini-4-hydrogen-xhigh" || mapping["agy/gemini-4-hydrogen"] != "gemini-4-hydrogen" {
		t.Fatalf("gemini-4-hydrogen mapping missing: %#v", mapping)
	}
	if mapping["agy/claude-sonnet-4-6-thinking"] != "Claude Sonnet 4.6 (Thinking)" {
		t.Fatalf("legacy label mapping missing: %#v", mapping)
	}

	byID := map[string]modelInfo{}
	for _, m := range models {
		byID[m.ID] = m
		if m.Name != m.ID {
			t.Fatalf("model %q has Name=%q, want %q", m.ID, m.Name, m.ID)
		}
	}
	if byID["agy/gemini-3.6-flash-high"].DisplayName != "Gemini 3.6 Flash (High)" {
		t.Fatalf("unexpected DisplayName for gemini-3.6-flash-high: %q", byID["agy/gemini-3.6-flash-high"].DisplayName)
	}
	if byID["agy/gemini-3.6-flash"].DisplayName != "Gemini 3.6 Flash" {
		t.Fatalf("unexpected DisplayName for base gemini-3.6-flash: %q", byID["agy/gemini-3.6-flash"].DisplayName)
	}
	if byID["agy/gemini-4-hydrogen-xhigh"].DisplayName != "Gemini 4 Hydrogen (XHigh)" {
		t.Fatalf("unexpected DisplayName for gemini-4-hydrogen-xhigh: %q", byID["agy/gemini-4-hydrogen-xhigh"].DisplayName)
	}
	if byID["agy/gemini-4-hydrogen"].DisplayName != "Gemini 4 Hydrogen" {
		t.Fatalf("unexpected DisplayName for base gemini-4-hydrogen: %q", byID["agy/gemini-4-hydrogen"].DisplayName)
	}

	rawModels, _ := parseAgyModelLines(`
gemini-3.8-flash-high
gemini-4-hydrogen-max
gpt-oss-120b-medium
claude-sonnet-4-6
`)
	rawByID := map[string]modelInfo{}
	for _, m := range rawModels {
		rawByID[m.ID] = m
	}
	if rawByID["agy/gemini-3.8-flash"].DisplayName != "Gemini 3.8 Flash" {
		t.Fatalf("unexpected humanized DisplayName for raw base slug: %q", rawByID["agy/gemini-3.8-flash"].DisplayName)
	}
	if rawByID["agy/gemini-4-hydrogen"].DisplayName != "Gemini 4 Hydrogen" || rawByID["agy/gemini-4-hydrogen-max"].DisplayName != "Gemini 4 Hydrogen (Max)" {
		t.Fatalf("unexpected humanized DisplayName for raw max slug: %#v", rawByID)
	}
	if rawByID["agy/gpt-oss-120b"].DisplayName != "GPT-OSS 120B" {
		t.Fatalf("unexpected humanized DisplayName for raw base slug: %q", rawByID["agy/gpt-oss-120b"].DisplayName)
	}
	if rawByID["agy/claude-sonnet-4-6"].DisplayName != "Claude Sonnet 4.6" {
		t.Fatalf("unexpected humanized DisplayName for raw claude slug: %q", rawByID["agy/claude-sonnet-4-6"].DisplayName)
	}

	// Label-only output must keep clean baseSlug in nativeByID regardless of line or map order.
	for _, lines := range []string{
		"* Gemini 4 Hydrogen (XHigh)\n",
		"* Gemini 4 Hydrogen\n* Gemini 4 Hydrogen (XHigh)\n",
		"* Gemini 4 Hydrogen (XHigh)\n* Gemini 4 Hydrogen\n",
	} {
		_, labelMapping := parseAgyModelLines(lines)
		if got := labelMapping["agy/gemini-4-hydrogen"]; got != "gemini-4-hydrogen" {
			t.Fatalf("parseAgyModelLines(%q) mapping[\"agy/gemini-4-hydrogen\"] = %q, want %q", lines, got, "gemini-4-hydrogen")
		}
		for i := 0; i < 20; i++ {
			nativeByID := map[string]string{}
			mergeDiscoveredNativeMap(nativeByID, labelMapping)
			for _, key := range []string{"gemini-4-hydrogen", "agy/gemini-4-hydrogen", "antigravity/gemini-4-hydrogen"} {
				if got := nativeByID[key]; got != "gemini-4-hydrogen" {
					t.Fatalf("lines %q: nativeByID[%q] = %q, want %q", lines, key, got, "gemini-4-hydrogen")
				}
			}
		}
	}
}

func TestHumanizeModelName(t *testing.T) {
	tests := map[string]string{
		"gemini-3.8-flash-high":        "Gemini 3.8 Flash (High)",
		"gemini-3.8-flash":             "Gemini 3.8 Flash",
		"gemini-4-hydrogen-xhigh":      "Gemini 4 Hydrogen (XHigh)",
		"gemini-4-hydrogen-x-high":     "Gemini 4 Hydrogen (XHigh)",
		"gemini-4-hydrogen-extra-high": "Gemini 4 Hydrogen (XHigh)",
		"gemini-4-hydrogen-max":        "Gemini 4 Hydrogen (Max)",
		"gemini-4-hydrogen":            "Gemini 4 Hydrogen",
		"gpt-oss-120b-medium":          "GPT-OSS 120B (Medium)",
		"gpt-oss-120b":                 "GPT-OSS 120B",
		"claude-sonnet-4-6":            "Claude Sonnet 4.6",
		"claude-opus-4-6-thinking":     "Claude Opus 4.6 (Thinking)",
		"default":                      "Antigravity CLI (default model)",
	}
	for slug, want := range tests {
		got := humanizeModelName(slug)
		if got != want {
			t.Errorf("humanizeModelName(%q) = %q, want %q", slug, got, want)
		}
	}
}

func TestStandardBuiltinModelsHaveCleanNames(t *testing.T) {
	for _, m := range standardBuiltinModels() {
		if m.DisplayName == "" || looksLikeModelSlug(m.DisplayName) {
			t.Fatalf("builtin model %s has ugly slug DisplayName %q", m.ID, m.DisplayName)
		}
		if m.Name != m.ID {
			t.Fatalf("builtin model %s has Name %q, want %q", m.ID, m.Name, m.ID)
		}
	}
}

func TestAgyArgs(t *testing.T) {
	cfg := defaultConfig()
	cfg.DangerouslySkipPermissions = true
	cfg.Sandbox = true
	opts := agyOptions{
		NativeModel:     "gemini-3.8-flash",
		OutputFormat:    "stream-json",
		ConversationID:  "test-conv-123",
		ReasoningEffort: "high",
	}
	got := agyArgs(cfg, opts)
	want := []string{
		"--output-format", "stream-json",
		"--print-timeout", "30m",
		"--disable-slash-commands",
		"--dangerously-skip-permissions",
		"--sandbox",
		"--model", "gemini-3.8-flash",
		"--conversation", "test-conv-123",
		"--effort", "high",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("agyArgs() = %#v, want %#v", got, want)
	}
}

func TestAgyArgsResolvesConflict(t *testing.T) {
	cfg := defaultConfig()
	cases := []struct {
		model  string
		effort string
		want   []string
	}{
		{
			model:  "gemini-3.8-flash-high",
			effort: "low",
			want: []string{
				"--output-format", "json",
				"--print-timeout", "30m",
				"--disable-slash-commands",
				"--model", "gemini-3.8-flash",
				"--effort", "low",
			},
		},
		{
			model:  "gemini-4-hydrogen-xhigh",
			effort: "max",
			want: []string{
				"--output-format", "json",
				"--print-timeout", "30m",
				"--disable-slash-commands",
				"--model", "gemini-4-hydrogen",
				"--effort", "max",
			},
		},
		{
			model:  "gemini-4-hydrogen-max",
			effort: "xhigh",
			want: []string{
				"--output-format", "json",
				"--print-timeout", "30m",
				"--disable-slash-commands",
				"--model", "gemini-4-hydrogen",
				"--effort", "xhigh",
			},
		},
	}
	for _, tc := range cases {
		got := agyArgs(cfg, agyOptions{
			NativeModel:     tc.model,
			ReasoningEffort: tc.effort,
		})
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("agyArgs(%q, %q) = %#v, want %#v", tc.model, tc.effort, got, tc.want)
		}
	}
}

func TestAgyArgsClaudeOmitsEffort(t *testing.T) {
	cfg := defaultConfig()
	opts := agyOptions{
		NativeModel:     "claude-sonnet-4-6",
		ReasoningEffort: "high",
	}
	got := agyArgs(cfg, opts)
	want := []string{
		"--output-format", "json",
		"--print-timeout", "30m",
		"--disable-slash-commands",
		"--model", "claude-sonnet-4-6",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("agyArgs() = %#v, want %#v", got, want)
	}
}

func TestResolveExecutionOptions(t *testing.T) {
	useTempModelCache(t)
	invalidateModels()
	t.Cleanup(invalidateModels)

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

	// 4. Built-in alias: antigravity/gemini-3.8-flash-high splits suffix into high effort and base model gemini-3.8-flash
	req4 := rpcExecutorRequest{
		Model:   "antigravity/gemini-3.8-flash-high",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}
	opts4 := resolveExecutionOptions(req4, cfg)
	if opts4.ReasoningEffort != "high" {
		t.Fatalf("expected implicit high effort from gemini-3.8-flash-high, got %q", opts4.ReasoningEffort)
	}
	if opts4.NativeModel != "gemini-3.8-flash" {
		t.Fatalf("expected native model gemini-3.8-flash, got %q", opts4.NativeModel)
	}

	// 5. Explicit model alias (gemini-3.8-flash-high) splits into base model and high effort, ignoring payload low effort
	req5 := rpcExecutorRequest{
		Model:   "agy/gemini-3.8-flash-high",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`),
	}
	opts5 := resolveExecutionOptions(req5, cfg)
	if opts5.ReasoningEffort != "high" {
		t.Fatalf("expected model alias to take priority for effort, got %q", opts5.ReasoningEffort)
	}
	if opts5.NativeModel != "gemini-3.8-flash" {
		t.Fatalf("expected native model gemini-3.8-flash, got %q", opts5.NativeModel)
	}

	// 6. Colon override on model with effort suffix: gemini-3.8-flash-high:low overrides effort to low and strips -high
	req6 := rpcExecutorRequest{
		Model:   "agy/gemini-3.8-flash-high:low",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}
	opts6 := resolveExecutionOptions(req6, cfg)
	if opts6.ReasoningEffort != "low" {
		t.Fatalf("expected colon override to set effort to 'low', got %q", opts6.ReasoningEffort)
	}
	if opts6.NativeModel != "gemini-3.8-flash" {
		t.Fatalf("expected native model gemini-3.8-flash after stripping conflicting suffix, got %q", opts6.NativeModel)
	}

	// 7. Base model gemini-3.8-flash with payload reasoning_effort
	req7 := rpcExecutorRequest{
		Model:   "agy/gemini-3.8-flash",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`),
	}
	opts7 := resolveExecutionOptions(req7, cfg)
	if opts7.ReasoningEffort != "low" {
		t.Fatalf("expected effort 'low' from payload, got %q", opts7.ReasoningEffort)
	}
	if opts7.NativeModel != "gemini-3.8-flash" {
		t.Fatalf("expected native model gemini-3.8-flash, got %q", opts7.NativeModel)
	}

	// 8. Base model without any effort provided falls back to model default (high)
	req8 := rpcExecutorRequest{
		Model:   "agy/gemini-3.8-flash",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}
	opts8 := resolveExecutionOptions(req8, defaultConfig())
	if opts8.ReasoningEffort != "high" {
		t.Fatalf("expected fallback effort 'high' for base gemini model, got %q", opts8.ReasoningEffort)
	}
	if opts8.NativeModel != "gemini-3.8-flash" {
		t.Fatalf("expected native model gemini-3.8-flash, got %q", opts8.NativeModel)
	}

	// 9. Claude model does not support effort and effort is cleared
	req9 := rpcExecutorRequest{
		Model:   "agy/claude-sonnet-4-6",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`),
	}
	opts9 := resolveExecutionOptions(req9, cfg)
	if opts9.ReasoningEffort != "" {
		t.Fatalf("expected effort to be cleared for claude model, got %q", opts9.ReasoningEffort)
	}
	if opts9.NativeModel != "claude-sonnet-4-6" {
		t.Fatalf("expected native model claude-sonnet-4-6, got %q", opts9.NativeModel)
	}

	// 10. CLIProxyAPI thinking suffix that the host passed through: agy/gemini-3.8-flash(low)
	req10 := rpcExecutorRequest{
		Model:   "agy/gemini-3.8-flash(low)",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`),
	}
	opts10 := resolveExecutionOptions(req10, cfg)
	if opts10.ReasoningEffort != "low" || opts10.NativeModel != "gemini-3.8-flash" {
		t.Fatalf("expected low effort on gemini-3.8-flash from (low) suffix, got %q on %q", opts10.ReasoningEffort, opts10.NativeModel)
	}

	// 11. A numeric budget suffix is stripped from the model and falls back to other sources
	req11 := rpcExecutorRequest{
		Model:   "agy/gemini-3.8-flash(8192)",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`),
	}
	opts11 := resolveExecutionOptions(req11, cfg)
	if opts11.ReasoningEffort != "medium" || opts11.NativeModel != "gemini-3.8-flash" {
		t.Fatalf("expected payload effort on gemini-3.8-flash, got %q on %q", opts11.ReasoningEffort, opts11.NativeModel)
	}

	// 12. Extended effort levels (xhigh, max) across model suffix, colon override, paren suffix, payload, header, and alias normalization
	extendedCases := []struct {
		name       string
		req        rpcExecutorRequest
		wantModel  string
		wantEffort string
	}{
		{
			name: "dash xhigh suffix",
			req: rpcExecutorRequest{
				Model:   "agy/gemini-4-hydrogen-xhigh",
				Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
			},
			wantModel:  "gemini-4-hydrogen",
			wantEffort: "xhigh",
		},
		{
			name: "dash extra-high alias suffix",
			req: rpcExecutorRequest{
				Model:   "agy/gemini-4-hydrogen-extra-high",
				Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
			},
			wantModel:  "gemini-4-hydrogen",
			wantEffort: "xhigh",
		},
		{
			name: "dash max suffix",
			req: rpcExecutorRequest{
				Model:   "agy/gemini-4-hydrogen-max",
				Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
			},
			wantModel:  "gemini-4-hydrogen",
			wantEffort: "max",
		},
		{
			name: "colon xhigh on default",
			req: rpcExecutorRequest{
				Model:   "agy/default:xhigh",
				Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
			},
			wantModel:  "",
			wantEffort: "xhigh",
		},
		{
			name: "colon max overrides dash suffix",
			req: rpcExecutorRequest{
				Model:   "agy/gemini-4-hydrogen-xhigh:max",
				Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
			},
			wantModel:  "gemini-4-hydrogen",
			wantEffort: "max",
		},
		{
			name: "paren xhigh suffix",
			req: rpcExecutorRequest{
				Model:   "agy/gemini-4-hydrogen(xhigh)",
				Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`),
			},
			wantModel:  "gemini-4-hydrogen",
			wantEffort: "xhigh",
		},
		{
			name: "paren max suffix",
			req: rpcExecutorRequest{
				Model:   "agy/gemini-4-hydrogen(max)",
				Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
			},
			wantModel:  "gemini-4-hydrogen",
			wantEffort: "max",
		},
		{
			name: "payload xhigh",
			req: rpcExecutorRequest{
				Model:   "agy/gemini-4-hydrogen",
				Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"xhigh"}`),
			},
			wantModel:  "gemini-4-hydrogen",
			wantEffort: "xhigh",
		},
		{
			name: "header x-high alias",
			req: func() rpcExecutorRequest {
				h := http.Header{}
				h.Set("X-AGY-Effort", "x-high")
				return rpcExecutorRequest{
					Model:   "agy/gemini-4-hydrogen",
					Headers: h,
					Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
				}
			}(),
			wantModel:  "gemini-4-hydrogen",
			wantEffort: "xhigh",
		},
		{
			name: "header max",
			req: func() rpcExecutorRequest {
				h := http.Header{}
				h.Set("X-Reasoning-Effort", "max")
				return rpcExecutorRequest{
					Model:   "agy/gemini-4-hydrogen",
					Headers: h,
					Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
				}
			}(),
			wantModel:  "gemini-4-hydrogen",
			wantEffort: "max",
		},
		{
			name: "claude clears max effort",
			req: rpcExecutorRequest{
				Model:   "agy/claude-opus-4-6-thinking",
				Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"max"}`),
			},
			wantModel:  "claude-opus-4-6-thinking",
			wantEffort: "",
		},
	}
	for _, tc := range extendedCases {
		got := resolveExecutionOptions(tc.req, cfg)
		if got.NativeModel != tc.wantModel || got.ReasoningEffort != tc.wantEffort {
			t.Errorf("%s: got (model=%q, effort=%q), want (%q, %q)", tc.name, got.NativeModel, got.ReasoningEffort, tc.wantModel, tc.wantEffort)
		}
	}
}

func TestErrorEnvelopeCarriesRetryAfter(t *testing.T) {
	raw := mustErrorEnvelope(&pluginError{Code: "agy_quota_exhausted", Message: "exhausted", HTTPStatus: http.StatusTooManyRequests, Retryable: true, RetryAfterSeconds: 90})
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code              string `json:"code"`
			HTTPStatus        int    `json:"http_status"`
			Retryable         bool   `json:"retryable"`
			RetryAfterSeconds int    `json:"retry_after_seconds"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error.Code != "agy_quota_exhausted" || env.Error.HTTPStatus != 429 || !env.Error.Retryable || env.Error.RetryAfterSeconds != 90 {
		t.Fatalf("unexpected envelope: %s", raw)
	}
}

func TestClassifyAgyErrorModelNotFoundUsesHostCode(t *testing.T) {
	pe := asPluginError(classifyAgyError(errors.New("invalid model selection: nope"), ""))
	if pe.Code != errorCodeModelNotFound || pe.HTTPStatus != http.StatusNotFound {
		t.Fatalf("expected %s/404, got %s/%d", errorCodeModelNotFound, pe.Code, pe.HTTPStatus)
	}
}

func TestClassifyAgyErrorUnsupportedEffortIsRequestScoped(t *testing.T) {
	cases := []string{
		`invalid model selection (--model "gemini-4-hydrogen" --effort "max"): gemini-4-hydrogen has no "max" effort (available: low, medium, high, xhigh)`,
		`invalid model selection (--model "gemini-3.8-flash" --effort "xhigh"): gemini-3.8-flash has no "xhigh" effort (available: low, medium, high)`,
		`invalid model selection (--model "gemini-4-hydrogen" --effort "invalid"): invalid --effort "invalid" (valid: low, medium, high, xhigh, max)`,
		`invalid model selection (--model "claude-sonnet-4-6" --effort "max"): --effort is not supported for model "claude-sonnet-4-6"`,
	}
	for _, msg := range cases {
		pe := asPluginError(classifyAgyError(errors.New(msg), ""))
		if pe.Code != errorCodeRequestScoped || pe.HTTPStatus != http.StatusBadRequest {
			t.Fatalf("classifyAgyError(%q) = %s/%d, want %s/400", msg, pe.Code, pe.HTTPStatus, errorCodeRequestScoped)
		}
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

func TestFinishPluginStreamPropagatesDoneEmitError(t *testing.T) {
	wantErr := errors.New("broken pipe")
	var gotCloseErr error
	var emitted [][]byte

	finishPluginStream("stream-1", false, func(_ string, payload []byte) error {
		emitted = append(emitted, append([]byte(nil), payload...))
		return wantErr
	}, func(_ string, err error) {
		gotCloseErr = err
	})
	if len(emitted) != 1 || string(emitted[0]) != "data: [DONE]\n\n" {
		t.Fatalf("emitted = %q, want data: [DONE]\\n\\n", emitted)
	}
	if !errors.Is(gotCloseErr, wantErr) {
		t.Fatalf("close err = %v, want %v", gotCloseErr, wantErr)
	}

	gotCloseErr = errors.New("unexpected")
	emitted = nil
	finishPluginStream("stream-2", true, func(_ string, payload []byte) error {
		emitted = append(emitted, payload)
		return wantErr
	}, func(_ string, err error) {
		gotCloseErr = err
	})
	if len(emitted) != 0 {
		t.Fatalf("raw stream must not emit [DONE], got %q", emitted)
	}
	if gotCloseErr != nil {
		t.Fatalf("raw stream close err = %v, want nil", gotCloseErr)
	}
}

// useTempModelCache isolates the on-disk catalog cache for a test. The cache root
// comes from os.UserCacheDir(), which honours different environment variables per
// platform, so the path is overridden directly instead.
func useTempModelCache(t *testing.T) {
	t.Helper()
	previous := modelCacheDirOverride
	modelCacheDirOverride = t.TempDir()
	t.Cleanup(func() { modelCacheDirOverride = previous })
}

func TestModelCatalogCacheRoundTrip(t *testing.T) {
	useTempModelCache(t)
	cfg := pluginConfig{BinaryPath: "agy"}
	models := []modelInfo{{ID: "agy/gemini-4-neon", Name: "Neon"}, {ID: "agy/default", Name: "Default"}}
	writeModelCatalogCache(cfg, models, map[string]string{"agy/gemini-4-neon": "gemini-4-neon"})

	gotModels, gotMapping := readModelCatalogCache(cfg)
	if len(gotModels) != 2 || gotModels[0].ID != "agy/gemini-4-neon" {
		t.Fatalf("cached models = %+v", gotModels)
	}
	if gotMapping["agy/gemini-4-neon"] != "gemini-4-neon" {
		t.Fatalf("cached mapping = %+v", gotMapping)
	}

	// A cache written for another binary must not apply.
	if other, _ := readModelCatalogCache(pluginConfig{BinaryPath: "other-agy"}); other != nil {
		t.Fatalf("cache leaked across binaries: %+v", other)
	}
}

func TestMergeCatalogModelsPrefersPrimary(t *testing.T) {
	primary := []modelInfo{{ID: "agy/default", Name: "fresh"}}
	extra := []modelInfo{{ID: "agy/default", Name: "stale"}, {ID: "agy/gemini-4-neon", Name: "cached"}}
	merged := mergeCatalogModels(primary, extra)
	if len(merged) != 2 || merged[0].Name != "fresh" {
		t.Fatalf("merged = %+v", merged)
	}
	mapping := mergeCatalogMapping(map[string]string{"agy/default": "fresh-native"}, map[string]string{"agy/default": "stale-native", "agy/other": "y"})
	if mapping["agy/default"] != "fresh-native" || mapping["agy/other"] != "y" {
		t.Fatalf("mapping = %+v", mapping)
	}
}

// A rate-limited account makes `agy models` fail; the catalog must keep the
// discovered models, otherwise every agy/* request 400s on an unknown model.
func TestDiscoverModelsFallsBackToCachedCatalog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stub agy binary is a shell script")
	}
	useTempModelCache(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "agy")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := pluginConfig{BinaryPath: bin}
	writeModelCatalogCache(cfg, []modelInfo{{ID: "agy/gemini-4-neon", Name: "Neon"}},
		map[string]string{"agy/gemini-4-neon": "gemini-4-neon"})

	invalidateModels()
	defer invalidateModels()
	models := discoverModels(cfg)
	for _, model := range models {
		if model.ID == "agy/gemini-4-neon" {
			return
		}
	}
	t.Fatalf("cached model missing after a failed discovery: %+v", models)
}

func TestModelCatalogCacheSkipsDiskWithoutUserCacheDir(t *testing.T) {
	previous := modelCacheDirOverride
	modelCacheDirOverride = ""
	t.Cleanup(func() { modelCacheDirOverride = previous })
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	if os.Getenv("LOCALAPPDATA") != "" {
		t.Setenv("LOCALAPPDATA", "")
	}
	if path, ok := modelCatalogCachePath(); ok {
		t.Fatalf("expected no cache path without a user cache dir, got %q", path)
	}
	writeModelCatalogCache(pluginConfig{BinaryPath: "agy"}, []modelInfo{{ID: "agy/default"}}, nil)
	if models, _ := readModelCatalogCache(pluginConfig{BinaryPath: "agy"}); models != nil {
		t.Fatalf("expected no disk cache read, got %+v", models)
	}
}

func TestWriteModelCatalogCacheKeepsOldFileOnInvalidPayload(t *testing.T) {
	useTempModelCache(t)
	cfg := pluginConfig{BinaryPath: "agy"}
	writeModelCatalogCache(cfg, []modelInfo{{ID: "agy/default", Name: "first"}}, nil)
	writeModelCatalogCache(cfg, []modelInfo{{ID: "agy/default", Name: "second"}}, nil)
	models, _ := readModelCatalogCache(cfg)
	if len(models) != 1 || models[0].Name != "second" {
		t.Fatalf("cache = %+v, want the replaced entry", models)
	}
	// No temporary files may be left behind.
	dir := filepath.Dir(mustCachePath(t))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "models-") && strings.HasSuffix(entry.Name(), ".json") {
			t.Fatalf("temporary cache file left behind: %s", entry.Name())
		}
	}
}

func mustCachePath(t *testing.T) string {
	t.Helper()
	path, ok := modelCatalogCachePath()
	if !ok {
		t.Fatal("no cache path")
	}
	return path
}
