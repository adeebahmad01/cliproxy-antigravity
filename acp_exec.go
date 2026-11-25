package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	acpImageMaxBytes     = 20 << 20
	acpImageFetchTimeout = 30 * time.Second
	acpDiscoverTimeout   = 20 * time.Second
	acpMaxRedirects      = 5
)

// useACP reports whether a request runs on the resident agy. Requests carrying a
// conversation_id stay on print mode so conversations.go handles native resumes and handoffs.
func useACP(cfg pluginConfig, opts agyOptions) bool {
	return cfg.Mode == modeACP && strings.TrimSpace(opts.ConversationID) == ""
}

// remoteFetchClientOverride lets tests inject an HTTP client without bypassing SSRF tests.
var remoteFetchClientOverride *http.Client

func isDisallowedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	// Carrier-grade NAT (100.64.0.0/10) and documentation/benchmark ranges.
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
		if (ip4[0] == 192 && ip4[1] == 0 && (ip4[2] == 0 || ip4[2] == 2)) ||
			(ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19 || (ip4[1] == 51 && ip4[2] == 100))) ||
			(ip4[0] == 203 && ip4[1] == 0 && ip4[2] == 113) {
			return true
		}
	}
	return false
}

func validateRemoteURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("invalid attachment URL: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported attachment URL scheme %q", u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("attachment URLs with embedded credentials are not allowed")
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" {
		return nil, fmt.Errorf("attachment URL has no host")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return nil, fmt.Errorf("attachment URL host %q is not allowed", host)
	}
	if ip := net.ParseIP(host); ip != nil && isDisallowedIP(ip) {
		return nil, fmt.Errorf("attachment URL IP %s is not allowed", ip)
	}
	return u, nil
}

func newSSRFSafeHTTPClient() *http.Client {
	if remoteFetchClientOverride != nil {
		return remoteFetchClientOverride
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			var safeIPs []net.IPAddr
			for _, ipAddr := range ips {
				if isDisallowedIP(ipAddr.IP) {
					return nil, fmt.Errorf("refusing connection to non-public IP %s", ipAddr.IP)
				}
				safeIPs = append(safeIPs, ipAddr)
			}
			if len(safeIPs) == 0 {
				return nil, fmt.Errorf("no IP addresses resolved for %s", host)
			}
			var lastErr error
			for _, ipAddr := range safeIPs {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= acpMaxRedirects {
				return fmt.Errorf("too many redirects")
			}
			// Never forward signed query URLs across cross-origin redirects.
			req.Header.Del("Referer")
			_, err := validateRemoteURL(req.URL.String())
			return err
		},
	}
}

func fetchRemoteAttachment(rawURL string, maxBytes int64) ([]byte, string, error) {
	if _, err := validateRemoteURL(rawURL); err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), acpImageFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	client := newSSRFSafeHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetching attachment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetching attachment: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("fetching attachment: %w", err)
	}
	if int64(len(raw)) > maxBytes {
		return nil, "", fmt.Errorf("attachment larger than %d bytes", maxBytes)
	}
	mime := strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0])
	return raw, mime, nil
}

// acpImageBlock turns a data URL or public http(s) URL into an ACP image block. Local paths are
// refused: in ACP mode nothing on the CLIProxyAPI host is readable through a request.
func acpImageBlock(ref string) (acpPromptBlock, error) {
	ref = strings.TrimSpace(ref)
	switch {
	case strings.HasPrefix(ref, "data:"):
		comma := strings.IndexByte(ref, ',')
		if comma < 0 || !strings.Contains(ref[:comma], ";base64") {
			return nil, fmt.Errorf("unsupported data URL: base64 encoding is required")
		}
		mime := strings.TrimPrefix(strings.SplitN(ref[:comma], ";", 2)[0], "data:")
		data := ref[comma+1:]
		if _, err := base64.StdEncoding.DecodeString(data); err != nil {
			return nil, fmt.Errorf("invalid base64 image: %w", err)
		}
		return acpPromptBlock{"type": "image", "mimeType": nonEmpty(mime, "image/png"), "data": data}, nil
	case strings.HasPrefix(ref, "http://"), strings.HasPrefix(ref, "https://"):
		raw, mime, err := fetchRemoteAttachment(ref, acpImageMaxBytes)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(mime, "image/") {
			mime = http.DetectContentType(raw)
		}
		return acpPromptBlock{"type": "image", "mimeType": mime, "data": base64.StdEncoding.EncodeToString(raw)}, nil
	default:
		return nil, fmt.Errorf("only data: and https image URLs are accepted in acp mode")
	}
}

