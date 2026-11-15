package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

const usageFixture = `{"conversation_id":"","status":"SUCCESS","response":"...","duration_seconds":0,"num_turns":0,"usage":{"input_tokens":0,"output_tokens":0,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":0},"command":{"name":"usage","data":{"description":"Within each group, models share a weekly limit and a 5-hour limit.","groups":[{"name":"Gemini Models","description":"Models within this group: Gemini 3.8 Flash","buckets":[{"id":"gemini-weekly","name":"Weekly Limit Remaining","description":"You have used some of your weekly limit.","window":"weekly","remaining_fraction":0.925955057144165,"reset_time":"2099-11-21T06:20:27Z"},{"id":"gemini-5h","name":"Five Hour Limit Remaining","window":"5h","remaining_fraction":0.9974474906921387,"reset_time":"2099-11-15T19:16:01Z"}]},{"name":"Claude and GPT models","description":"Models within this group: Claude Sonnet 4.6, Claude Opus 4.6, GPT-OSS 120B","buckets":[{"id":"3p-weekly","name":"Weekly Limit Remaining","window":"weekly","remaining_fraction":0,"reset_time":"2099-11-19T02:31:24Z"},{"id":"3p-5h","name":"Five Hour Limit Remaining","window":"5h","disabled":true,"remaining_fraction":1}]}]}}}`

func fixtureLimits(t *testing.T) agyLimits {
	t.Helper()
	limits, err := parseAgyUsageOutput([]byte(usageFixture))
	if err != nil {
		t.Fatalf("parseAgyUsageOutput: %v", err)
	}
	return limits
}

func stubLimits(t *testing.T, limits agyLimits, err error) *int {
	t.Helper()
	calls := 0
	prev := fetchLimitsFunc
	fetchLimitsFunc = func(pluginConfig) (agyLimits, error) {
		calls++
		return limits, err
	}
	invalidateLimits()
	t.Cleanup(func() {
		fetchLimitsFunc = prev
		invalidateLimits()
	})
	return &calls
}

func TestParseAgyUsageOutput(t *testing.T) {
	limits := fixtureLimits(t)
	if len(limits.Groups) != 2 || len(limits.Groups[1].Buckets) != 2 {
		t.Fatalf("unexpected groups: %#v", limits.Groups)
	}
	if _, err := parseAgyUsageOutput([]byte(`{"status":"ERROR","error":"boom"}`)); err == nil {
		t.Fatal("expected error for failed usage command")
	}
}

func TestGroupForModel(t *testing.T) {
	limits := fixtureLimits(t)
	tests := map[string]string{
		"gemini-3.8-flash-high":    "Gemini Models",
		"gemini-3.8-flash":         "Gemini Models",
		"claude-sonnet-4-6":        "Claude and GPT models",
		"claude-opus-4-6-thinking": "Claude and GPT models",
		"gpt-oss-120b-medium":      "Claude and GPT models",
	}
	for model, want := range tests {
		group := limits.groupForModel(model)
		if group == nil || group.Name != want {
			t.Errorf("groupForModel(%q) = %v, want %q", model, group, want)
		}
	}
	if group := limits.groupForModel(""); group != nil {
		t.Errorf("default model should not map to a group, got %q", group.Name)
	}
	if group := limits.groupForModel("gemini-3.80-pro"); group != nil {
		t.Errorf("prefix match must respect slug boundaries, got %q", group.Name)
	}
}

func TestBucketExhausted(t *testing.T) {
	limits := fixtureLimits(t)
	now := time.Now()
	weekly, fiveHour := limits.Groups[1].Buckets[0], limits.Groups[1].Buckets[1]
	if !weekly.exhausted(now) {
		t.Fatal("0% weekly bucket should be exhausted")
	}
	if fiveHour.exhausted(now) {
		t.Fatal("disabled bucket must never count as exhausted")
	}
	if weekly.exhausted(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("bucket past its reset time must not count as exhausted")
	}
}

