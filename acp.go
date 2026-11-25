package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Resident `agy acp` mode: one long-lived agy process per account home serves requests
// through ACP sessions so sign-in, config fetches, and process startup are paid once.

const (
	acpInitTimeout      = 30 * time.Second
	acpSessionTimeout   = 60 * time.Second
	acpCloseTimeout     = 5 * time.Second
	acpModelCacheTTL    = 10 * time.Minute
	acpStderrTailBytes  = 4096
	acpCancelGrace      = 3 * time.Second
	acpPermissionCancel = "cancelled"
)

type acpRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *acpRPCError) Error() string { return fmt.Sprintf("agy acp error %d: %s", e.Code, e.Message) }

type acpMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *acpRPCError    `json:"error,omitempty"`
}

type acpReply struct {
	result json.RawMessage
	err    error
}

type acpUpdate struct {
	Kind    string
	Text    string
	Content json.RawMessage
}

// acpSink queues one session's updates without blocking the shared reader.
type acpSink struct {
	mu     sync.Mutex
	items  []acpUpdate
	notify chan struct{}
}

func newACPSink() *acpSink { return &acpSink{notify: make(chan struct{}, 1)} }

func (s *acpSink) push(u acpUpdate) {
	s.mu.Lock()
	s.items = append(s.items, u)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *acpSink) drain() []acpUpdate {
	s.mu.Lock()
	items := s.items
	s.items = nil
	s.mu.Unlock()
	return items
}

type acpModelOption struct {
	ID   string
	Name string
}

type acpModelSet struct {
	Bases    []acpModelOption // configOptions "model" values, e.g. gemini-4-hydrogen
	Variants []acpModelOption // models.availableModels, e.g. gemini-4-hydrogen-high
	Fetched  time.Time
}

func (m acpModelSet) has(id string) bool {
	for _, o := range m.Bases {
		if o.ID == id {
			return true
		}
	}
	for _, o := range m.Variants {
		if o.ID == id {
			return true
		}
	}
	return false
}

type acpClient struct {
	key  string
	home string
	busy atomic.Int64
	cmd  *exec.Cmd
	in   io.WriteCloser
	wmu  sync.Mutex
	next atomic.Int64

	mu       sync.Mutex
	pending  map[int64]chan acpReply
	sessions map[string]*acpSink
	models   acpModelSet

	skipPermissions bool
	dead            chan struct{}
	deadOnce        sync.Once
	deadErr         error
	stderr          *tailBuffer
}

type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	t.mu.Unlock()
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

type acpStart struct {
	done   chan struct{}
	client *acpClient
	err    error
}

var acpPool = struct {
	sync.Mutex
	clients  map[string]*acpClient
	starting map[string]*acpStart
}{clients: map[string]*acpClient{}, starting: map[string]*acpStart{}}

func acpArgs(cfg pluginConfig) []string {
	args := []string{"acp"}
	if cfg.DangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	if cfg.Sandbox {
		args = append(args, "--sandbox")
	}
	return args
}

func acpPoolKey(cfg pluginConfig) string {
	return strings.Join(append([]string{resolveBinaryPath(cfg.BinaryPath), cfg.AccountHome, cfg.Workdir}, acpArgs(cfg)...), "\x00")
}

// acquireACP returns the live resident client for cfg's account, starting one when needed.
func acquireACP(cfg pluginConfig) (*acpClient, error) {
	key := acpPoolKey(cfg)
	acpPool.Lock()
	if c := acpPool.clients[key]; c != nil && c.alive() {
		acpPool.Unlock()
		return c, nil
	}
	if pending := acpPool.starting[key]; pending != nil {
		acpPool.Unlock()
		<-pending.done
		return pending.client, pending.err
	}
	pending := &acpStart{done: make(chan struct{})}
	acpPool.starting[key] = pending
	acpPool.Unlock()

	defer func() {
		acpPool.Lock()
		delete(acpPool.starting, key)
		if pending.err == nil {
			acpPool.clients[key] = pending.client
		}
		acpPool.Unlock()
		close(pending.done)
	}()
	pending.err = errors.New("agy acp start aborted")
	pending.client, pending.err = startACP(cfg, key)
	return pending.client, pending.err
}

// retireACPExcept closes the residents of homes outside keep once their turns finish.
func retireACPExcept(keep map[string]bool) {
	var retired []*acpClient
	acpPool.Lock()
	for key, c := range acpPool.clients {
		if !keep[c.home] {
			delete(acpPool.clients, key)
			retired = append(retired, c)
		}
	}
	acpPool.Unlock()
	for _, c := range retired {
		c := c
		safeGo("retireACP", func() {
			for c.busy.Load() > 0 {
				time.Sleep(time.Second)
			}
			c.shutdown(errors.New("agy acp account disabled"))
		})
	}
}

