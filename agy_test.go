package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
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
	}
	if byID["agy/gemini-3.6-flash-high"].Name != "Gemini 3.6 Flash (High)" {
		t.Fatalf("unexpected Name for gemini-3.6-flash-high: %q", byID["agy/gemini-3.6-flash-high"].Name)
	}
	if byID["agy/gemini-3.6-flash"].Name != "Gemini 3.6 Flash" {
		t.Fatalf("unexpected Name for base gemini-3.6-flash: %q", byID["agy/gemini-3.6-flash"].Name)
	}
	if byID["agy/gemini-4-hydrogen-xhigh"].Name != "Gemini 4 Hydrogen (XHigh)" {
		t.Fatalf("unexpected Name for gemini-4-hydrogen-xhigh: %q", byID["agy/gemini-4-hydrogen-xhigh"].Name)
	}
	if byID["agy/gemini-4-hydrogen"].Name != "Gemini 4 Hydrogen" {
		t.Fatalf("unexpected Name for base gemini-4-hydrogen: %q", byID["agy/gemini-4-hydrogen"].Name)
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
	if rawByID["agy/gemini-3.8-flash"].Name != "Gemini 3.8 Flash" {
		t.Fatalf("unexpected humanized Name for raw base slug: %q", rawByID["agy/gemini-3.8-flash"].Name)
	}
	if rawByID["agy/gemini-4-hydrogen"].Name != "Gemini 4 Hydrogen" || rawByID["agy/gemini-4-hydrogen-max"].Name != "Gemini 4 Hydrogen (Max)" {
		t.Fatalf("unexpected humanized Name for raw max slug: %#v", rawByID)
	}
	if rawByID["agy/gpt-oss-120b"].Name != "GPT-OSS 120B" {
		t.Fatalf("unexpected humanized Name for raw base slug: %q", rawByID["agy/gpt-oss-120b"].Name)
	}
	if rawByID["agy/claude-sonnet-4-6"].Name != "Claude Sonnet 4.6" {
		t.Fatalf("unexpected humanized Name for raw claude slug: %q", rawByID["agy/claude-sonnet-4-6"].Name)
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
		if m.Name == "" || looksLikeModelSlug(m.Name) {
			t.Fatalf("builtin model %s has ugly slug name %q", m.ID, m.Name)
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

func TestRouteModel(t *testing.T) {
	invalidateModels()
	cases := []struct {
		model     string
		providers []string
		want      bool
	}{
		{"agy/gemini-3.8-flash", nil, true},
		{"agy/gemini-3.8-flash(high)", nil, true},
		{"agy/gemini-4-hydrogen(xhigh)", nil, true},
		{"agy/default:high", nil, true},
		{"agy/default:xhigh", nil, true},
		{"agy/default:max", nil, true},
		{"gemini-3.8-flash-high", nil, true},
		{"gemini-3.8-flash-xhigh", nil, true},
		{"gemini-3.8-flash-max", nil, true},
		{"gemini-3.8-flash-extra-high", nil, true},
		{"gemini-3.8-flash:xhigh", nil, true},
		{"gemini-3.8-flash:max", nil, true},
		{"antigravity/gemini-3.8-flash", nil, true},
		{"antigravity/gemini-3.8-flash", []string{"antigravity"}, false},
		{"gpt-5.5", nil, false},
		{"gpt-5.5-xhigh", nil, false},
		{"gpt-5.5-max", nil, false},
		{"agy/gemini-3.8-flash", []string{"agy"}, false},
	}
	for _, tc := range cases {
		resp := routeModel(modelRouteRequest{RequestedModel: tc.model, AvailableProviders: tc.providers})
		if resp.Handled != tc.want || (tc.want && resp.TargetKind != "self") {
			t.Errorf("routeModel(%q, %v) = %+v, want handled=%v", tc.model, tc.providers, resp, tc.want)
		}
	}
}