func TestCheckModelLimitsEnforcement(t *testing.T) {
	stubLimits(t, fixtureLimits(t), nil)
	cfg := defaultConfig()
	if _, _, err := currentLimits(cfg); err != nil {
		t.Fatalf("currentLimits: %v", err)
	}

	if err := checkModelLimits(cfg, "claude-sonnet-4-6"); err != nil {
		t.Fatalf("enforcement is opt-in, got %v", err)
	}
	cfg.EnforceLimits = true
	err := checkModelLimits(cfg, "claude-sonnet-4-6")
	pe := asPluginError(err)
	if err == nil || pe.HTTPStatus != http.StatusTooManyRequests || !pe.Retryable || pe.Code != "agy_quota_exhausted" {
		t.Fatalf("expected retryable 429, got %#v", pe)
	}
	if !strings.Contains(pe.Message, "Claude and GPT models") || !strings.Contains(pe.Message, "2099-11-19T02:31:24Z") || !strings.Contains(pe.Message, "weekly limit for") {
		t.Fatalf("message should name the group and reset time: %q", pe.Message)
	}
	if err := checkModelLimits(cfg, "gemini-3.8-flash"); err != nil {
		t.Fatalf("gemini group has quota left, got %v", err)
	}
}

func TestLimitHeaders(t *testing.T) {
	stubLimits(t, fixtureLimits(t), nil)
	cfg := defaultConfig()
	_, _, _ = currentLimits(cfg)

	h := limitHeaders(cfg, "gemini-3.8-flash-low")
	if h.Get("X-AGY-Limit-Group") != "Gemini Models" {
		t.Fatalf("unexpected headers: %#v", h)
	}
	if h.Get("X-AGY-Limit-Weekly-Remaining") != "92.6%" || h.Get("X-AGY-Limit-5h-Remaining") != "99.7%" {
		t.Fatalf("unexpected remaining headers: %#v", h)
	}
	if h.Get("X-AGY-Limit-Weekly-Reset") != "2099-11-21T06:20:27Z" {
		t.Fatalf("unexpected reset header: %#v", h)
	}
	if got := limitHeaders(cfg, "claude-sonnet-4-6").Get("X-AGY-Limit-5h-Remaining"); got != "disabled" {
		t.Fatalf("disabled bucket header = %q", got)
	}

	cfg.LimitsTracking = false
	if h := limitHeaders(cfg, "gemini-3.8-flash"); h != nil {
		t.Fatalf("tracking disabled should emit no headers: %#v", h)
	}
}

func TestLimitsCacheRefresh(t *testing.T) {
	calls := stubLimits(t, fixtureLimits(t), nil)
	cfg := defaultConfig()
	_, _, _ = currentLimits(cfg)
	_, _, _ = currentLimits(cfg)
	if *calls != 1 {
		t.Fatalf("fresh cache should not refetch, calls = %d", *calls)
	}
	markLimitsStale()
	_, _, _ = currentLimits(cfg)
	if *calls != 2 {
		t.Fatalf("stale cache should refetch, calls = %d", *calls)
	}
}