// livePooledACP returns the running resident for cfg's account, or nil when there is none.
func livePooledACP(cfg pluginConfig) *acpClient {
	acpPool.Lock()
	defer acpPool.Unlock()
	if c := acpPool.clients[acpPoolKey(cfg)]; c != nil && c.alive() {
		return c
	}
	return nil
}

func closeAllACP() {
	acpPool.Lock()
	clients := acpPool.clients
	acpPool.clients = map[string]*acpClient{}
	acpPool.Unlock()
	dropAllConvs()
	for _, c := range clients {
		c.shutdown(errors.New("agy acp pool closed"))
	}
}

func startACP(cfg pluginConfig, key string) (*acpClient, error) {
	if err := validateBinaryPath(cfg.BinaryPath); err != nil {
		return nil, pError("invalid_binary", err.Error(), http.StatusBadRequest)
	}
	binPath := resolveBinaryPath(cfg.BinaryPath)
	cmd := exec.Command(binPath, acpArgs(cfg)...)
	if cfg.Workdir != "" {
		cmd.Dir = cfg.Workdir
	}
	applyAccountEnv(cmd, cfg)
	configureSysProcAttr(cmd)
	// The resident process outlives any request context; Cancel is only valid with CommandContext,
	// and shutdown kills the process group itself.
	cmd.Cancel = nil
	stderr := &tailBuffer{max: acpStderrTailBytes}
	cmd.Stderr = stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, classifyAgyError(err, "")
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, classifyAgyError(err, "")
	}
	if err := cmd.Start(); err != nil {
		return nil, classifyAgyError(err, stderr.String())
	}
	c := &acpClient{
		key:             key,
		home:            cfg.AccountHome,
		cmd:             cmd,
		in:              in,
		pending:         map[int64]chan acpReply{},
		sessions:        map[string]*acpSink{},
		skipPermissions: cfg.DangerouslySkipPermissions,
		dead:            make(chan struct{}),
		stderr:          stderr,
	}
	go c.readLoop(out)

	ctx, cancel := context.WithTimeout(context.Background(), acpInitTimeout)
	defer cancel()
	_, err = c.call(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
		"clientInfo":         map[string]any{"name": pluginID, "version": pluginVersion},
	})
	if err != nil {
		c.shutdown(err)
		return nil, c.wrapErr(err)
	}
	return c, nil
}

func (c *acpClient) alive() bool {
	select {
	case <-c.dead:
		return false
	default:
		return true
	}
}

func (c *acpClient) shutdown(reason error) {
	c.markDead(reason)
	_ = c.in.Close()
	killProcessTree(c.cmd)
}

func (c *acpClient) markDead(reason error) {
	c.deadOnce.Do(func() {
		if reason == nil {
			reason = errors.New("agy acp exited")
		}
		c.deadErr = reason
		close(c.dead)
		c.mu.Lock()
		for id, ch := range c.pending {
			ch <- acpReply{err: reason}
			delete(c.pending, id)
		}
		for _, sink := range c.sessions {
			sink.push(acpUpdate{Kind: "__dead"})
		}
		c.mu.Unlock()
	})
}

func (c *acpClient) readLoop(out io.Reader) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("cliproxy-antigravity: acp reader panic: %v", r)
		}
		c.markDead(errors.New("agy acp exited"))
		// agy spawns a language-server child in its process group; reap the whole group.
		killProcessTree(c.cmd)
		_ = c.cmd.Wait()
	}()
	scan := bufio.NewScanner(out)
	scan.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scan.Scan() {
		line := bytes.TrimSpace(scan.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg acpMessage
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		c.dispatch(msg)
	}
	if err := scan.Err(); err != nil {
		c.markDead(fmt.Errorf("reading agy acp output: %w", err))
	}
}

func (c *acpClient) dispatch(msg acpMessage) {
	hasID := len(msg.ID) > 0 && string(msg.ID) != "null"
	switch {
	case msg.Method != "" && hasID:
		c.answerAgentRequest(msg)
	case msg.Method == "session/update":
		var p struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				Kind    string          `json:"sessionUpdate"`
				Content json.RawMessage `json:"content"`
			} `json:"update"`
		}
		if json.Unmarshal(msg.Params, &p) != nil {
			return
		}
		c.mu.Lock()
		sink := c.sessions[p.SessionID]
		c.mu.Unlock()
		if sink == nil {
			return
		}
		u := acpUpdate{Kind: p.Update.Kind, Content: p.Update.Content}
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(p.Update.Content, &block) == nil && block.Type == "text" {
			u.Text = block.Text
		}
		sink.push(u)
	case msg.Method == "" && hasID:
		var id int64
		if json.Unmarshal(msg.ID, &id) != nil {
			return
		}
		c.mu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ch == nil {
			return
		}
		if msg.Error != nil {
			ch <- acpReply{err: msg.Error}
			return
		}
		ch <- acpReply{result: msg.Result}
	}
}

