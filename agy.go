package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

const catalogTTL = time.Minute

var catalogRetryAfter = 10 * time.Second

type catalogEntry struct {
	home       string
	fetchedAt  time.Time
	failedAt   time.Time
	models     []modelInfo
	nativeByID map[string]string
	loading    chan struct{}
}

type catalogState struct {
	sync.Mutex
	entries map[string]*catalogEntry
}

var modelCatalog = catalogState{entries: map[string]*catalogEntry{}}

func catalogKey(cfg pluginConfig) string {
	return strings.Join([]string{cfg.BinaryPath, cfg.Mode, cfg.AccountHome}, "\x00")
}

// modelCatalogDiskCache is the last discovery that succeeded. `agy models` fails
// while an account is rate limited, when the CLI times out, or on a transient
// network error, and publishing a catalog without the discovered models makes
// every agy/* request fail with an unknown model, so a failed refresh falls back
// to the previous catalog and to this cache.
type modelCatalogDiskCache struct {
	BinaryPath  string            `json:"binary_path"`
	AccountHome string            `json:"account_home,omitempty"`
	Models      []modelInfo       `json:"models"`
	NativeByID  map[string]string `json:"native_by_id"`
}

// modelCacheDirOverride lets tests point the cache at a temporary directory; the
// platform cache root differs per OS, so redirecting $HOME/XDG_CACHE_HOME is not
// portable.
var modelCacheDirOverride string

// modelCatalogCachePath returns the cached catalog path. It reports false when no
// per-user cache directory exists: falling back to os.TempDir() would put a
// predictable path in a shared directory where another local user could pre-create
// a symlink or FIFO for the plugin to follow or block on.
func modelCatalogCachePath() (string, bool) {
	if modelCacheDirOverride != "" {
		return filepath.Join(modelCacheDirOverride, pluginID, "models.json"), true
	}
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		return "", false
	}
	return filepath.Join(dir, pluginID, "models.json"), true
}

func modelCatalogCachePathFor(cfg pluginConfig) (string, bool) {
	base, ok := modelCatalogCachePath()
	if !ok {
		return "", false
	}
	if cfg.AccountHome == "" {
		return base, true
	}
	sum := sha256.Sum256([]byte(cfg.AccountHome))
	return filepath.Join(filepath.Dir(base), "accounts", hex.EncodeToString(sum[:6])+".json"), true
}

// writeModelCatalogCache persists a discovery. Only call it after `agy models`
// exited successfully; a partial catalog would otherwise outlive the failure.
func writeModelCatalogCache(cfg pluginConfig, models []modelInfo, nativeByID map[string]string) {
	path, ok := modelCatalogCachePathFor(cfg)
	if !ok {
		return
	}
	payload, err := json.Marshal(modelCatalogDiskCache{
		BinaryPath:  cfg.BinaryPath,
		AccountHome: cfg.AccountHome,
		Models:      models,
		NativeByID:  nativeByID,
	})
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	// Write beside the target and rename: a partial write must not destroy the
	// last valid catalog.
	tmp, err := os.CreateTemp(dir, "models-*.json")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
	}
}

