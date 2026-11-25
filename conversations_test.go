package main

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func setTestConversationConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("APPDATA", dir)
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

func TestConversationSchedulerOnlyOverridesNativeRequests(t *testing.T) {
	setTestConversationConfig(t)
	owner := sessionHome(t.TempDir())
	ref := conversationRef{NativeID: "native-session", Home: owner, Managed: true}
	pickReq := schedulerRequest{Providers: []string{"agy"}, Candidates: []schedulerCandidate{
		{ID: "second", Provider: "agy", Priority: 10, Attributes: map[string]string{sessionHomeAttr: sessionHome(t.TempDir())}},
		{ID: "original", Provider: "agy", Priority: 0, Attributes: map[string]string{sessionHomeAttr: owner}},
		{ID: "duplicate-home", Provider: "agy", Priority: -1, Attributes: map[string]string{sessionHomeAttr: owner}},
		{ID: "foreign", Provider: "openai", Priority: 20, Attributes: map[string]string{sessionHomeAttr: "foreign"}},
	}}

	if pick, err := pickConversationAccount(pickReq); err != nil || pick.Handled {
		t.Fatalf("new chats must be left to CLIProxyAPI: %+v, %v", pick, err)
	}
	// Verify the body-to-scheduler path, and ensure the owner beats higher priorities.
	pickReq.Options.Headers = conversationRoutingHeaders([]byte(`{"conversation_id":"` + ref.clientID() + `"}`))
	if pick, err := pickConversationAccount(pickReq); err != nil || pick.AuthID != "original" {
		t.Fatalf("resume selected %+v, error %v", pick, err)
	}
	pickReq.Candidates = pickReq.Candidates[:1]
	if _, err := pickConversationAccount(pickReq); asPluginError(err).HTTPStatus != http.StatusConflict {
		t.Fatalf("unavailable original should return a conflict: %v", err)
	}
	pickReq.Options.Headers.Set(handoffHeader, "true")
	if pick, err := pickConversationAccount(pickReq); err != nil || pick.AuthID != "second" {
		t.Fatalf("handoff selected %+v, error %v", pick, err)
	}
	pickReq.Candidates[0].Attributes[sessionHomeAttr] = owner
	if _, err := pickConversationAccount(pickReq); asPluginError(err).HTTPStatus != http.StatusConflict {
		t.Fatalf("same-home duplicate should not count as an alternate: %v", err)
	}
	pickReq.Providers = []string{"openai"}
	if pick, err := pickConversationAccount(pickReq); err != nil || pick.Handled {
		t.Fatalf("unrelated providers must not be affected: %+v, %v", pick, err)
	}
}

func TestPrepareExecutionPinsOrReplaysConversation(t *testing.T) {
	setTestConversationConfig(t)
	owner, alternate := t.TempDir(), t.TempDir()
	ref := conversationRef{NativeID: "native-session", Home: sessionHome(owner), Managed: true}
	id := ref.clientID()
	req := rpcExecutorRequest{
		Model:        "agy/default",
		AuthID:       "alice@example.invalid.json",
		AuthMetadata: map[string]any{"home": owner},
		Payload: []byte(`{"conversation_id":"` + id + `","messages":[
			{"role":"user","content":"Earlier question"},
			{"role":"assistant","content":"Earlier answer"},
			{"role":"user","content":"Next turn"}
		]}`),
	}
	run, err := prepareAgyExecution(req)
	if err != nil || run.opts.ConversationID != "native-session" || run.prompt != "Next turn" {
		t.Fatalf("resume should send just the new turn and native ID: %+v, %v", run, err)
	}
	req.AuthMetadata["home"] = alternate
	if _, err := prepareAgyExecution(req); asPluginError(err).HTTPStatus != http.StatusConflict {
		t.Fatalf("executor must enforce affinity even if the scheduler is bypassed: %v", err)
	}
	req.Headers = make(http.Header)
	req.Headers.Set(handoffHeader, "true")
	run, err = prepareAgyExecution(req)
	if err != nil || run.opts.ConversationID != "" || !strings.Contains(run.prompt, "Earlier question") || !strings.Contains(run.prompt, "Earlier answer") || !strings.Contains(run.prompt, "Next turn") {
		t.Fatalf("handoff should clear the native ID and keep the transcript: %+v, %v", run, err)
	}
	req.AuthMetadata["home"] = owner
	if _, err := prepareAgyExecution(req); asPluginError(err).HTTPStatus != http.StatusConflict {
		t.Fatalf("executor must not hand off to the original home: %v", err)
	}
	req.AuthMetadata["home"] = alternate
	req.Payload = []byte(`{"conversation_id":"` + id + `","messages":[{"role":"user","content":"Next turn"}]}`)
	if _, err := prepareAgyExecution(req); asPluginError(err).HTTPStatus != http.StatusBadRequest {
		t.Fatalf("handoff without past context must fail: %v", err)
	}
	req.Payload = []byte(`{"conversation_id":"` + id + `","agy_replay_summary":"Prior chat summary","messages":[{"role":"user","content":"Next turn"}]}`)
	run, err = prepareAgyExecution(req)
	if err != nil || run.opts.ConversationID != "" || !strings.Contains(run.prompt, "Prior chat summary") {
		t.Fatalf("handoff may replay a supplied summary: %+v, %v", run, err)
	}
}

func TestManagedSessionCannotResumeThroughDefaultHomeFallback(t *testing.T) {
	setTestConversationConfig(t)
	ref := conversationRef{NativeID: "native-session", Home: sessionHome(""), Managed: true}
	dest := conversationRef{Home: ref.Home}
	if _, _, err := resolveConversation(ref.clientID(), nil, dest); asPluginError(err).HTTPStatus != http.StatusConflict {
		t.Fatalf("disabled managed credentials must not be bypassed via direct routing: %v", err)
	}
	ref.Managed = false
	if id, _, err := resolveConversation(ref.clientID(), nil, dest); err != nil || id != ref.NativeID {
		t.Fatalf("normal default-HOME resumes must work: %q, %v", id, err)
	}
}

func TestInvalidConversationIDFailsClosed(t *testing.T) {
	setTestConversationConfig(t)
	ref := conversationRef{NativeID: "native-session", Home: sessionHome(filepath.Join(t.TempDir(), "private@example.invalid"))}
	id := ref.clientID()
	if !strings.HasPrefix(id, conversationPrefix) {
		t.Fatalf("expected signed ID, got %q", id)
	}
	replacement := byte('A')
	if id[len(conversationPrefix)] == replacement {
		replacement = 'B'
	}
	tampered := conversationPrefix + string(replacement) + id[len(conversationPrefix)+1:]
	if _, _, err := resolveConversation(tampered, nil, ref); asPluginError(err).HTTPStatus != http.StatusBadRequest {
		t.Fatalf("modified ID must fail closed: %v", err)
	}
}