// answerAgentRequest refuses every agent->client request: built-in tool permissions are denied
// (so agy cannot touch the CLIProxyAPI host) and fs/terminal methods are not offered.
func (c *acpClient) answerAgentRequest(msg acpMessage) {
	if msg.Method == "session/request_permission" {
		outcome := map[string]any{"outcome": acpPermissionCancel}
		if c.skipPermissions {
			if id := firstAllowOption(msg.Params); id != "" {
				outcome = map[string]any{"outcome": "selected", "optionId": id}
			}
		}
		_ = c.write(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"outcome": outcome}})
		return
	}
	_ = c.write(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32601, "message": "not supported by cliproxy-antigravity"}})
}

func firstAllowOption(params json.RawMessage) string {
	var p struct {
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	if json.Unmarshal(params, &p) != nil {
		return ""
	}
	for _, o := range p.Options {
		if strings.HasPrefix(o.Kind, "allow") {
			return o.OptionID
		}
	}
	return ""
}

func (c *acpClient) write(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.in.Write(raw)
	return err
}

func (c *acpClient) notify(method string, params any) {
	_ = c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *acpClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.next.Add(1)
	ch := make(chan acpReply, 1)
	c.mu.Lock()
	if !c.alive() {
		c.mu.Unlock()
		return nil, c.deadErr
	}
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.forget(id)
		c.shutdown(fmt.Errorf("writing to agy acp: %w", err))
		return nil, err
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		c.forget(id)
		return nil, ctx.Err()
	}
}

func (c *acpClient) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *acpClient) register(sessionID string) *acpSink {
	sink := newACPSink()
	c.mu.Lock()
	c.sessions[sessionID] = sink
	c.mu.Unlock()
	return sink
}

func (c *acpClient) unregister(sessionID string) {
	c.mu.Lock()
	delete(c.sessions, sessionID)
	c.mu.Unlock()
}

func (c *acpClient) wrapErr(err error) error {
	if err == nil {
		return nil
	}
	var pe *pluginError
	if errors.As(err, &pe) {
		return err
	}
	var rpcErr *acpRPCError
	if errors.As(err, &rpcErr) {
		return classifyAgyError(errors.New(rpcErr.Message), "")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return classifyAgyError(err, "")
	}
	return classifyAgyError(err, c.stderr.String())
}

type acpSessionInfo struct {
	SessionID     string `json:"sessionId"`
	ConfigOptions []struct {
		ID       string `json:"id"`
		Category string `json:"category"`
		Options  []struct {
			Value string `json:"value"`
			Name  string `json:"name"`
		} `json:"options"`
	} `json:"configOptions"`
	Models struct {
		AvailableModels []struct {
			ModelID string `json:"modelId"`
			Name    string `json:"name"`
		} `json:"availableModels"`
	} `json:"models"`
}

func (s acpSessionInfo) modelSet() acpModelSet {
	set := acpModelSet{Fetched: time.Now()}
	for _, o := range s.ConfigOptions {
		if o.Category != "model" && o.ID != "model" {
			continue
		}
		for _, v := range o.Options {
			if v.Value != "" {
				set.Bases = append(set.Bases, acpModelOption{ID: v.Value, Name: v.Name})
			}
		}
	}
	for _, m := range s.Models.AvailableModels {
		if m.ModelID != "" {
			set.Variants = append(set.Variants, acpModelOption{ID: m.ModelID, Name: m.Name})
		}
	}
	return set
}

func (c *acpClient) newSession(ctx context.Context, cwd string) (acpSessionInfo, error) {
	var info acpSessionInfo
	sctx, cancel := context.WithTimeout(ctx, acpSessionTimeout)
	defer cancel()
	raw, err := c.call(sctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(raw, &info); err != nil || info.SessionID == "" {
		return info, pError("invalid_upstream_response", "agy acp returned no session ID", http.StatusBadGateway)
	}
	if set := info.modelSet(); len(set.Bases)+len(set.Variants) > 0 {
		c.mu.Lock()
		c.models = set
		c.mu.Unlock()
	}
	return info, nil
}

func (c *acpClient) closeSession(sessionID string) {
	c.unregister(sessionID)
	if !c.alive() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), acpCloseTimeout)
	defer cancel()
	_, _ = c.call(ctx, "session/close", map[string]any{"sessionId": sessionID})
}

