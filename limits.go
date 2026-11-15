package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Antigravity reports per-account usage limits through the `/usage` slash command:
// models are grouped, and every group has a weekly and a five-hour bucket.

type agyLimitBucket struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	Window            string   `json:"window"`
	Disabled          bool     `json:"disabled"`
	RemainingFraction *float64 `json:"remaining_fraction"`
	ResetTime         string   `json:"reset_time"`
}

type agyLimitGroup struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Buckets     []agyLimitBucket `json:"buckets"`
}

type agyLimits struct {
	Description string          `json:"description"`
	Groups      []agyLimitGroup `json:"groups"`
}

type agyUsageCommandOutput struct {
	Status  string `json:"status"`
	Error   string `json:"error"`
	Command *struct {
		Name string    `json:"name"`
		Data agyLimits `json:"data"`
	} `json:"command"`
}

// exhausted reports whether an enabled bucket has no quota left and has not reset yet.
func (b agyLimitBucket) exhausted(now time.Time) bool {
	if b.Disabled || b.RemainingFraction == nil || *b.RemainingFraction > 0 {
		return false
	}
	if reset, ok := b.resetAt(); ok && !reset.After(now) {
		return false
	}
	return true
}

func (b agyLimitBucket) resetAt() (time.Time, bool) {
	if strings.TrimSpace(b.ResetTime) == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, b.ResetTime)
	return t, err == nil
}

// groupModelSlugs returns slugified model names from "Models within this group: A, B, C".
func (g agyLimitGroup) groupModelSlugs() []string {
	desc := g.Description
	if idx := strings.IndexByte(desc, ':'); idx >= 0 {
		desc = desc[idx+1:]
	}
	var slugs []string
	for _, name := range strings.Split(desc, ",") {
		if slug := slugifyModelLabel(stripEffortLabel(name)); slug != "" {
			slugs = append(slugs, slug)
		}
	}
	return slugs
}

// groupForModel finds the limit group whose model list matches the native agy model,
// e.g. "claude-opus-4-6-thinking" belongs to the group listing "Claude Opus 4.6".
func (l *agyLimits) groupForModel(nativeModel string) *agyLimitGroup {
	if l == nil {
		return nil
	}
	slug := slugifyModelLabel(nativeModel)
	if slug == "" {
		return nil
	}
	var best *agyLimitGroup
	bestLen := 0
	for i := range l.Groups {
		for _, candidate := range l.Groups[i].groupModelSlugs() {
			if (slug == candidate || strings.HasPrefix(slug, candidate+"-")) && len(candidate) > bestLen {
				best = &l.Groups[i]
				bestLen = len(candidate)
			}
		}
	}
	return best
}

func parseAgyUsageOutput(raw []byte) (agyLimits, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return agyLimits{}, fmt.Errorf("empty stdout")
	}
	var out agyUsageCommandOutput
	if err := json.Unmarshal(trimmed, &out); err != nil {
		// Fall back to the last JSON line if agy printed anything before the result.
		lines := bytes.Split(trimmed, []byte("\n"))
		if errLine := json.Unmarshal(bytes.TrimSpace(lines[len(lines)-1]), &out); errLine != nil {
			return agyLimits{}, fmt.Errorf("decode usage output: %w", err)
		}
	}
	if !strings.EqualFold(out.Status, "SUCCESS") {
		return agyLimits{}, fmt.Errorf("usage command ended with status %q: %s", out.Status, out.Error)
	}
	if out.Command == nil || len(out.Command.Data.Groups) == 0 {
		return agyLimits{}, fmt.Errorf("usage command returned no limit groups")
	}
	return out.Command.Data, nil
}