func buildACPBlocks(chatReq chatCompletionRequest, opts agyOptions, cfg pluginConfig) ([]acpPromptBlock, []string, error) {
	var attachments []acpPromptBlock
	prompt, err := buildChatPromptWithAttachments(chatReq, opts.ConversationID, cfg.Workdir, false, &attachments)
	if err != nil {
		return nil, nil, err
	}
	if cfg.MaxPromptBytes > 0 && cfg.MaxPromptAction == promptActionError && len(prompt) > cfg.MaxPromptBytes {
		releaseMediaRefs(mediaPathsFromBlocks(attachments))
		log.Printf("cliproxy-antigravity: rejecting oversized acp prompt (%d bytes > max_prompt_bytes=%d, messages=%d, tools=%d, model=%s)", len(prompt), cfg.MaxPromptBytes, len(chatReq.Messages), len(chatReq.Tools), opts.NativeModel)
		return nil, nil, promptTooLargeErrorWithContext(len(prompt), cfg.MaxPromptBytes, len(chatReq.Messages), len(chatReq.Tools))
	}
	blocks := []acpPromptBlock{{"type": "text", "text": prompt}}
	return append(blocks, attachments...), mediaPathsFromBlocks(attachments), nil
}

func executeNonStreamACP(req rpcExecutorRequest, cfg pluginConfig, opts agyOptions) (agyResult, error) {
	plan, err := planACP(req, opts, cfg)
	if err != nil {
		return agyResult{}, err
	}
	defer plan.releaseMedia()

	result, conv, err := runACP(context.Background(), cfg, opts, plan, nil, nil, nil)
	if err != nil {
		return agyResult{}, err
	}
	declared := declaredToolNames(req.Payload)
	toolCalls, isToolCall := extractToolCalls(result.Response, len(declared) > 0)
	toolCalls = normalizeToolCalls(toolCalls, declared)
	plan.retainConv(conv, result.Response, toolCalls, isToolCall)
	return result, nil
}

// executeStreamACP mirrors executeStream's SSE framing, fed by ACP agent_message_chunk updates.
func executeStreamACP(req rpcExecutorRequest, cfg pluginConfig, opts agyOptions) {
	streamID := req.StreamID
	plan, err := planACP(req, opts, cfg)
	if err != nil {
		closePluginStream(streamID, err)
		return
	}
	defer plan.releaseMedia()

	out := newChatStream(req, "")
	defer out.stopKeepalive()
	keepaliveInterval := streamKeepaliveInterval(cfg)
	var streamed strings.Builder
	emittedLen := 0
	isToolCallStream := false
	toolDecisionPending := opts.HasTools
	onDelta := func(delta string) error {
		streamed.WriteString(delta)
		if toolDecisionPending {
			switch toolCallPrefixState(streamed.String()) {
			case toolPrefixMatch:
				isToolCallStream = true
				toolDecisionPending = false
			case toolPrefixNone:
				toolDecisionPending = false
			}
		}
		if isToolCallStream || toolDecisionPending {
			return nil
		}
		pending := streamed.String()[emittedLen:]
		emittedLen = streamed.Len()
		return out.emit(map[string]any{"content": pending}, nil, nil)
	}

	acpCtx, acpCancel := context.WithCancel(context.Background())
	defer acpCancel()
	result, conv, err := runACP(
		acpCtx,
		cfg,
		opts,
		plan,
		onDelta,
		func() { out.startKeepalive(keepaliveInterval, func(error) { acpCancel() }) },
		func() bool { return !out.hasSent() },
	)
	out.stopKeepalive()
	if kErr := out.keepaliveFailure(); kErr != nil {
		if conv != nil {
			safeGo("closeConv", func() { conv.client.closeSession(conv.sessionID) })
		}
		out.closeError(kErr)
		return
	}
	if err == errACPClientGone {
		return
	}
	if err != nil {
		out.closeError(err)
		return
	}

	fullResponse := nonEmpty(result.Response, streamed.String())
	declared := declaredToolNames(req.Payload)
	toolCalls, isToolCall := extractToolCalls(fullResponse, opts.HasTools)
	toolCalls = normalizeToolCalls(toolCalls, declared)
	isToolCall = isToolCall && opts.HasTools
	plan.retainConv(conv, fullResponse, toolCalls, isToolCall)
	if !isToolCall && strings.TrimSpace(fullResponse) == "" {
		out.closeError(emptyResponseError())
		return
	}
	if isToolCall {
		if err := out.emit(map[string]any{"tool_calls": toolCalls}, nil, nil); err != nil {
			out.closeError(err)
			return
		}
		finish := "tool_calls"
		if err := out.emit(map[string]any{}, &finish, nil); err != nil {
			out.closeError(err)
			return
		}
		if streamWantsUsage(req) {
			if err := out.emit(map[string]any{}, nil, openAIUsage(result.Usage)); err != nil {
				out.closeError(err)
				return
			}
		}
		out.finish()
		return
	}

	if streamed.Len() > emittedLen {
		if err := out.emit(map[string]any{"content": streamed.String()[emittedLen:]}, nil, nil); err != nil {
			out.closeError(err)
			return
		}
	}
	finish := "stop"
	if err := out.emit(map[string]any{}, &finish, nil); err != nil {
		out.closeError(err)
		return
	}
	if streamWantsUsage(req) {
		if err := out.emit(map[string]any{}, nil, openAIUsage(result.Usage)); err != nil {
			out.closeError(err)
			return
		}
	}
	out.finish()
}

