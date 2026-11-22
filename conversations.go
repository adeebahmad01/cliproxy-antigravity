package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	conversationPrefix = "agy.v1."
	conversationHeader = "X-AGY-Conversation-ID"
	handoffHeader      = "X-AGY-Continue-On-Another-Account"
	sessionHomeAttr    = "agy_session_home"
)

type conversationRef struct {
	NativeID string `json:"id"`
	Home     string `json:"home"`
	Managed  bool   `json:"managed,omitempty"`
}

// Use the actual session storage home rather than a filename, which may change or
// point to a home already registered under another name.
func sessionHome(home string) string {
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ""
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(home)))
}

// A signed, stateless ID survives plugin/host restarts without exposing emails or
// paths. Publish the signing key atomically so concurrent processes sharing the
// config directory can't mint IDs with two different keys.
func conversationKey() ([]byte, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, pluginID)
	path := filepath.Join(dir, "conversation.key")
	readKey := func() ([]byte, error) {
		key, err := os.ReadFile(path)
		if err == nil && len(key) != sha256.Size {
			return nil, errors.New("invalid conversation key length")
		}
		return key, err
	}
	if key, err := readKey(); !errors.Is(err, os.ErrNotExist) {
		return key, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, sha256.Size)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".conversation-key-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(key)
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return nil, err
	}
	if err := os.Link(tmp.Name(), path); errors.Is(err, os.ErrExist) {
		return readKey()
	} else if err != nil {
		return nil, err
	}
	return key, nil
}

func signConversation(key, body []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(conversationPrefix))
	mac.Write(body)
	return mac.Sum(nil)
}

func (ref conversationRef) clientID() string {
	if ref.NativeID == "" || ref.Home == "" {
		return ""
	}
	key, err := conversationKey()
	if err != nil {
		// Stateless clients still get their answer; never emit an unbound native ID.
		return ""
	}
	body, _ := json.Marshal(ref)
	return conversationPrefix + base64.RawURLEncoding.EncodeToString(append(body, signConversation(key, body)...))
}

func parseConversation(id string) (conversationRef, error) {
	var ref conversationRef
	invalid := pError(errorCodeRequestScoped, "Invalid Antigravity conversation ID. Use the ID from a successful proxy response, or omit it and send the chat history to start a new session.", http.StatusBadRequest)
	if !strings.HasPrefix(id, conversationPrefix) || len(id) > 4096 {
		return ref, invalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(id, conversationPrefix))
	if err != nil || len(raw) <= sha256.Size {
		return ref, invalid
	}
	key, err := conversationKey()
	if err != nil {
		return ref, pError(errorCodeRequestScoped, "Antigravity conversation routing is temporarily unavailable.", http.StatusServiceUnavailable)
	}
	body, sig := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	if !hmac.Equal(sig, signConversation(key, body)) || json.Unmarshal(body, &ref) != nil || ref.NativeID == "" || ref.Home == "" {
		return conversationRef{}, invalid
	}
	return ref, nil
}