func fetchAgyLimits(cfg pluginConfig) (agyLimits, error) {
	if err := validateBinaryPath(cfg.BinaryPath); err != nil {
		return agyLimits{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	// Slash commands are deliberately enabled here: the prompt is our fixed "/usage", never client text.
	cmd := exec.CommandContext(ctx, cfg.BinaryPath, "--output-format", "json", "--print-timeout", "30s")
	if cfg.Workdir != "" {
		cmd.Dir = cfg.Workdir
	}
	configureSysProcAttr(cmd)
	cmd.Stdin = strings.NewReader("/usage\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return agyLimits{}, classifyAgyError(err, stderr.String())
	}
	activeProcesses.add(cmd)
	errWait := cmd.Wait()
	activeProcesses.remove(cmd)

	limits, err := parseAgyUsageOutput(stdout.Bytes())
	if err != nil {
		if errWait != nil {
			return agyLimits{}, classifyAgyError(errWait, stderr.String())
		}
		return agyLimits{}, err
	}
	return limits, nil
}

type limitsCache struct {
	sync.Mutex
	limits     *agyLimits
	fetchedAt  time.Time
	binaryPath string
	lastErr    error
	stale      bool
	refreshing bool
}

var limitsState = limitsCache{}

// fetchLimitsFunc is swapped out in tests.
var fetchLimitsFunc = fetchAgyLimits

func limitsRefreshInterval(cfg pluginConfig) time.Duration {
	d, err := time.ParseDuration(cfg.LimitsRefreshInterval)
	if err != nil || d <= 0 {
		return time.Minute
	}
	return d
}

func invalidateLimits() {
	limitsState.Lock()
	limitsState.limits = nil
	limitsState.fetchedAt = time.Time{}
	limitsState.binaryPath = ""
	limitsState.lastErr = nil
	limitsState.stale = false
	limitsState.Unlock()
}

// markLimitsStale forces the next lookup to refresh, e.g. after agy reports a quota error.
func markLimitsStale() {
	limitsState.Lock()
	limitsState.stale = true
	limitsState.Unlock()
}

func (c *limitsCache) needsRefreshLocked(cfg pluginConfig) bool {
	return c.stale || c.binaryPath != cfg.BinaryPath || time.Since(c.fetchedAt) >= limitsRefreshInterval(cfg)
}

func (c *limitsCache) store(cfg pluginConfig, limits agyLimits, err error) {
	c.Lock()
	defer c.Unlock()
	binaryChanged := c.binaryPath != cfg.BinaryPath
	c.refreshing = false
	c.fetchedAt = time.Now()
	c.binaryPath = cfg.BinaryPath
	c.stale = false
	c.lastErr = err
	if err == nil {
		c.limits = &limits
	} else if binaryChanged {
		c.limits = nil
	}
}

// currentLimits returns the cached limits, refreshing synchronously when they are stale.
func currentLimits(cfg pluginConfig) (*agyLimits, time.Time, error) {
	limitsState.Lock()
	if !limitsState.needsRefreshLocked(cfg) {
		limits, fetchedAt, lastErr := limitsState.limits, limitsState.fetchedAt, limitsState.lastErr
		limitsState.Unlock()
		return limits, fetchedAt, lastErr
	}
	limitsState.refreshing = true
	limitsState.Unlock()

	limits, err := fetchLimitsFunc(cfg)
	limitsState.store(cfg, limits, err)

	limitsState.Lock()
	defer limitsState.Unlock()
	return limitsState.limits, limitsState.fetchedAt, limitsState.lastErr
}

// cachedLimits never blocks on agy: it returns what is cached and refreshes in the background.
func cachedLimits(cfg pluginConfig) *agyLimits {
	limitsState.Lock()
	defer limitsState.Unlock()
	if limitsState.needsRefreshLocked(cfg) && !limitsState.refreshing {
		limitsState.refreshing = true
		go func() {
			limits, err := fetchLimitsFunc(cfg)
			limitsState.store(cfg, limits, err)
		}()
	}
	if limitsState.binaryPath != cfg.BinaryPath {
		return nil
	}
	return limitsState.limits
}

// checkModelLimits rejects a request up front when the model's quota group is known to be exhausted.
func checkModelLimits(cfg pluginConfig, nativeModel string) error {
	if !cfg.LimitsTracking || !cfg.EnforceLimits {
		return nil
	}
	group := cachedLimits(cfg).groupForModel(nativeModel)
	if group == nil {
		return nil
	}
	now := time.Now()
	for _, b := range group.Buckets {
		if !b.exhausted(now) {
			continue
		}
		msg := fmt.Sprintf("Antigravity %s limit for %s is exhausted", nonEmpty(b.Window, b.ID), group.Name)
		retryAfter := 0
		if reset, ok := b.resetAt(); ok {
			msg += fmt.Sprintf("; resets at %s (in %s)", reset.UTC().Format(time.RFC3339), reset.Sub(now).Round(time.Minute))
			retryAfter = int(math.Ceil(reset.Sub(now).Seconds()))
		}
		return &pluginError{Code: "agy_quota_exhausted", Message: msg, HTTPStatus: http.StatusTooManyRequests, Retryable: true, RetryAfterSeconds: retryAfter}
	}
	return nil
}

// limitHeaders describes the quota group of a model as response headers.
func limitHeaders(cfg pluginConfig, nativeModel string) http.Header {
	if !cfg.LimitsTracking {
		return nil
	}
	group := cachedLimits(cfg).groupForModel(nativeModel)
	if group == nil {
		return nil
	}
	h := http.Header{}
	h.Set("X-AGY-Limit-Group", group.Name)
	for _, b := range group.Buckets {
		window := strings.TrimSpace(b.Window)
		if window == "" {
			window = b.ID
		}
		prefix := "X-AGY-Limit-" + http.CanonicalHeaderKey(window)
		switch {
		case b.Disabled:
			h.Set(prefix+"-Remaining", "disabled")
		case b.RemainingFraction != nil:
			h.Set(prefix+"-Remaining", formatPercent(*b.RemainingFraction))
		}
		if reset, ok := b.resetAt(); ok {
			h.Set(prefix+"-Reset", reset.UTC().Format(time.RFC3339))
		}
	}
	return h
}

func formatPercent(fraction float64) string {
	return strconv.FormatFloat(math.Round(fraction*1000)/10, 'f', -1, 64) + "%"
}

func mergeHeaders(dst, src http.Header) {
	for k, values := range src {
		for _, v := range values {
			dst.Add(k, v)
		}
	}
}

const limitsManagementPath = "/agy/limits"

// Management API wire types. The host's structs carry no json tags, so Go field names are the keys.
type managementRegistrationResponse struct {
	Routes []managementRoute `json:"routes,omitempty"`
}

type managementRoute struct {
	Method      string
	Path        string
	Description string
}

type managementRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Query   map[string][]string
	Body    []byte
}

type managementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

type limitsReport struct {
	Provider    string          `json:"provider"`
	Tracking    bool            `json:"tracking"`
	FetchedAt   string          `json:"fetched_at,omitempty"`
	Description string          `json:"description,omitempty"`
	Groups      []agyLimitGroup `json:"groups"`
	Error       string          `json:"error,omitempty"`
}

func handleManagement(req managementRequest) managementResponse {
	path := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(req.Path), "/v0/management"), "/")
	if path != limitsManagementPath {
		return jsonManagementResponse(http.StatusNotFound, map[string]string{"error": "unknown cliproxy-antigravity management route"})
	}
	if method := strings.ToUpper(strings.TrimSpace(req.Method)); method != "" && method != http.MethodGet {
		return jsonManagementResponse(http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}

	cfg := currentConfig()
	report := limitsReport{Provider: providerID, Tracking: cfg.LimitsTracking, Groups: []agyLimitGroup{}}
	if !cfg.LimitsTracking {
		return jsonManagementResponse(http.StatusOK, report)
	}
	if refresh := firstQueryValue(req.Query, "refresh"); refresh != "" && refresh != "0" && !strings.EqualFold(refresh, "false") {
		markLimitsStale()
	}
	limits, fetchedAt, err := currentLimits(cfg)
	if limits != nil {
		report.Description = limits.Description
		report.Groups = limits.Groups
	}
	if !fetchedAt.IsZero() {
		report.FetchedAt = fetchedAt.UTC().Format(time.RFC3339)
	}
	status := http.StatusOK
	if err != nil {
		report.Error = err.Error()
		if limits == nil {
			status = http.StatusBadGateway
		}
	}
	return jsonManagementResponse(status, report)
}