// discoverModelsACP lists the models of cfg's account from its resident agy.
func discoverModelsACP(cfg pluginConfig) ([]modelInfo, map[string]string, error) {
	if err := beginACPWork(); err != nil {
		return nil, nil, err
	}
	defer endACPWork()

	c := livePooledACP(cfg)
	switch {
	case c != nil:
	case cfg.AccountHome != "" || processHomeServesRequests():
		var err error
		if c, err = acquireACP(cfg); err != nil {
			return nil, nil, err
		}
	default:
		temp, err := startACP(cfg, "discovery", true)
		if err != nil {
			return nil, nil, err
		}
		defer func() {
			untrackTempACP(temp)
			temp.shutdown(errors.New("discovery finished"))
		}()
		c = temp
	}
	c.busy.Add(1)
	defer c.busy.Add(-1)
	ctx, cancel := context.WithTimeout(context.Background(), acpDiscoverTimeout)
	defer cancel()
	cwd := resolveSessionWorkdir(cfg.Workdir)
	set, err := c.listModels(ctx, cwd)
	if err != nil {
		return nil, nil, err
	}
	var models []modelInfo
	mapping := map[string]string{}
	seen := map[string]bool{}
	add := func(o acpModelOption, description string) {
		id := "agy/" + o.ID
		if seen[id] {
			return
		}
		seen[id] = true
		label := nonEmpty(o.Name, humanizeModelName(o.ID))
		models = append(models, modelInfo{
			ID:                         id,
			Object:                     "model",
			OwnedBy:                    "google-antigravity-cli",
			DisplayName:                label,
			Name:                       id,
			Description:                description,
			SupportedGenerationMethods: []string{"chat"},
			UserDefined:                true,
		})
		mapping[id] = o.ID
		mapping[label] = o.ID
	}
	for _, o := range set.Bases {
		add(o, "Discovered from the resident agy acp session with configurable reasoning effort.")
	}
	for _, o := range set.Variants {
		add(o, "Discovered from the resident agy acp session.")
	}
	if len(models) == 0 {
		return nil, nil, fmt.Errorf("agy acp reported no models")
	}
	return models, mapping, nil
}

func processHomeServesRequests() bool {
	homes, _ := enabledAgyHomes(currentAuthDir())
	for _, home := range homes {
		if home == "" {
			return true
		}
	}
	return false
}

// safeGo runs fn on a goroutine that can never take the CLIProxyAPI process down with a panic.
func safeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("cliproxy-antigravity: %s panic: %v", name, r)
			}
		}()
		fn()
	}()
}
