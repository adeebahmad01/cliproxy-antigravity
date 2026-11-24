package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testChatPrompt(t *testing.T, payload []byte, nativeID, workdir string) (string, error) {
	t.Helper()
	req, err := decodeChatRequest(payload)
	if err != nil {
		return "", err
	}
	return buildChatPrompt(req, nativeID, workdir)
}

func TestBuildPromptPreservesConversationRoles(t *testing.T) {
	payload := []byte(`{
      "model":"agy/default",
      "messages":[
        {"role":"system","content":"Be concise."},
        {"role":"user","content":"Hello"},
        {"role":"assistant","content":"Hi"},
        {"role":"user","content":[{"type":"text","text":"Explain Go."}]}
      ]
    }`)
	prompt, err := testChatPrompt(t, payload, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"role=system", "Be concise.", "role=user", "Explain Go."} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildPromptResumedConversation(t *testing.T) {
	payload := []byte(`{
      "model":"agy/default",
      "conversation_id":"conv-abc",
      "messages":[
        {"role":"user","content":"First turn"},
        {"role":"assistant","content":"First answer"},
        {"role":"user","content":"Second turn"}
      ]
    }`)
	prompt, err := testChatPrompt(t, payload, "conv-abc", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "First turn") {
		t.Fatalf("resumed prompt should not duplicate prior user message:\n%s", prompt)
	}
	if strings.Contains(prompt, "First answer") {
		t.Fatalf("resumed prompt should not duplicate prior assistant message:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Second turn") {
		t.Fatalf("resumed prompt missing latest user turn:\n%s", prompt)
	}
}

func TestBuildPromptStagesBase64Image(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cliproxy-img-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	fakePNG := base64.StdEncoding.EncodeToString([]byte("fake-png-content-data"))
	payload := []byte(`{
      "model":"agy/default",
      "messages":[
        {
          "role":"user",
          "content":[
            {"type":"text","text":"Look at this image"},
            {"type":"image_url","image_url":{"url":"data:image/png;base64,` + fakePNG + `"}}
          ]
        }
      ]
    }`)

	prompt, err := testChatPrompt(t, payload, "", tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "[Attached image:") || !strings.Contains(prompt, ".png") {
		t.Fatalf("expected staged image tag in prompt, got:\n%s", prompt)
	}
}

func TestBuildPromptInjectsClientToolsInstruction(t *testing.T) {
	payload := []byte(`{
      "model":"agy/default",
      "messages":[{"role":"user","content":"What is the weather?"}],
      "tools":[
        {
          "type":"function",
          "function":{"name":"get_weather","description":"Get current weather"}
        }
      ]
    }`)

	prompt, err := testChatPrompt(t, payload, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "[SYSTEM INSTRUCTION: CLIENT-SIDE TOOL CALLING]") {
		t.Fatalf("expected tool calling instructions in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "get_weather") {
		t.Fatalf("expected tool schema in prompt:\n%s", prompt)
	}
}

func TestExtractToolCalls(t *testing.T) {
	textWithFence := "Here is the tool call:\n```tool_calls\n[\n  {\n    \"name\": \"calc\",\n    \"arguments\": {\"expr\": \"2+2\"}\n  }\n]\n```"
	calls, ok := extractToolCalls(textWithFence, true)
	if !ok || len(calls) != 1 {
		t.Fatalf("failed to extract fenced tool calls: ok=%v, len=%d", ok, len(calls))
	}
	if calls[0].Function.Name != "calc" {
		t.Fatalf("call name = %q, want 'calc'", calls[0].Function.Name)
	}
	if !strings.Contains(calls[0].Function.Arguments, "2+2") {
		t.Fatalf("call args = %q", calls[0].Function.Arguments)
	}
}

func TestMakeChatCompletionRejectsEmptyResponse(t *testing.T) {
	for _, response := range []string{"", "   \n\t "} {
		_, err := makeChatCompletion("agy/gemini-4-argon-high", nil, agyResult{
			Status:   "SUCCESS",
			Response: response,
		})
		if err == nil {
			t.Fatalf("empty response %q produced a completion instead of an error", response)
		}
		var coded *pluginError
		if !errors.As(err, &coded) || coded.Code != "empty_assistant_response" {
			t.Fatalf("error = %v, want empty_assistant_response", err)
		}
		if coded.HTTPStatus != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", coded.HTTPStatus)
		}
	}
}

func TestMakeChatCompletionMapsToolCalls(t *testing.T) {
	raw, err := makeChatCompletion("gemini-3.8-flash-high", nil, agyResult{
		Status:   "SUCCESS",
		Response: "```tool_calls\n[{\"name\":\"search\",\"arguments\":{\"query\":\"golang\"}}]\n```",
	})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	choices := body["choices"].([]any)
	choice0 := choices[0].(map[string]any)
	if choice0["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v, want 'tool_calls'", choice0["finish_reason"])
	}
	msg := choice0["message"].(map[string]any)
	if msg["content"] != nil {
		t.Fatalf("content should be nil for tool_calls, got %v", msg["content"])
	}
	toolCalls := msg["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
}

func TestMakeChatCompletionMapsUsageAndConversation(t *testing.T) {
	raw, err := makeChatCompletion("agy/test", nil, agyResult{
		ConversationID: "conv-xyz-99",
		Status:         "SUCCESS",
		Response:       "done",
		Usage: agyUsage{
			InputTokens: 10, OutputTokens: 5, ThinkingTokens: 2, CacheReadTokens: 3, TotalTokens: 15,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "agy/test" {
		t.Fatalf("model = %#v", body["model"])
	}
	if body["conversation_id"] != "conv-xyz-99" {
		t.Fatalf("conversation_id = %#v, want 'conv-xyz-99'", body["conversation_id"])
	}
	choices := body["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "done" {
		t.Fatalf("content = %#v", message["content"])
	}
}

func TestMakeChatChunkIncludesConversationID(t *testing.T) {
	raw := makeChatChunk("id", 1, "agy/test", map[string]any{"content": "hello"}, nil, nil, "conv-stream-1")
	if !json.Valid(raw) || !strings.Contains(string(raw), `"conversation_id":"conv-stream-1"`) {
		t.Fatalf("expected conversation_id in JSON chunk: %s", raw)
	}
}

func TestMakeChatUsageChunkHasEmptyChoices(t *testing.T) {
	raw := makeChatChunk("id", 1, "agy/test", map[string]any{}, nil, map[string]any{"total_tokens": 1}, "")
	if !json.Valid(raw) || !strings.Contains(string(raw), `"choices":[]`) {
		t.Fatalf("unexpected usage chunk: %s", raw)
	}
}

func TestBuildPromptPutsToolInstructionBeforeMessages(t *testing.T) {
	payload := []byte(`{
      "model":"agy/default",
      "messages":[{"role":"user","content":"Hello"}],
      "tools":[{"type":"function","function":{"name":"bash","description":"Run commands"}}]
    }`)

	prompt, err := testChatPrompt(t, payload, "", "")
	if err != nil {
		t.Fatal(err)
	}
	toolIdx := strings.Index(prompt, "[SYSTEM INSTRUCTION: CLIENT-SIDE TOOL CALLING]")
	msgIdx := strings.Index(prompt, "--- message 1")
	if toolIdx < 0 || msgIdx < 0 {
		t.Fatalf("prompt is missing the tool instruction or the first message:\n%s", prompt[:200])
	}
	if toolIdx > msgIdx {
		t.Fatalf("tool instruction must precede the conversation so tail truncation cannot drop it")
	}
}

// Models embed markdown fences inside string arguments, which used to cut the
// fenced tool_calls payload at the first inner fence and degrade it to text.
func TestExtractToolCallsWithEmbeddedFences(t *testing.T) {
	response := "```tool_calls\n[\n  {\n    \"name\": \"subagent\",\n    \"arguments\": {\n" +
		"      \"agent\": \"worker\",\n" +
		"      \"task\": \"Please do this:\\n```bash\\nmake test\\n```\\nAnd also ```json\\n{\\\"a\\\":1}\\n```\\nthanks\"\n" +
		"    }\n  }\n]\n```"
	calls, ok := extractToolCalls(response, true)
	if !ok || len(calls) != 1 {
		t.Fatalf("expected one recovered tool call, got ok=%v len=%d", ok, len(calls))
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments are not valid JSON: %v (%s)", err, calls[0].Function.Arguments)
	}
	if args["agent"] != "worker" {
		t.Fatalf("agent = %v, want worker", args["agent"])
	}
}

func TestExtractToolCallsUnquotesStringArguments(t *testing.T) {
	calls, ok := extractToolCalls("```tool_calls\n[{\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\\\"ls\\\"}\"}]\n```", true)
	if !ok || len(calls) != 1 {
		t.Fatalf("ok=%v len=%d", ok, len(calls))
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments are not a JSON object: %v (raw=%s)", err, calls[0].Function.Arguments)
	}
	if args["command"] != "ls" {
		t.Fatalf("command = %v, want ls", args["command"])
	}
}

func TestExtractToolCallsUnfencedWithTrailingProse(t *testing.T) {
	calls, ok := extractToolCalls("[{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}]\n\n以上是调用说明。", true)
	if !ok || len(calls) != 1 {
		t.Fatalf("bare payload with trailing prose was not recovered: ok=%v len=%d", ok, len(calls))
	}

	// Without declared tools a bare JSON body is ordinary text, not a tool call.
	if _, ok := extractToolCalls("[{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}]", false); ok {
		t.Fatalf("bare JSON must not become a tool call when no tools were declared")
	}
}

func TestNormalizeArgumentsKeepsPlainObject(t *testing.T) {
	if got := normalizeArguments(json.RawMessage(`{"command":"ls"}`)); got != `{"command":"ls"}` {
		t.Fatalf("got %q", got)
	}
	if got := normalizeArguments(nil); got != "{}" {
		t.Fatalf("nil arguments = %q, want {}", got)
	}
	if got := normalizeArguments(json.RawMessage(`"not json"`)); got != `"not json"` {
		t.Fatalf("non-JSON string should be left alone, got %q", got)
	}
	if got := normalizeArguments(json.RawMessage(`"null"`)); got != "{}" {
		t.Fatalf("quoted null arguments = %q, want {}", got)
	}
}

func TestDeclaredToolNames(t *testing.T) {
	payload := []byte(`{"tools":[{"type":"function","function":{"name":"bash"}},{"type":"function","function":{"name":"subagent"}}]}`)
	got := declaredToolNames(payload)
	if len(got) != 2 || got[0] != "bash" || got[1] != "subagent" {
		t.Fatalf("declaredToolNames = %v", got)
	}
	if names := declaredToolNames([]byte(`{"model":"agy/default"}`)); len(names) != 0 {
		t.Fatalf("expected no names, got %v", names)
	}
}

// An ordinary answer that embeds an example ```json block must not be parsed as
// a tool call just because the tools were declared.
func TestFencedJSONOnlyCountsAtTheStart(t *testing.T) {
	response := "Here is an example payload your API accepts:\n\n```json\n{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}\n```\n\nThat is all."
	if calls, ok := extractToolCalls(response, true); ok {
		t.Fatalf("embedded example block became a tool call: %+v", calls)
	}

	// The same payload at the start of the response is still accepted.
	payload := "```json\n[{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}]\n```"
	calls, ok := extractToolCalls(payload, true)
	if !ok || len(calls) != 1 || calls[0].Function.Name != "bash" {
		t.Fatalf("leading ```json payload was not accepted: ok=%v calls=%v", ok, calls)
	}
}

// ```tool_calls may still be preceded by prose.
func TestToolCallsFenceAllowsLeadingProse(t *testing.T) {
	calls, ok := extractToolCalls("Here is the call:\n\n```tool_calls\n[{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}]\n```", true)
	if !ok || len(calls) != 1 {
		t.Fatalf("leading prose before the fence broke extraction: ok=%v", ok)
	}
}

// A fenced payload that is not JSON ("```json null```") must not cause a search
// past the closing fence into the prose that follows it.
func TestFencedPayloadDoesNotLeakPastTheClosingFence(t *testing.T) {
	response := "```json\nnull\n```\nExample: {\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}"
	if calls, ok := extractToolCalls(response, false); ok {
		t.Fatalf("prose after the closing fence became a tool call: %+v", calls)
	}
	if calls, ok := extractToolCalls(response, true); ok {
		t.Fatalf("prose after the closing fence became a tool call: %+v", calls)
	}
}

// A bare payload whose arguments contain a fence marker must survive: the fence
// search would otherwise pick that marker up first.
func TestBarePayloadWithFenceMarkerInArguments(t *testing.T) {
	response := "[{\"name\":\"bash\",\"arguments\":{\"command\":\"echo ```json {}\"}}]"
	calls, ok := extractToolCalls(response, true)
	if !ok || len(calls) != 1 {
		t.Fatalf("bare payload with a fence marker in its arguments was dropped: ok=%v", ok)
	}
	if calls[0].Function.Name != "bash" {
		t.Fatalf("name = %q, want bash", calls[0].Function.Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments invalid: %v", err)
	}
	if args["command"] != "echo ```json {}" {
		t.Fatalf("command = %v", args["command"])
	}
}

// The fenced fallback still works when a bare candidate does not decode or carries no calls.
func TestBareCandidateFailureFallsBackToFence(t *testing.T) {
	cases := []string{
		"[not json]\n\n```tool_calls\n[{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}]\n```",
		"[]\n\n```tool_calls\n[{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}]\n```",
		"[{}]\n\n```tool_calls\n[{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}]\n```",
		"[{\"name\":\"   \"}]\n\n```tool_calls\n[{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}]\n```",
	}
	for _, response := range cases {
		calls, ok := extractToolCalls(response, true)
		if !ok || len(calls) != 1 || calls[0].Function.Name != "bash" {
			t.Fatalf("fenced fallback lost for %q: ok=%v calls=%v", response, ok, calls)
		}
	}
}

// A ```tool_calls marker inside a string argument of a leading ```json payload
// must not become the start point.
func TestLeadingJSONFenceWithToolCallsMarkerInArguments(t *testing.T) {
	response := "```json\n[{\"name\":\"bash\",\"arguments\":{\"command\":\"echo ```tool_calls {}\"}}]\n```"
	calls, ok := extractToolCalls(response, false)
	if !ok || len(calls) != 1 {
		t.Fatalf("leading json payload with a marker in its arguments was dropped: ok=%v", ok)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments invalid: %v", err)
	}
	if args["command"] != "echo ```tool_calls {}" {
		t.Fatalf("command = %v", args["command"])
	}
}

// A leading example block followed by a genuine tool_calls fence still resolves.
func TestLeadingJSONExampleThenRealToolCallsFence(t *testing.T) {
	response := "```json\n{\"note\":\"example only\"}\n```\n\nHere is the call:\n\n```tool_calls\n[{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}]\n```"
	calls, ok := extractToolCalls(response, false)
	if !ok || len(calls) != 1 || calls[0].Function.Name != "bash" {
		t.Fatalf("genuine fence after a leading example block was lost: ok=%v calls=%v", ok, calls)
	}
}

func TestResolveToolNameIsStrict(t *testing.T) {
	declared := []string{"read", "bash", "subagent", "codemode", "tools:search", "read-file"}
	cases := map[string]string{
		"subagent":      "subagent", // exact
		"Bash":          "bash",     // case-insensitive
		"Agent":         "subagent", // explicit alias
		"agent":         "subagent",
		"tools:search":  "tools:search",
		"search":        "tools:search",  // delimiter-safe namespace match
		"mcp:read-file": "read-file",     // namespace prefix over kebab-case tool name
		"write-file":    "write-file",    // hyphenated tool names must not match on trailing word
		"file":          "file",          // bare suffix after hyphen must not snap to read-file
		"write_file":    "write_file",    // unknown stays untouched
		"readSomething": "readSomething", // no suffix matching
	}
	for input, want := range cases {
		if got := resolveToolName(input, declared); got != want {
			t.Fatalf("resolveToolName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestResolveToolNameSkipsAmbiguousNamespace(t *testing.T) {
	declared := []string{"fs:read", "net:read"}
	if got := resolveToolName("read", declared); got != "read" {
		t.Fatalf("ambiguous segment match must not be guessed, got %q", got)
	}
}

func TestNormalizeToolCallsRewritesAlias(t *testing.T) {
	raw, err := makeChatCompletion("agy/test", []string{"read", "subagent"}, agyResult{
		Status:   "SUCCESS",
		Response: "```tool_calls\n[{\"name\":\"Agent\",\"arguments\":{\"agent\":\"scout\",\"task\":\"look\"}}]\n```",
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Choices []struct {
			Message struct {
				ToolCalls []openAIToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Choices) == 0 || len(payload.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("no tool call in payload: %s", raw)
	}
	if got := payload.Choices[0].Message.ToolCalls[0].Function.Name; got != "subagent" {
		t.Fatalf("tool name = %q, want subagent", got)
	}
}

func TestToolInstructionPinsExactNames(t *testing.T) {
	payload := []byte(`{"model":"agy/default","messages":[{"role":"user","content":"hi"}],
      "tools":[{"type":"function","function":{"name":"subagent","description":"delegate"}},
               {"type":"function","function":{"name":"bash","description":"run"}}]}`)
	prompt, err := testChatPrompt(t, payload, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "never a paraphrase") || !strings.Contains(prompt, "subagent, bash") {
		t.Fatalf("tool instruction does not pin exact names:\n%s", prompt[:400])
	}
}

func stagedRequest(t *testing.T, promptBytes int) rpcExecutorRequest {
	t.Helper()
	blob := strings.Repeat("x", promptBytes)
	payload := []byte(`{"model":"agy/default","messages":[{"role":"user","content":"` + blob + `"}]}`)
	req := rpcExecutorRequest{Model: "agy/default", Payload: payload}
	return req
}

func TestPrepareAgyExecutionStagesOnlyWhenConfigured(t *testing.T) {
	useTempStagingDir(t)
	setConfig(pluginConfig{BinaryPath: "agy", MaxPromptBytes: 200000})
	t.Cleanup(func() { setConfig(defaultConfig()) })

	// Under the threshold: inline prompt, nothing staged, nothing to clean up.
	run, err := prepareAgyExecution(stagedRequest(t, 1000))
	if err != nil {
		t.Fatal(err)
	}
	if run.stagedBytes != 0 || run.cleanup != nil {
		t.Fatalf("small request was staged: %d", run.stagedBytes)
	}

	// Over the threshold: staged, handoff prompt names the file, cleanup removes it.
	run, err = prepareAgyExecution(stagedRequest(t, 220000))
	if err != nil {
		t.Fatal(err)
	}
	if run.stagedBytes <= 200000 {
		t.Fatalf("stagedBytes = %d, want the full prompt size", run.stagedBytes)
	}
	if len(run.prompt) > 4096 {
		t.Fatalf("handoff prompt is %d bytes, expected the short wrapper", len(run.prompt))
	}
	path := stagedPathFromHandoff(t, run.prompt)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("staged file missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("staged file mode = %04o, want 0600", perm)
	}
	if run.stagedBytes != int(info.Size()) {
		t.Fatalf("stagedBytes = %d, file size = %d", run.stagedBytes, info.Size())
	}
	run.cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("staged file not removed: %v", err)
	}
	if entries, _ := os.ReadDir(promptStagingDir()); len(entries) != 0 {
		t.Fatalf("staging root not empty after cleanup: %v", entries)
	}
}

// useTempStagingDir isolates the shared staging root for a test.
func useTempStagingDir(t *testing.T) {
	t.Helper()
	previous := stagingDirOverride
	dir := filepath.Join(t.TempDir(), "cliproxy-prompts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stagingDirOverride = dir
	t.Cleanup(func() { stagingDirOverride = previous })
}

func stagedPathFromHandoff(t *testing.T, handoff string) string {
	t.Helper()
	start := strings.Index(handoff, promptStagingDir())
	if start < 0 {
		t.Fatalf("handoff does not reference the staging dir: %s", handoff)
	}
	rest := handoff[start:]
	end := strings.IndexAny(rest, " \n")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

// Staging is off by default: no threshold, no file, whatever the size.
func TestPrepareAgyExecutionDoesNotStageByDefault(t *testing.T) {
	setConfig(defaultConfig())
	t.Cleanup(func() { setConfig(defaultConfig()) })
	run, err := prepareAgyExecution(stagedRequest(t, 220000))
	if err != nil {
		t.Fatal(err)
	}
	if run.stagedBytes != 0 || run.cleanup != nil {
		t.Fatalf("default config staged a request: %+v", run.stagedBytes)
	}
}

// The staged copy keeps the tool contract at the top (PR #26 ordering) and the
// preamble permits reading exactly the one staged file.
func TestStagedPromptKeepsContractAndReadPolicy(t *testing.T) {
	payload := []byte(`{"model":"agy/default","messages":[{"role":"user","content":"hi"}],
      "tools":[{"type":"function","function":{"name":"bash","description":"run"}}]}`)
	req, err := decodeChatRequest(payload)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := buildChatPromptFor(req, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(staged, bridgeReadPolicyStaged) {
		t.Fatal("staged prompt does not use the staged read policy")
	}
	if strings.Contains(staged, bridgeReadPolicy) {
		t.Fatal("staged prompt still carries the no-read policy")
	}
	toolIdx := strings.Index(staged, "[SYSTEM INSTRUCTION: CLIENT-SIDE TOOL CALLING]")
	msgIdx := strings.Index(staged, "--- message 1")
	if toolIdx < 0 || msgIdx < 0 || toolIdx > msgIdx {
		t.Fatalf("tool contract must lead the staged file (%d vs %d)", toolIdx, msgIdx)
	}
	if !strings.Contains(string(mustHandoff(t)), "Do not read any other file") {
		t.Fatal("handoff does not forbid reading other files")
	}
}

func mustHandoff(t *testing.T) string {
	t.Helper()
	return fileHandoffPrompt(filepath.Join(promptStagingDir(), "prompt_x.txt"), 1234)
}

// count_tokens must reflect the conversation, not the short handoff.
func TestPromptForTokenCountUsesFullPromptWhenStaged(t *testing.T) {
	setConfig(pluginConfig{BinaryPath: "agy", MaxPromptBytes: 200000})
	t.Cleanup(func() { setConfig(defaultConfig()) })
	req := stagedRequest(t, 220000)
	run, err := prepareAgyExecution(req)
	if err != nil {
		t.Fatal(err)
	}
	if run.cleanup != nil {
		defer run.cleanup()
	}
	full := promptForTokenCount(req, run)
	if len(full) <= len(run.prompt) {
		t.Fatalf("token count prompt = %d bytes, want the full conversation (%d)", len(full), len(run.prompt))
	}
	if estimateTokens(full) <= estimateTokens(run.prompt) {
		t.Fatal("estimated tokens were computed from the handoff")
	}
}

// A staged file must be removed when agy fails as well, not only on success.
func TestStagedPromptRemovedOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub is a shell script")
	}
	useTempStagingDir(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "agy")
	// Record that it was invoked, then fail: the staged file must still be removed.
	marker := filepath.Join(dir, "invoked")
	script := "#!/bin/sh\ncat >/dev/null 2>&1\ntouch " + marker + "\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	setConfig(pluginConfig{BinaryPath: bin, MaxPromptBytes: 4096})
	t.Cleanup(func() { setConfig(defaultConfig()) })

	req := stagedRequest(t, 8000)
	// Prove staging happens for this request before the turn runs.
	prepared, err := prepareAgyExecution(req)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.stagedBytes == 0 {
		t.Fatal("request was not staged, so this test would not exercise cleanup")
	}
	prepared.cleanup()

	if _, err := executeNonStream(req); err == nil {
		t.Fatal("expected the stubbed agy failure to surface")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("stub agy was not invoked: %v", err)
	}
	if entries, _ := os.ReadDir(promptStagingDir()); len(entries) != 0 {
		t.Fatalf("staged file leaked after a failed turn: %v", entries)
	}
}

// A staging root another local user could control must be refused.
func TestEnsurePrivateStagingDirRejectsLoosePermissions(t *testing.T) {
	previous := stagingDirOverride
	base := t.TempDir()
	dir := filepath.Join(base, "cliproxy-prompts")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	stagingDirOverride = dir
	t.Cleanup(func() { stagingDirOverride = previous })

	if _, err := ensurePrivateStagingDir(); err == nil {
		t.Fatal("expected a 0777 staging directory to be rejected")
	}
	if _, err := writePromptFile("payload"); err == nil {
		t.Fatal("expected staging into a world-writable directory to fail")
	}
}

// Each staged prompt lives in its own directory, so its name is not predictable.
func TestWritePromptFileIsolatesEachTurn(t *testing.T) {
	useTempStagingDir(t)
	first, err := writePromptFile("same payload")
	if err != nil {
		t.Fatal(err)
	}
	second, err := writePromptFile("same payload")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(first) == filepath.Dir(second) {
		t.Fatalf("two turns share a staging directory: %s", filepath.Dir(first))
	}
	if !strings.HasPrefix(filepath.Base(filepath.Dir(first)), "turn-") {
		t.Fatalf("staging directory is not per-turn: %s", filepath.Dir(first))
	}
	removeStagedPrompt(first)
	removeStagedPrompt(second)
	if entries, _ := os.ReadDir(promptStagingDir()); len(entries) != 0 {
		t.Fatalf("staging root not empty: %v", entries)
	}
}