func readModelCatalogCache(cfg pluginConfig) ([]modelInfo, map[string]string) {
	path, ok := modelCatalogCachePathFor(cfg)
	if !ok {
		return nil, nil
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	var cached modelCatalogDiskCache
	if err := json.Unmarshal(payload, &cached); err != nil {
		return nil, nil
	}
	if cached.BinaryPath != cfg.BinaryPath || cached.AccountHome != cfg.AccountHome || len(cached.Models) == 0 {
		return nil, nil
	}
	return cached.Models, cached.NativeByID
}

// mergeCatalogModels keeps the first entry per ID so a stale cache cannot shadow
// a freshly discovered model.
func mergeCatalogModels(primary, extra []modelInfo) []modelInfo {
	seen := make(map[string]struct{}, len(primary))
	merged := make([]modelInfo, 0, len(primary)+len(extra))
	for _, group := range [][]modelInfo{primary, extra} {
		for _, model := range group {
			if _, ok := seen[model.ID]; ok {
				continue
			}
			seen[model.ID] = struct{}{}
			merged = append(merged, model)
		}
	}
	return merged
}

func mergeCatalogMapping(primary, extra map[string]string) map[string]string {
	merged := make(map[string]string, len(primary)+len(extra))
	for key, value := range extra {
		merged[key] = value
	}
	for key, value := range primary {
		merged[key] = value
	}
	return merged
}

func invalidateModels() {
	modelCatalog.Lock()
	modelCatalog.entries = map[string]*catalogEntry{}
	modelCatalog.Unlock()
}

func catalogConfigs(cfg pluginConfig) []pluginConfig {
	if cfg.AccountHome != "" {
		return []pluginConfig{cfg}
	}
	homes, _ := enabledAgyHomes(currentAuthDir())
	if len(homes) == 0 {
		return []pluginConfig{cfg}
	}
	configs := make([]pluginConfig, 0, len(homes))
	for _, home := range homes {
		accountCfg := cfg
		accountCfg.AccountHome = home
		configs = append(configs, accountCfg)
	}
	return configs
}

func cachedEnabledModels(cfg pluginConfig) []modelInfo {
	configs := catalogConfigs(cfg)
	var merged []modelInfo
	seen := map[string]bool{}
	modelCatalog.Lock()
	for _, accountCfg := range configs {
		if entry := modelCatalog.entries[catalogKey(accountCfg)]; entry != nil {
			for _, m := range entry.models {
				if !seen[m.ID] {
					seen[m.ID] = true
					merged = append(merged, m)
				}
			}
		}
	}
	modelCatalog.Unlock()
	if len(merged) == 0 {
		merged, _ = builtinCatalog()
	}
	return cloneModels(merged)
}

func reconcileAccounts() {
	homes, ok := enabledAgyHomes(currentAuthDir())
	if !ok {
		return
	}
	enabled := make(map[string]bool, len(homes))
	for _, home := range homes {
		enabled[home] = true
	}
	modelCatalog.Lock()
	for key, entry := range modelCatalog.entries {
		if !enabled[entry.home] {
			delete(modelCatalog.entries, key)
		}
	}
	modelCatalog.Unlock()
	retireACPExcept(enabled)
}

func standardBuiltinModels() []modelInfo {
	return []modelInfo{
		{
			ID:                         "agy/default",
			Object:                     "model",
			OwnedBy:                    "google-antigravity-cli",
			DisplayName:                "Antigravity CLI (default model)",
			Name:                       "agy/default",
			Description:                "Uses the default model selected by the installed official agy CLI.",
			SupportedGenerationMethods: []string{"chat"},
			UserDefined:                true,
		},
		{
			ID:                         "gemini-3.8-flash",
			Object:                     "model",
			OwnedBy:                    "google-antigravity-cli",
			DisplayName:                "Gemini 3.8 Flash",
			Name:                       "gemini-3.8-flash",
			Description:                "Standard Gemini 3.8 Flash model with configurable reasoning effort.",
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
	}
}

func discoverModels(cfg pluginConfig) []modelInfo {
	models, _, _ := loadCatalog(cfg)
	return cloneModels(models)
}

func builtinCatalog() ([]modelInfo, map[string]string) {
	models := standardBuiltinModels()
	nativeByID := map[string]string{
		"agy/default":                     "",
		"antigravity/default":             "",
		"default":                         "",
		"Antigravity CLI (default model)": "",
		"gemini-3.8-flash":                "gemini-3.8-flash",
		"Gemini 3.8 Flash":                "gemini-3.8-flash",
		"gemini-3.8-flash-high":           "gemini-3.8-flash-high",
		"Gemini 3.8 Flash (High)":         "gemini-3.8-flash-high",
		"gemini-3.8-flash-medium":         "gemini-3.8-flash-medium",
		"Gemini 3.8 Flash (Medium)":       "gemini-3.8-flash-medium",
		"gemini-3.8-flash-low":            "gemini-3.8-flash-low",
		"Gemini 3.8 Flash (Low)":          "gemini-3.8-flash-low",
	}
	return models, nativeByID
}

// loadCatalog returns the catalog of cfg's account home; ok is false when discovery failed and the
// builtin list stands in. Concurrent first loads of one home wait for a single discovery.
func loadCatalog(cfg pluginConfig) (models []modelInfo, nativeByID map[string]string, ok bool) {
	key := catalogKey(cfg)
	for {
		modelCatalog.Lock()
		entry := modelCatalog.entries[key]
		if entry == nil {
			entry = &catalogEntry{home: cfg.AccountHome}
			modelCatalog.entries[key] = entry
		}
		cached := len(entry.models) > 0
		switch {
		case cached && (time.Since(entry.fetchedAt) < catalogTTL || entry.loading != nil || time.Since(entry.failedAt) < catalogRetryAfter):
			models, nativeByID = entry.models, entry.nativeByID
			modelCatalog.Unlock()
			return models, nativeByID, true
		case entry.loading != nil:
			wait := entry.loading
			modelCatalog.Unlock()
			<-wait
			continue
		case time.Since(entry.failedAt) < catalogRetryAfter:
			if cachedModels, cachedMapping := readModelCatalogCache(cfg); len(cachedModels) > 0 {
				cachedModels = normalizeCachedModelNames(cachedModels)
				if modelCatalog.entries[key] == entry {
					entry.models, entry.nativeByID = cachedModels, cachedMapping
				}
				modelCatalog.Unlock()
				return cachedModels, cachedMapping, true
			}
			modelCatalog.Unlock()
			models, nativeByID = builtinCatalog()
			return models, nativeByID, false
		}
		loading := make(chan struct{})
		entry.loading = loading
		modelCatalog.Unlock()

		models, nativeByID, ok = discoverCatalog(cfg)

		modelCatalog.Lock()
		entry.loading = nil
		if ok {
			if modelCatalog.entries[key] == entry {
				entry.fetchedAt, entry.failedAt = time.Now(), time.Time{}
				entry.models, entry.nativeByID = models, nativeByID
			}
		} else {
			entry.failedAt = time.Now()
			if cached {
				models, nativeByID, ok = entry.models, entry.nativeByID, true
			} else if cachedModels, cachedMapping := readModelCatalogCache(cfg); len(cachedModels) > 0 {
				models = mergeCatalogModels(models, normalizeCachedModelNames(cachedModels))
				nativeByID = mergeCatalogMapping(nativeByID, cachedMapping)
				if modelCatalog.entries[key] == entry {
					entry.models, entry.nativeByID = models, nativeByID
				}
				ok = true
			}
		}
		modelCatalog.Unlock()
		close(loading)
		return models, nativeByID, ok
	}
}

func discoverCatalog(cfg pluginConfig) ([]modelInfo, map[string]string, bool) {
	models, nativeByID := builtinCatalog()
	discoveryComplete := false

	if cfg.Mode == modeACP {
		if discovered, mapping, err := discoverModelsACP(cfg); err == nil {
			models = append(models, discovered...)
			mergeDiscoveredNativeMap(nativeByID, mapping)
			discoveryComplete = true
		}
	}

	if err := validateBinaryPath(cfg.BinaryPath); err == nil && !discoveryComplete {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		binPath := resolveBinaryPath(cfg.BinaryPath)
		cmd := exec.CommandContext(ctx, binPath, "models")
		configureSysProcAttr(cmd)
		applyAccountEnv(cmd, cfg)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if cfg.Workdir != "" {
			cmd.Dir = cfg.Workdir
		}

		if err := cmd.Start(); err == nil {
			activeProcesses.add(cmd)
			waitErr := cmd.Wait()
			activeProcesses.remove(cmd)
			discovered, mapping := parseAgyModelLines(stdout.String())
			// Only a clean exit means the listing is complete.
			if waitErr == nil && len(discovered) > 0 {
				models = append(models, discovered...)
				mergeDiscoveredNativeMap(nativeByID, mapping)
				discoveryComplete = true
			}
		}
	}

	// CLIProxyAPI lists Gemini-format models as models/<Name>; keep the routable ID there and
	// the label in DisplayName, so clients that fetch the Gemini list get usable model IDs.
	for i := range models {
		models[i].Name = models[i].ID
	}
	if discoveryComplete {
		writeModelCatalogCache(cfg, models, nativeByID)
	}
	return models, nativeByID, discoveryComplete
}

// mergeDiscoveredNativeMap records direct and base-slug mappings deterministically.
func mergeDiscoveredNativeMap(nativeByID, mapping map[string]string) {
	// Pass 1: record direct mappings. Pass 2: normalize derived base slugs so
	// map iteration order never lets a display label overwrite a clean base slug.
	for id, native := range mapping {
		nativeByID[id] = native
		slug := strings.TrimPrefix(id, "agy/")
		nativeByID[slug] = native
		nativeByID["antigravity/"+slug] = native
	}
	for id := range mapping {
		slug := strings.TrimPrefix(id, "agy/")
		baseSlug := stripEffortSuffix(slug)
		if baseSlug != slug {
			nativeByID[baseSlug] = baseSlug
			nativeByID["agy/"+baseSlug] = baseSlug
			nativeByID["antigravity/"+baseSlug] = baseSlug
		}
	}
}

func containsFold(values []string, target string) bool {
	for _, v := range values {
		if strings.EqualFold(strings.TrimSpace(v), target) {
			return true
		}
	}
	return false
}

func normalizeCachedModelNames(models []modelInfo) []modelInfo {
	out := cloneModels(models)
	for i := range out {
		if out[i].ID != "" {
			out[i].Name = out[i].ID
		}
	}
	return out
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
			} else if len(fields) == 1 && looksLikeModelSlug(fields[0]) {
				slug = fields[0]
				label = humanizeModelName(slug)
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

		name := label
		if name == "" || name == slug || looksLikeModelSlug(name) {
			name = humanizeModelName(slug)
		}

		id := "agy/" + slug
		if _, exists := byID[id]; !exists {
			byID[id] = modelInfo{
				ID:                         id,
				Object:                     "model",
				OwnedBy:                    "google-antigravity-cli",
				DisplayName:                name,
				Name:                       id,
				Description:                "Discovered from the installed official agy CLI.",
				SupportedGenerationMethods: []string{"chat"},
				UserDefined:                true,
			}
			nativeByID[id] = native
			nativeByID[name] = native
		}

		baseSlug := stripEffortSuffix(slug)
		if baseSlug != slug {
			baseID := "agy/" + baseSlug
			baseName := stripEffortLabel(name)
			if baseName == "" || baseName == baseSlug || looksLikeModelSlug(baseName) {
				baseName = humanizeModelName(baseSlug)
			}
			if _, exists := byID[baseID]; !exists {
				byID[baseID] = modelInfo{
					ID:                         baseID,
					Object:                     "model",
					OwnedBy:                    "google-antigravity-cli",
					DisplayName:                baseName,
					Name:                       baseID,
					Description:                "Discovered from the installed official agy CLI with configurable reasoning effort.",
					SupportedGenerationMethods: []string{"chat"},
					UserDefined:                true,
				}
			}
			nativeByID[baseID] = baseSlug
			nativeByID[baseName] = baseSlug
		}
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
	return resolveNativeModelWithDiscovery(model, cfg, true)
}

func resolveNativeModelWithDiscovery(model string, cfg pluginConfig, discover bool) string {
	cleanModel := strings.TrimSpace(model)
	cleanModel = strings.TrimPrefix(cleanModel, "antigravity/")
	cleanModel = strings.TrimPrefix(cleanModel, "agy/")

	if cleanModel == "" || cleanModel == "default" {
		return ""
	}

	var nativeByID map[string]string
	if discover {
		_, nativeByID, _ = loadCatalog(cfg)
	} else {
		nativeByID = cachedNativeMapping(cfg)
	}

	if native, ok := nativeByID[cleanModel]; ok && native != "" {
		return native
	}
	if native, ok := nativeByID["agy/"+cleanModel]; ok && native != "" {
		return native
	}
	return cleanModel
}

func cachedNativeMapping(cfg pluginConfig) map[string]string {
	key := catalogKey(cfg)
	modelCatalog.Lock()
	entry := modelCatalog.entries[key]
	var mapping map[string]string
	if entry != nil && len(entry.nativeByID) > 0 {
		mapping = entry.nativeByID
	}
	modelCatalog.Unlock()
	if len(mapping) > 0 {
		return mapping
	}
	if _, diskMapping := readModelCatalogCache(cfg); len(diskMapping) > 0 {
		return diskMapping
	}
	_, builtinMapping := builtinCatalog()
	return builtinMapping
}

type effortSuffixSpec struct {
	suffix    string
	canonical string
	label     string
}

// Ordered longest-suffix-first so multi-part suffixes like -extra-high and -x-high
// match before -high.
var effortDashSuffixes = []effortSuffixSpec{
	{suffix: "-extra-high", canonical: "xhigh", label: " (XHigh)"},
	{suffix: "-medium", canonical: "medium", label: " (Medium)"},
	{suffix: "-x-high", canonical: "xhigh", label: " (XHigh)"},
	{suffix: "-xhigh", canonical: "xhigh", label: " (XHigh)"},
	{suffix: "-high", canonical: "high", label: " (High)"},
	{suffix: "-low", canonical: "low", label: " (Low)"},
	{suffix: "-med", canonical: "medium", label: " (Medium)"},
	{suffix: "-max", canonical: "max", label: " (Max)"},
}

var effortParenLabels = []string{
	"(extra-high)", "(extra high)", "(x-high)", "(xhigh)",
	"(high)", "(medium)", "(med)", "(low)", "(max)", "(thinking)",
}

func stripEffortLabel(label string) string {
	cleaned := strings.TrimSpace(label)
	lower := strings.ToLower(cleaned)
	for _, suffix := range effortParenLabels {
		if strings.HasSuffix(lower, suffix) {
			return strings.TrimSpace(cleaned[:len(cleaned)-len(suffix)])
		}
	}
	return cleaned
}

func humanizeModelName(slug string) string {
	raw := strings.TrimSpace(slug)
	if raw == "" {
		return ""
	}
	if raw == "default" {
		return "Antigravity CLI (default model)"
	}

	effortSuffix := ""
	clean := raw
	if base, _, label := splitEffortSuffix(raw); label != "" {
		clean = base
		effortSuffix = label
	} else {
		lower := strings.ToLower(raw)
		for _, suffix := range []string{"-thinking", "-thought"} {
			if strings.HasSuffix(lower, suffix) {
				effortSuffix = " (Thinking)"
				clean = raw[:len(raw)-len(suffix)]
				break
			}
		}
	}

	parts := strings.Split(clean, "-")
	var formatted []string
	for i, part := range parts {
		pLower := strings.ToLower(part)
		switch pLower {
		case "gpt":
			formatted = append(formatted, "GPT")
		case "oss":
			formatted = append(formatted, "OSS")
		case "gemini":
			formatted = append(formatted, "Gemini")
		case "claude":
			formatted = append(formatted, "Claude")
		case "sonnet":
			formatted = append(formatted, "Sonnet")
		case "opus":
			formatted = append(formatted, "Opus")
		case "haiku":
			formatted = append(formatted, "Haiku")
		case "flash":
			formatted = append(formatted, "Flash")
		case "pro":
			formatted = append(formatted, "Pro")
		case "ultra":
			formatted = append(formatted, "Ultra")
		default:
			if len(pLower) > 1 && pLower[len(pLower)-1] == 'b' && isDigits(pLower[:len(pLower)-1]) {
				formatted = append(formatted, strings.ToUpper(part))
			} else if isDigits(part) && i > 0 && isDigits(parts[i-1]) && len(formatted) > 0 {
				formatted[len(formatted)-1] = formatted[len(formatted)-1] + "." + part
			} else {
				r := []rune(part)
				if len(r) > 0 {
					r[0] = unicode.ToUpper(r[0])
					formatted = append(formatted, string(r))
				}
			}
		}
	}

	res := strings.Join(formatted, " ")
	res = strings.ReplaceAll(res, "GPT OSS", "GPT-OSS")
	return res + effortSuffix
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func splitEffortSuffix(model string) (base, effort, label string) {
	trimmed := strings.TrimSpace(model)
	lower := strings.ToLower(trimmed)
	for _, spec := range effortDashSuffixes {
		if strings.HasSuffix(lower, spec.suffix) && len(trimmed) > len(spec.suffix) {
			return strings.TrimSpace(trimmed[:len(trimmed)-len(spec.suffix)]), spec.canonical, spec.label
		}
	}
	return trimmed, "", ""
}

func splitColonEffortSuffix(model string) (base, effort string) {
	trimmed := strings.TrimSpace(model)
	colon := strings.LastIndexByte(trimmed, ':')
	if colon <= 0 {
		return trimmed, ""
	}
	candidate := trimmed[colon+1:]
	if strings.ContainsAny(candidate, " \t") {
		return trimmed, ""
	}
	if eff := normalizeEffort(candidate); eff != "" {
		return strings.TrimSpace(trimmed[:colon]), eff
	}
	return trimmed, ""
}

func extractEffortSuffix(model string) string {
	_, effort, _ := splitEffortSuffix(model)
	return effort
}

func stripEffortSuffix(model string) string {
	base, _, _ := splitEffortSuffix(model)
	return base
}

func supportsEffort(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	lower = strings.TrimPrefix(lower, "antigravity/")
	lower = strings.TrimPrefix(lower, "agy/")
	if lower == "" || lower == "default" {
		return true
	}
	if strings.HasPrefix(lower, "claude-") {
		return false
	}
	return true
}

func defaultEffortForModel(model string) string {
	lower := strings.ToLower(strings.TrimSpace(model))
	lower = strings.TrimPrefix(lower, "antigravity/")
	lower = strings.TrimPrefix(lower, "agy/")
	if strings.HasPrefix(lower, "gpt-oss-") {
		return "medium"
	}
	if strings.HasPrefix(lower, "gemini-") {
		return "high"
	}
	return ""
}

func resolveExecutionOptions(req rpcExecutorRequest, cfg pluginConfig) agyOptions {
	return resolveExecutionOptionsWithDiscovery(req, cfg, true)
}

func resolveExecutionOptionsWithDiscovery(req rpcExecutorRequest, cfg pluginConfig, discover bool) agyOptions {
	opts, requestedModel := parseExecutionOptions(req, cfg)
	opts.NativeModel = resolveNativeModelWithDiscovery(requestedModel, cfg, discover)
	return opts
}

// resolveExecutionOptionsForRequest decides between ACP and print mode before model discovery
// so requests with a conversation_id never start or wait on a resident ACP process.
func resolveExecutionOptionsForRequest(req rpcExecutorRequest, cfg pluginConfig) (agyOptions, bool) {
	opts, requestedModel := parseExecutionOptions(req, cfg)
	acp := useACP(cfg, opts)
	modelCfg := cfg
	if !acp {
		modelCfg.Mode = modePrint
	}
	opts.NativeModel = resolveNativeModelWithDiscovery(requestedModel, modelCfg, true)
	return opts, acp
}

func parseExecutionOptions(req rpcExecutorRequest, cfg pluginConfig) (agyOptions, string) {
	var chatReq chatCompletionRequest
	_ = json.Unmarshal(req.Payload, &chatReq)

	requestedModel := strings.TrimSpace(req.Model)
	if requestedModel == "" {
		requestedModel = strings.TrimSpace(chatReq.Model)
	}

	effort := ""
	// 1. CLIProxyAPI thinking suffix (e.g. model(high)). The host normally moves it into
	// reasoning_effort itself, but passes it through for models it has no metadata for.
	if open := strings.LastIndex(requestedModel, "("); open > 0 && strings.HasSuffix(requestedModel, ")") {
		effort = normalizeEffort(requestedModel[open+1 : len(requestedModel)-1])
		requestedModel = strings.TrimSpace(requestedModel[:open])
	}

	// 2. Check explicit model colon suffix override :low, :medium, :med, :high, :xhigh, :max (e.g. model:xhigh)
	if base, colonEffort := splitColonEffortSuffix(requestedModel); colonEffort != "" {
		if effort == "" {
			effort = colonEffort
		}
		requestedModel = base
	}

	// 3. Split model name: if model has a reasoning effort suffix (e.g. -high, -medium, -low),
	// split the last part into reasoning effort and strip it to the base model name.
	modelEffort := extractEffortSuffix(requestedModel)
	if modelEffort != "" {
		if effort == "" {
			effort = modelEffort
		}
		requestedModel = stripEffortSuffix(requestedModel)
	}

	// 4. Fallback: Check chatReq.ReasoningEffort from request payload
	if effort == "" && chatReq.ReasoningEffort != "" {
		effort = normalizeEffort(chatReq.ReasoningEffort)
	}

	// 5. Fallback: Check request headers (X-AGY-Effort, X-Reasoning-Effort)
	if effort == "" && req.Headers != nil {
		if val := req.Headers.Get("X-AGY-Effort"); val != "" {
			effort = normalizeEffort(val)
		} else if val := req.Headers.Get("X-Reasoning-Effort"); val != "" {
			effort = normalizeEffort(val)
		}
	}

	// 6. Fallback: Check request metadata (reasoning_effort, effort)
	if effort == "" && req.Metadata != nil {
		if val, ok := req.Metadata["reasoning_effort"].(string); ok && val != "" {
			effort = normalizeEffort(val)
		} else if val, ok := req.Metadata["effort"].(string); ok && val != "" {
			effort = normalizeEffort(val)
		}
	}

	// 7. Fallback: Check config default if specified
	if effort == "" && cfg.DefaultReasoningEffort != "" {
		effort = cfg.DefaultReasoningEffort
	}

	// 8. Fallback: If the model requires an effort flag but none was provided, use model default fallback
	if effort == "" {
		effort = defaultEffortForModel(requestedModel)
	}

	// If the model does not support reasoning effort (e.g. claude-*), clear effort
	if !supportsEffort(requestedModel) {
		effort = ""
	}

	return agyOptions{
		ConversationID:  requestConversationID(chatReq.conversationFields, req.Headers, req.Metadata),
		ReasoningEffort: effort,
		HasTools:        len(chatReq.Tools) > 0,
	}, requestedModel
}

func normalizeEffort(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "low":
		return "low"
	case "medium", "med":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "x-high", "x_high", "extra-high", "extra_high", "extra high":
		return "xhigh"
	case "max":
		return "max"
	default:
		return ""
	}
}

func agyArgs(cfg pluginConfig, opts agyOptions) []string {
	outputFormat := opts.OutputFormat
	if outputFormat == "" {
		outputFormat = "json"
	}
	// Client prompts are untrusted text: never let a leading "/" expand into an agy
	// slash command or skill (e.g. "/usage", plugin commands).
	args := []string{"--output-format", outputFormat, "--print-timeout", cfg.PrintTimeout, "--disable-slash-commands"}
	if cfg.DangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	if cfg.Sandbox {
		args = append(args, "--sandbox")
	}
	model := strings.TrimSpace(opts.NativeModel)
	effort := strings.TrimSpace(opts.ReasoningEffort)

	// Guard against conflicting model and effort flags (e.g. --model gemini-3.8-flash-high --effort low).
	// If the model name already specifies an effort suffix and effort differs, strip the suffix
	// so the base model is used with the desired effort flag.
	modelEffort := extractEffortSuffix(model)
	if modelEffort != "" && effort != "" && modelEffort != effort {
		model = stripEffortSuffix(model)
	}

	if model != "" {
		args = append(args, "--model", model)
	}
	if strings.TrimSpace(opts.ConversationID) != "" {
		args = append(args, "--conversation", strings.TrimSpace(opts.ConversationID))
	}
	if effort != "" && supportsEffort(model) {
		args = append(args, "--effort", effort)
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
	binPath := resolveBinaryPath(cfg.BinaryPath)
	cmd := exec.CommandContext(ctx, binPath, agyArgs(cfg, opts)...)
	if cfg.Workdir != "" {
		cmd.Dir = cfg.Workdir
	}
	applyAccountEnv(cmd, cfg)
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
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return agyResult{}, stderr.String(), classifyAgyError(ctx.Err(), stderr.String())
	}

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

// agyStructuredError is the machine-readable "AGY_ERROR: {...}" line agy writes to stderr.
type agyStructuredError struct {
	ShortError string `json:"short_error"`
	Status     string `json:"status"`
	ErrorCode  int    `json:"error_code"`
	Retryable  bool   `json:"retryable"`
	Reason     string `json:"reason"`
}

const agyErrorPrefix = "AGY_ERROR:"

// splitAgyStderr separates the last structured AGY_ERROR line from the human-readable stderr text.
func splitAgyStderr(stderr string) (agyStructuredError, bool, string) {
	var structured agyStructuredError
	found := false
	var text []string
	for _, line := range strings.Split(stderr, "\n") {
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, agyErrorPrefix); ok {
			var parsed agyStructuredError
			if json.Unmarshal([]byte(strings.TrimSpace(rest)), &parsed) == nil {
				structured = parsed
				found = true
			}
			continue
		}
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "error:"))
		if trimmed != "" {
			text = append(text, trimmed)
		}
	}
	return structured, found, strings.Join(text, "\n")
}