// listModels returns the account's ACP model set, opening a throwaway session when the cache is stale.
func (c *acpClient) listModels(ctx context.Context, cwd string) (acpModelSet, error) {
	c.mu.Lock()
	cached := c.models
	c.mu.Unlock()
	if len(cached.Bases)+len(cached.Variants) > 0 && time.Since(cached.Fetched) < acpModelCacheTTL {
		return cached, nil
	}
	info, err := c.newSession(ctx, cwd)
	if err != nil {
		return acpModelSet{}, err
	}
	c.closeSession(info.SessionID)
	return info.modelSet(), nil
}

// acpModelChoice maps the plugin's model and effort to an ACP model ID, preferring the effort
// variant (e.g. gemini-4-hydrogen-xhigh) and falling back to the bare model.
func acpModelChoice(opts agyOptions, models acpModelSet) string {
	native := strings.TrimSpace(opts.NativeModel)
	if native == "" {
		return ""
	}
	effort := strings.TrimSpace(opts.ReasoningEffort)
	if effort != "" && extractEffortSuffix(native) == "" {
		candidates := []string{native + "-" + effort}
		switch effort {
		case "xhigh":
			candidates = append(candidates, native+"-x-high", native+"-extra-high")
		case "medium":
			candidates = append(candidates, native+"-med")
		}
		for _, candidate := range candidates {
			if models.has(candidate) {
				return candidate
			}
		}
	}
	return native
}

type acpPromptBlock = map[string]any

func acpUsageToAgy(raw json.RawMessage) agyUsage {
	var u struct {
		InputTokens      int `json:"inputTokens"`
		OutputTokens     int `json:"outputTokens"`
		CachedReadTokens int `json:"cachedReadTokens"`
		ThoughtTokens    int `json:"thoughtTokens"`
		TotalTokens      int `json:"totalTokens"`
	}
	_ = json.Unmarshal(raw, &u)
	// ACP inputTokens excludes cache reads; OpenAI prompt_tokens counts them (cached_tokens is a subset).
	input := u.InputTokens + u.CachedReadTokens
	return agyUsage{
		InputTokens:     input,
		OutputTokens:    u.OutputTokens,
		ThinkingTokens:  u.ThoughtTokens,
		CacheReadTokens: u.CachedReadTokens,
		TotalTokens:     input + u.OutputTokens,
	}
}

var errACPClientGone = errors.New("client stream closed")

// acpInFlight counts running ACP turns so a quiesce can wait for them before closing residents.
var acpInFlight atomic.Int64

func drainACP(limit time.Duration) {
	safeGo("drainACP", func() {
		deadline := time.Now().Add(limit)
		for acpInFlight.Load() > 0 && time.Now().Before(deadline) {
			time.Sleep(time.Second)
		}
		closeAllACP()
	})
}

// runACP executes one prompt on the resident agy for cfg's account. onDelta, when set, receives
// assistant text as it streams; returning an error from it cancels the ACP turn.
func runACP(cfg pluginConfig, opts agyOptions, plan acpPlan, onDelta func(string) error) (agyResult, *acpConv, error) {
	acpInFlight.Add(1)
	defer acpInFlight.Add(-1)
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout(cfg))
	defer cancel()

	emitted := false
	wrapped := func(text string) error {
		emitted = true
		if onDelta == nil {
			return nil
		}
		return onDelta(text)
	}
	blocks, reuse := plan.full, (*acpConv)(nil)
	if plan.reuse != nil {
		blocks, reuse = plan.follow, plan.reuse
	}
	result, conv, err, retryable := runACPOnce(ctx, cfg, opts, blocks, wrapped, reuse)
	// A reused session that is gone (agy restarted, session evicted) is replayed in full, as is a
	// fresh session whose resident died; both only before any text reached the client.
	if err != nil && !emitted && ctx.Err() == nil && (retryable || (reuse != nil && sessionGone(err))) {
		result, conv, err, _ = runACPOnce(ctx, cfg, opts, plan.full, wrapped, nil)
	}
	return result, conv, err
}

func sessionGone(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "session") && (strings.Contains(msg, "not found") || strings.Contains(msg, "unknown") || strings.Contains(msg, "no such"))
}