func requestConversationID(ids conversationFields, headers http.Header, metadata map[string]any) string {
	for _, value := range []string{
		ids.ConversationID, ids.SessionID, headers.Get(conversationHeader),
		headers.Get("X-Conversation-ID"), headers.Get("Conversation-ID"),
	} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	for _, field := range []string{"conversation_id", "agy_conversation_id", "session_id"} {
		if value, _ := metadata[field].(string); strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

type conversationFields struct {
	ConversationID string `json:"conversation_id,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
}

func readConversationFields(body []byte) conversationFields {
	var ids conversationFields
	_ = json.Unmarshal(body, &ids)
	return ids
}

func parseHandoff(headers http.Header) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(headers.Get(handoffHeader))) {
	case "true":
		return true, nil
	case "", "false":
		return false, nil
	default:
		return false, pError(errorCodeRequestScoped, handoffHeader+" must be true or false.", http.StatusBadRequest)
	}
}

func ownerUnavailable() error {
	return pError(errorCodeRequestScoped, "The Antigravity account for this conversation is unavailable. Retry when it is available, or use "+handoffHeader+": true and send the chat history to continue on another account.", http.StatusConflict)
}

func alternateUnavailable() error {
	return pError(errorCodeRequestScoped, "No other enabled Antigravity account is available for this model.", http.StatusConflict)
}

func handoffNeedsID() error {
	return pError(errorCodeRequestScoped, "Continuing on another account requires a conversation ID returned by this proxy.", http.StatusBadRequest)
}

type schedulerRequest struct {
	Provider  string
	Providers []string
	Options   struct {
		Headers  http.Header
		Metadata map[string]any
	}
	Candidates []schedulerCandidate
}

type schedulerCandidate struct {
	ID         string
	Provider   string
	Priority   int
	Attributes map[string]string
}

type schedulerResponse struct {
	Handled bool
	AuthID  string `json:",omitempty"`
}

// CLIProxyAPI applies model support, disabled status and cooldowns first.
// Only native resumes and explicit handoffs override its strategy.
func pickConversationAccount(req schedulerRequest) (schedulerResponse, error) {
	if !containsFold(req.Providers, providerID) && !strings.EqualFold(req.Provider, providerID) {
		return schedulerResponse{}, nil
	}
	id := requestConversationID(conversationFields{}, req.Options.Headers, req.Options.Metadata)
	handoff, err := parseHandoff(req.Options.Headers)
	if err != nil {
		return schedulerResponse{}, err
	}
	if !strings.HasPrefix(id, conversationPrefix) {
		if handoff {
			return schedulerResponse{}, handoffNeedsID()
		}
		return schedulerResponse{}, nil
	}
	ref, err := parseConversation(id)
	if err != nil {
		return schedulerResponse{}, err
	}
	var best schedulerResponse
	var priority int
	for _, candidate := range req.Candidates {
		home := candidate.Attributes[sessionHomeAttr]
		if candidate.ID == "" || !strings.EqualFold(candidate.Provider, providerID) || home == "" {
			continue
		}
		sameHome := home == ref.Home
		if (sameHome && handoff) || (!sameHome && !handoff) {
			continue
		}
		if !best.Handled || candidate.Priority > priority || (candidate.Priority == priority && candidate.ID < best.AuthID) {
			best = schedulerResponse{Handled: true, AuthID: candidate.ID}
			priority = candidate.Priority
		}
	}
	if !best.Handled {
		if handoff {
			return best, alternateUnavailable()
		}
		return best, ownerUnavailable()
	}
	return best, nil
}

// The scheduler sees headers and metadata, not the JSON body.
func conversationRoutingHeaders(body []byte) http.Header {
	id := requestConversationID(readConversationFields(body), nil, nil)
	if strings.HasPrefix(id, conversationPrefix) {
		return http.Header{http.CanonicalHeaderKey(conversationHeader): []string{id}}
	}
	return nil
}

func resolveConversation(id string, headers http.Header, dest conversationRef) (nativeID string, handoff bool, err error) {
	handoff, err = parseHandoff(headers)
	if err != nil {
		return "", false, err
	}
	if strings.HasPrefix(id, conversationPrefix) {
		source, err := parseConversation(id)
		if err != nil {
			return "", false, err
		}
		if handoff {
			if !dest.Managed || dest.Home == "" || dest.Home == source.Home {
				return "", false, alternateUnavailable()
			}
			return "", true, nil
		}
		if dest.Home != source.Home || (source.Managed && !dest.Managed) {
			return "", false, ownerUnavailable()
		}
		return source.NativeID, false, nil
	}
	if handoff {
		return "", false, handoffNeedsID()
	}
	if id != "" && dest.Managed {
		return "", false, pError(errorCodeRequestScoped, "This older Antigravity conversation ID has no account binding. Omit the ID and send the chat history to start a new session.", http.StatusBadRequest)
	}
	return id, false, nil
}