func TestHandleManagementLimits(t *testing.T) {
	stubLimits(t, fixtureLimits(t), nil)
	resp := handleManagement(managementRequest{Method: "GET", Path: "/v0/management/agy/limits"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, resp.Body)
	}
	var report limitsReport
	if err := json.Unmarshal(resp.Body, &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report.Provider != "agy" || len(report.Groups) != 2 || report.FetchedAt == "" {
		t.Fatalf("unexpected report: %#v", report)
	}

	if resp := handleManagement(managementRequest{Method: "GET", Path: "/v0/management/agy/other"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route status = %d", resp.StatusCode)
	}
}

func TestHandleManagementLimitsFetchError(t *testing.T) {
	stubLimits(t, agyLimits{}, errors.New("agy not logged in"))
	resp := handleManagement(managementRequest{Method: "GET", Path: "/agy/limits"})
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(resp.Body), "agy not logged in") {
		t.Fatalf("status = %d body=%s", resp.StatusCode, resp.Body)
	}
}

func TestQuotaFetchFromLimits(t *testing.T) {
	limits := fixtureLimits(t)
	out := quotaFetchFromLimits(&limits)
	if len(out.Groups) != 2 || out.Groups[1].DisplayName != "Claude and GPT models" {
		t.Fatalf("unexpected groups: %#v", out.Groups)
	}
	if b := out.Groups[1].Buckets[0]; b.Window != "weekly" || b.RemainingFraction != 0 {
		t.Fatalf("unexpected weekly bucket: %#v", b)
	}
	if b := out.Groups[1].Buckets[1]; b.RemainingFraction != 1 {
		t.Fatalf("disabled bucket should report full remaining: %#v", b)
	}
}

func TestClassifyAgyErrorStructured(t *testing.T) {
	stderr := "error: Individual quota reached. Resets in 84h11m49s.\n" +
		`AGY_ERROR: {"short_error":"RESOURCE_EXHAUSTED (code 429): Individual quota reached.","status":"RESOURCE_EXHAUSTED","error_code":429,"code_kind":"http","retryable":true}` + "\n"
	pe := asPluginError(classifyAgyError(errors.New("Individual quota reached. Resets in 84h11m49s."), stderr))
	if pe.Code != "agy_rate_limit" || pe.HTTPStatus != http.StatusTooManyRequests || !pe.Retryable {
		t.Fatalf("unexpected error: %#v", pe)
	}
	if strings.Contains(pe.Message, "AGY_ERROR") {
		t.Fatalf("structured line leaked into message: %q", pe.Message)
	}
	if strings.Count(pe.Message, "Individual quota reached") != 1 {
		t.Fatalf("duplicated message: %q", pe.Message)
	}

	timeout := `AGY_ERROR: {"short_error":"print timeout after 30s","status":"DEADLINE_EXCEEDED","retryable":false,"reason":"print_timeout"}`
	if pe := asPluginError(classifyAgyError(errors.New("print timeout after 30s"), timeout)); pe.HTTPStatus != http.StatusGatewayTimeout {
		t.Fatalf("timeout status = %d", pe.HTTPStatus)
	}
}

func TestClassifyAgyErrorUnknownModel(t *testing.T) {
	msg := `invalid model selection (--model "nope" --effort ""): model nope is not recognized as a known model or custom model in settings`
	if pe := asPluginError(classifyAgyError(errors.New(msg), "")); pe.HTTPStatus != http.StatusNotFound {
		t.Fatalf("unknown model status = %d (%s)", pe.HTTPStatus, pe.Code)
	}
}

func TestToolCallPrefixState(t *testing.T) {
	tests := map[string]toolPrefixState{
		"":                  toolPrefixPending,
		"  ":                toolPrefixPending,
		"`":                 toolPrefixPending,
		"```tool":           toolPrefixPending,
		"```tool_calls\n[":  toolPrefixMatch,
		"```json":           toolPrefixMatch,
		"\n[{":              toolPrefixMatch,
		"Hello":             toolPrefixNone,
		"```python\nprint)": toolPrefixNone,
	}
	for text, want := range tests {
		if got := toolCallPrefixState(text); got != want {
			t.Errorf("toolCallPrefixState(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestParsePluginConfigLimits(t *testing.T) {
	cfg, err := parsePluginConfig([]byte("limits_tracking: false\nlimits-refresh-interval: 5m\nenforce_limits: true\n"))
	if err != nil {
		t.Fatalf("parsePluginConfig: %v", err)
	}
	if cfg.LimitsTracking || cfg.LimitsRefreshInterval != "5m" || !cfg.EnforceLimits {
		t.Fatalf("unexpected cfg: %#v", cfg)
	}
	if _, err := parsePluginConfig([]byte("limits_refresh_interval: 1s\n")); err == nil {
		t.Fatal("expected error for too-short refresh interval")
	}
	if def := defaultConfig(); !def.LimitsTracking || def.EnforceLimits {
		t.Fatalf("unexpected defaults: %#v", def)
	}
}