// runACPOnce runs one turn on reuse's session, or a fresh one. A successful turn leaves the session
// open for the caller to retain; retryable=true only when the resident died under the request.
func runACPOnce(ctx context.Context, cfg pluginConfig, opts agyOptions, blocks []acpPromptBlock, onDelta func(string) error, reuse *acpConv) (agyResult, *acpConv, error, bool) {
	var c *acpClient
	var sessionID string
	if reuse != nil {
		c, sessionID = reuse.client, reuse.sessionID
		c.busy.Add(1)
		defer c.busy.Add(-1)
		if !c.alive() {
			return agyResult{}, nil, errors.New("resident agy of the reused session exited"), true
		}
	} else {
		var err error
		if c, err = acquireACP(cfg); err != nil {
			return agyResult{}, nil, err, false
		}
		c.busy.Add(1)
		defer c.busy.Add(-1)
		info, err := c.newSession(ctx, nonEmpty(cfg.Workdir, "."))
		if err != nil {
			return agyResult{}, nil, c.wrapErr(err), !c.alive()
		}
		sessionID = info.SessionID
		if model := acpModelChoice(opts, info.modelSet()); model != "" {
			if _, err := c.call(ctx, "session/set_model", map[string]any{"sessionId": sessionID, "modelId": model}); err != nil {
				c.closeSession(sessionID)
				var rpcErr *acpRPCError
				if errors.As(err, &rpcErr) {
					return agyResult{}, nil, pError(errorCodeModelNotFound, "Antigravity model not available: "+model+" ("+rpcErr.Message+")", http.StatusNotFound), false
				}
				return agyResult{}, nil, c.wrapErr(err), !c.alive()
			}
		}
	}

	sink := c.register(sessionID)
	result, err, retryable := runACPTurn(ctx, c, sessionID, sink, blocks, onDelta)
	if err != nil {
		c.closeSession(sessionID)
		return result, nil, err, retryable
	}
	c.unregister(sessionID)
	return result, &acpConv{client: c, sessionID: sessionID}, nil, false
}

// runACPTurn sends one prompt on an open session and streams its reply through onDelta.
func runACPTurn(ctx context.Context, c *acpClient, sessionID string, sink *acpSink, blocks []acpPromptBlock, onDelta func(string) error) (agyResult, error, bool) {
	done := make(chan acpPromptOutcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- acpPromptOutcome{err: fmt.Errorf("acp prompt panic: %v", r)}
			}
		}()
		raw, err := c.call(ctx, "session/prompt", map[string]any{"sessionId": sessionID, "prompt": blocks})
		done <- acpPromptOutcome{raw: raw, err: err}
	}()

	var text strings.Builder
	cancelTurn := func() {
		c.notify("session/cancel", map[string]any{"sessionId": sessionID})
	}
	handle := func(items []acpUpdate) error {
		for _, u := range items {
			if u.Kind != "agent_message_chunk" || u.Text == "" {
				continue
			}
			text.WriteString(u.Text)
			if err := onDelta(u.Text); err != nil {
				return err
			}
		}
		return nil
	}

	for {
		select {
		case <-sink.notify:
			if err := handle(sink.drain()); err != nil {
				cancelTurn()
				waitPrompt(done)
				return agyResult{}, errACPClientGone, false
			}
		case out := <-done:
			if err := handle(sink.drain()); err != nil {
				return agyResult{}, errACPClientGone, false
			}
			if out.err != nil {
				if ctx.Err() != nil {
					cancelTurn()
					return agyResult{}, classifyAgyError(context.DeadlineExceeded, ""), false
				}
				return agyResult{}, c.wrapErr(out.err), !c.alive()
			}
			var res struct {
				StopReason string          `json:"stopReason"`
				Usage      json.RawMessage `json:"usage"`
			}
			_ = json.Unmarshal(out.raw, &res)
			if res.StopReason == "cancelled" {
				return agyResult{}, pRetryableError("agy_cancelled", "Antigravity turn was cancelled", http.StatusBadGateway), false
			}
			if res.StopReason == "refusal" {
				return agyResult{}, pError("agy_refusal", "Antigravity refused the request", http.StatusBadRequest), false
			}
			return agyResult{
				ConversationID: sessionID,
				Status:         "SUCCESS",
				Response:       text.String(),
				Usage:          acpUsageToAgy(res.Usage),
			}, nil, false
		case <-ctx.Done():
			cancelTurn()
			waitPrompt(done)
			return agyResult{}, classifyAgyError(context.DeadlineExceeded, ""), false
		}
	}
}

type acpPromptOutcome struct {
	raw json.RawMessage
	err error
}

// waitPrompt lets a cancelled turn settle so the session can be closed cleanly.
func waitPrompt(done <-chan acpPromptOutcome) {
	select {
	case <-done:
	case <-time.After(acpCancelGrace):
	}
}