func classifyAgyError(err error, stderr string) error {
	structured, hasStructured, stderrText := splitAgyStderr(stderr)

	// "exit status N" says nothing useful when agy explained itself on stderr.
	var exitErr *exec.ExitError
	errText := errorString(err)
	if errors.As(err, &exitErr) && (stderrText != "" || hasStructured) {
		errText = ""
	}
	if errText != "" && strings.Contains(stderrText, strings.TrimSpace(errText)) {
		errText = ""
	}
	msg := strings.TrimSpace(strings.Join([]string{errText, stderrText}, "\n"))
	if msg == "" && hasStructured {
		msg = structured.ShortError
	}
	lower := strings.ToLower(msg)

	if errors.Is(err, exec.ErrNotFound) || strings.Contains(lower, "executable file not found") || strings.Contains(lower, "no such file or directory") && strings.Contains(lower, "agy") {
		return pError("agy_not_found", "official Antigravity CLI executable was not found; install agy or set binary_path", http.StatusServiceUnavailable)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return pRetryableError("agy_timeout", "Antigravity CLI did not finish before the command timeout", http.StatusGatewayTimeout)
	}

	if hasStructured {
		switch {
		case structured.Status == "RESOURCE_EXHAUSTED" || structured.ErrorCode == http.StatusTooManyRequests:
			markLimitsStale()
			return &pluginError{Code: "agy_rate_limit", Message: nonEmpty(msg, "Antigravity quota or rate limit reached"), HTTPStatus: http.StatusTooManyRequests, Retryable: structured.Retryable}
		case structured.Status == "UNAUTHENTICATED" || structured.ErrorCode == http.StatusUnauthorized:
			return pError("agy_authentication_error", nonEmpty(msg, "Antigravity CLI is not authenticated"), http.StatusUnauthorized)
		case structured.Status == "PERMISSION_DENIED" || structured.ErrorCode == http.StatusForbidden:
			return pError("agy_permission_denied", nonEmpty(msg, "Antigravity denied the request"), http.StatusForbidden)
		case structured.Status == "DEADLINE_EXCEEDED" || structured.Reason == "print_timeout":
			return &pluginError{Code: "agy_timeout", Message: nonEmpty(msg, "Antigravity CLI print timeout reached"), HTTPStatus: http.StatusGatewayTimeout, Retryable: structured.Retryable}
		case structured.ErrorCode >= 500 && structured.ErrorCode <= 599:
			return &pluginError{Code: "agy_upstream_error", Message: nonEmpty(msg, "Antigravity upstream error"), HTTPStatus: http.StatusBadGateway, Retryable: structured.Retryable}
		}
	}

	switch {
	case strings.Contains(lower, "quota"), strings.Contains(lower, "rate limit"), strings.Contains(lower, "too many requests"), strings.Contains(lower, "resource_exhausted"):
		markLimitsStale()
		return pRetryableError("agy_rate_limit", nonEmpty(msg, "Antigravity quota or rate limit reached"), http.StatusTooManyRequests)
	case strings.Contains(lower, "invalid --effort"), (strings.Contains(lower, "has no ") && strings.Contains(lower, " effort")), strings.Contains(lower, "--effort is not supported"):
		return pError(errorCodeRequestScoped, nonEmpty(msg, "Antigravity model does not support the requested reasoning effort"), http.StatusBadRequest)
	case strings.Contains(lower, "invalid model selection"), strings.Contains(lower, "model") && (strings.Contains(lower, "not found") || strings.Contains(lower, "unknown") || strings.Contains(lower, "not recognized")):
		return pError(errorCodeModelNotFound, nonEmpty(msg, "Antigravity model not found"), http.StatusNotFound)
	case strings.Contains(lower, "sign in"), strings.Contains(lower, "login"), strings.Contains(lower, "authenticate"), strings.Contains(lower, "credential"):
		return pError("agy_authentication_error", nonEmpty(msg, "Antigravity CLI is not authenticated"), http.StatusUnauthorized)
	default:
		return &pluginError{Code: "agy_execution_error", Message: nonEmpty(msg, "Antigravity CLI execution failed"), HTTPStatus: http.StatusBadGateway, Retryable: hasStructured && structured.Retryable}
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

type agyExecution struct {
	cfg     pluginConfig
	opts    agyOptions
	prompt  string
	session conversationRef
	// stagedBytes is non-zero when the request was staged to a temp file.
	stagedBytes int
	// cleanup removes the staged file; it runs after agy exits, on cancel, and on
	// the process-kill paths.
	cleanup func()
}

func prepareAgyExecution(req rpcExecutorRequest) (agyExecution, error) {
	return prepareAgyExecutionWithDiscovery(req, true)
}

func prepareAgyExecutionWithDiscovery(req rpcExecutorRequest, discover bool) (agyExecution, error) {
	cfg, err := withAccount(currentConfig(), req.AuthMetadata)
	if err != nil {
		return agyExecution{}, err
	}
	// Print execution and count_tokens never need an ACP resident for model resolution.
	cfg.Mode = modePrint
	opts := resolveExecutionOptionsWithDiscovery(req, cfg, discover)
	session := conversationRef{Home: sessionHome(cfg.AccountHome), Managed: req.AuthID != ""}
	nativeID, handoff, err := resolveConversation(opts.ConversationID, req.Headers, session)
	if err != nil {
		return agyExecution{}, err
	}
	opts.ConversationID = nativeID
	session.NativeID = nativeID
	chatReq, err := decodeChatRequest(req.Payload)
	if err != nil {
		return agyExecution{}, err
	}
	if handoff {
		if err := addHandoffContext(&chatReq); err != nil {
			return agyExecution{}, err
		}
	}
	prompt, err := buildChatPrompt(chatReq, nativeID, cfg.Workdir)
	if err != nil {
		return agyExecution{}, err
	}
	run := agyExecution{cfg: cfg, opts: opts, prompt: prompt, session: session}
	// Staging is opt-in: only a configured threshold changes the forwarded payload.
	if cfg.MaxPromptBytes > 0 && len(prompt) > cfg.MaxPromptBytes {
		staged, buildErr := buildChatPromptFor(chatReq, nativeID, cfg.Workdir, true)
		if buildErr != nil {
			return agyExecution{}, buildErr
		}
		path, writeErr := writePromptFile(staged)
		if writeErr != nil {
			return agyExecution{}, pError("prompt_staging_failed", "failed to stage the oversized prompt: "+writeErr.Error(), http.StatusInternalServerError)
		}
		run.prompt = fileHandoffPrompt(path, len(staged))
		run.stagedBytes = len(staged)
		run.cleanup = func() { removeStagedPrompt(path) }
	}
	return run, nil
}

func executeNonStream(req rpcExecutorRequest) (executorResponse, error) {
	cfg, err := withAccount(currentConfig(), req.AuthMetadata)
	if err != nil {
		return executorResponse{}, err
	}
	opts, acp := resolveExecutionOptionsForRequest(req, cfg)
	if acp {
		if err := beginACPWork(); err != nil {
			return executorResponse{}, err
		}
		defer endACPWork()
		if err := checkModelLimits(cfg, opts.NativeModel); err != nil {
			return executorResponse{}, err
		}
		result, err := executeNonStreamACP(req, cfg, opts)
		if err != nil {
			return executorResponse{}, err
		}
		// Never expose a raw ACP session ID as conversation_id, or the next turn drops into print mode.
		result.ConversationID = ""
		payload, err := makeChatCompletion(req.Model, declaredToolNames(req.Payload), result)
		if err != nil {
			return executorResponse{}, err
		}
		headers := http.Header{"Content-Type": []string{"application/json"}}
		mergeHeaders(headers, limitHeaders(cfg, opts.NativeModel))
		return executorResponse{
			Payload: payload,
			Headers: headers,
			Metadata: map[string]any{
				"agy_status": result.Status,
				"agy_mode":   modeACP,
			},
		}, nil
	}

	run, err := prepareAgyExecution(req)
	if err != nil {
		return executorResponse{}, err
	}
	cfg, opts = run.cfg, run.opts
	if err := checkModelLimits(cfg, opts.NativeModel); err != nil {
		if run.cleanup != nil {
			run.cleanup()
		}
		return executorResponse{}, err
	}
	result, _, err := runAgyJSON(cfg, opts, run.prompt)
	if run.cleanup != nil {
		run.cleanup()
	}
	if err != nil {
		return executorResponse{}, err
	}
	run.session.NativeID = nonEmpty(result.ConversationID, run.session.NativeID)
	result.ConversationID = run.session.clientID()

	payload, err := makeChatCompletion(req.Model, declaredToolNames(req.Payload), result)
	if err != nil {
		return executorResponse{}, err
	}
	headers := http.Header{"Content-Type": []string{"application/json"}}
	if result.ConversationID != "" {
		headers.Set(conversationHeader, result.ConversationID)
	}
	mergeHeaders(headers, limitHeaders(cfg, opts.NativeModel))
	metadata := map[string]any{
		"agy_conversation_id": result.ConversationID,
		"agy_status":          result.Status,
	}
	// Observable, not silent: clients can see that the turn was staged.
	if run.stagedBytes > 0 {
		metadata["agy_prompt_staged_bytes"] = run.stagedBytes
	}
	return executorResponse{
		Payload:  payload,
		Headers:  headers,
		Metadata: metadata,
	}, nil
}

func executeStream(req rpcExecutorRequest, run agyExecution) {
	if run.cleanup != nil {
		defer run.cleanup()
	}
	streamID := req.StreamID
	cfg, opts := run.cfg, run.opts

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout(cfg))
	defer cancel()
	cmd, err := newAgyCommand(ctx, cfg, opts, run.prompt)
	if err != nil {
		closePluginStream(streamID, err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		closePluginStream(streamID, err)
		return
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		closePluginStream(streamID, classifyAgyError(err, stderr.String()))
		return
	}
	activeProcesses.add(cmd)
	defer activeProcesses.remove(cmd)

	model := displayModel(req.Model)
	_ = model
	session := run.session
	out := newChatStream(req, session.clientID())
	updateConversationID := func(nativeID string) {
		if nativeID = strings.TrimSpace(nativeID); nativeID != "" && nativeID != session.NativeID {
			session.NativeID = nativeID
			out.conversationID = session.clientID()
		}
	}

	emit := out.emit
	finishStream := out.finish

	scanner := bufio.NewScanner(stdout)
	// One NDJSON event carries a whole assistant turn, and a turn holding several
	// large tool calls easily exceeds the 64KB default. 32MB keeps those events
	// intact; bufio.Scanner drops lines beyond the limit instead of erroring.
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	var terminal *agyResult
	var streamError string
	var streamedText strings.Builder
	emittedLen := 0
	isToolCallStream := false
	// With client tools, hold text back until it is clear the reply is not a tool-call block.
	toolDecisionPending := opts.HasTools

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
			updateConversationID(event.ConversationID)
		case "step_update":
			if event.StepUpdate.StepType == "agent_response" && event.StepUpdate.TextDelta != "" {
				streamedText.WriteString(event.StepUpdate.TextDelta)

				if toolDecisionPending {
					switch toolCallPrefixState(streamedText.String()) {
					case toolPrefixMatch:
						isToolCallStream = true
						toolDecisionPending = false
					case toolPrefixNone:
						toolDecisionPending = false
					}
				}
				if isToolCallStream || toolDecisionPending {
					continue
				}

				pending := streamedText.String()[emittedLen:]
				emittedLen = streamedText.Len()
				if err := emit(map[string]any{"content": pending}, nil, nil); err != nil {
					closePluginStream(streamID, err)
					killProcessTree(cmd)
					_ = cmd.Wait()
					return
				}
			}
		case "result":
			if event.Result != nil {
				copyResult := *event.Result
				terminal = &copyResult
				updateConversationID(copyResult.ConversationID)
			}
		case "error":
			streamError = strings.TrimSpace(event.Message)
			if streamError == "" {
				streamError = "Antigravity stream failed"
			}
		default:
			if event.Status != "" {
				terminal = &agyResult{Status: event.Status, Response: event.Response, Error: event.Error, Usage: event.Usage, ConversationID: event.ConversationID}
				updateConversationID(event.ConversationID)
			}
		}
	}

	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		closePluginStream(streamID, classifyAgyError(ctx.Err(), stderr.String()))
		return
	}
	if scanErr != nil {
		closePluginStream(streamID, pRetryableError("agy_stream_error", "reading agy stream: "+scanErr.Error(), http.StatusBadGateway))
		return
	}
	if streamError != "" {
		closePluginStream(streamID, classifyAgyError(errors.New(streamError), stderr.String()))
		return
	}
	if terminal == nil {
		if waitErr != nil {
			closePluginStream(streamID, classifyAgyError(waitErr, stderr.String()))
		} else {
			closePluginStream(streamID, pError("invalid_upstream_response", "agy stream ended without a result event", http.StatusBadGateway))
		}
		return
	}
	if !strings.EqualFold(terminal.Status, "SUCCESS") {
		msg := nonEmpty(terminal.Error, "Antigravity CLI ended with status "+terminal.Status)
		closePluginStream(streamID, classifyAgyError(errors.New(msg), stderr.String()))
		return
	}
	if waitErr != nil {
		closePluginStream(streamID, classifyAgyError(waitErr, stderr.String()))
		return
	}

	fullResponse := terminal.Response
	if fullResponse == "" {
		fullResponse = streamedText.String()
	}

	toolCalls, isToolCall := extractToolCalls(fullResponse, opts.HasTools)
	toolCalls = normalizeToolCalls(toolCalls, declaredToolNames(req.Payload))
	if !isToolCall && strings.TrimSpace(fullResponse) == "" {
		// agy finished without text: it burned the turn on its own built-in tools.
		// Failing here lets the client walk its fallback chain instead of retrying
		// an identical empty completion.
		closePluginStream(streamID, emptyResponseError())
		return
	}
	if opts.HasTools && isToolCall {
		// Emit tool_calls delta chunk
		if err := emit(map[string]any{
			"tool_calls": toolCalls,
		}, nil, nil); err != nil {
			closePluginStream(streamID, err)
			return
		}

		// The finish chunk and the usage chunk must stay separate: a usage chunk has empty choices.
		finishReason := "tool_calls"
		if err := emit(map[string]any{}, &finishReason, nil); err != nil {
			closePluginStream(streamID, err)
			return
		}
		if streamWantsUsage(req) {
			if err := emit(map[string]any{}, nil, openAIUsage(terminal.Usage)); err != nil {
				closePluginStream(streamID, err)
				return
			}
		}
		finishStream()
		return
	}

	// If it was buffered thinking it was a tool call but turned out not to be
	if streamedText.Len() > emittedLen {
		if err := emit(map[string]any{"content": streamedText.String()[emittedLen:]}, nil, nil); err != nil {
			closePluginStream(streamID, err)
			return
		}
	} else if streamedText.Len() == 0 && terminal.Response != "" {
		if err := emit(map[string]any{"content": terminal.Response}, nil, nil); err != nil {
			closePluginStream(streamID, err)
			return
		}
	}

	finish := "stop"
	if err := emit(map[string]any{}, &finish, nil); err != nil {
		closePluginStream(streamID, err)
		return
	}
	if streamWantsUsage(req) {
		if err := emit(map[string]any{}, nil, openAIUsage(terminal.Usage)); err != nil {
			closePluginStream(streamID, err)
			return
		}
	}
	finishStream()
}