func firstQueryValue(query map[string][]string, key string) string {
	if values := query[key]; len(values) > 0 {
		return strings.TrimSpace(values[0])
	}
	return ""
}

func jsonManagementResponse(status int, body any) managementResponse {
	raw, _ := json.Marshal(body)
	return managementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       raw,
	}
}

// Quota provider wire types (CLIProxyAPI's pluginapi.Quota* structs).
type quotaDescribeResponse struct {
	SupportedProviders []string `json:"supported_providers,omitempty"`
	DisplayName        string   `json:"display_name,omitempty"`
	SupportsReset      bool     `json:"supports_reset,omitempty"`
}

type quotaFetchResponse struct {
	Groups []quotaGroup `json:"groups,omitempty"`
}

type quotaGroup struct {
	DisplayName string        `json:"displayName,omitempty"`
	Buckets     []quotaBucket `json:"buckets,omitempty"`
}

type quotaBucket struct {
	Window            string  `json:"window,omitempty"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
	Description       string  `json:"description,omitempty"`
}

type quotaResetResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

func quotaFetchFromLimits(limits *agyLimits) quotaFetchResponse {
	var out quotaFetchResponse
	for _, g := range limits.Groups {
		group := quotaGroup{DisplayName: g.Name}
		for _, b := range g.Buckets {
			remaining := 1.0
			if b.RemainingFraction != nil && !b.Disabled {
				remaining = *b.RemainingFraction
			}
			group.Buckets = append(group.Buckets, quotaBucket{
				Window:            nonEmpty(b.Window, b.ID),
				RemainingFraction: remaining,
				ResetTime:         b.ResetTime,
				Description:       nonEmpty(b.Description, b.Name),
			})
		}
		out.Groups = append(out.Groups, group)
	}
	return out
}