type toolPrefixState int

const (
	toolPrefixPending toolPrefixState = iota
	toolPrefixMatch
	toolPrefixNone
)

var toolCallMarkers = []string{"```tool_calls", "```json", "[", "{"}

// toolCallPrefixState reports whether streamed text starts a tool-call block, cannot start one,
// or is still too short to tell (e.g. a lone "`").
func toolCallPrefixState(text string) toolPrefixState {
	trimmed := strings.TrimLeftFunc(text, unicode.IsSpace)
	if trimmed == "" {
		return toolPrefixPending
	}
	pending := false
	for _, marker := range toolCallMarkers {
		if strings.HasPrefix(trimmed, marker) {
			return toolPrefixMatch
		}
		if strings.HasPrefix(marker, trimmed) {
			pending = true
		}
	}
	if pending {
		return toolPrefixPending
	}
	return toolPrefixNone
}

// finishPluginStream emits the SSE [DONE] sentinel on translated routes and closes
// the stream, propagating any emission failure to closeStream.
func finishPluginStream(streamID string, rawChunks bool, emitChunk func(string, []byte) error, closeStream func(string, error)) {
	if !rawChunks {
		if err := emitChunk(streamID, []byte("data: [DONE]\n\n")); err != nil {
			closeStream(streamID, err)
			return
		}
	}
	closeStream(streamID, nil)
}

func emitPluginStreamChunk(streamID string, payload []byte) error {
	if strings.TrimSpace(streamID) == "" {
		return fmt.Errorf("plugin stream id is required")
	}
	_, err := callHost(methodHostStreamEmit, streamEmitRequest{StreamID: streamID, Payload: payload})
	return err
}

// closePluginStream ends a plugin stream; a non-nil err is reported with its status, code and retry-after.
func closePluginStream(streamID string, err error) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	req := streamCloseRequest{StreamID: streamID}
	if err != nil {
		pe := asPluginError(err)
		req.Error = strings.TrimSpace(pe.Message)
		req.ErrorDetail = pe.envelopeError()
	}
	_, _ = callHost(methodHostStreamClose, req)
}
