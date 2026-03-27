package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClaudeAgentSummarizeSendsExpectedRequest(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("request path = %q, want %q", r.URL.Path, "/v1/messages")
		}
		if got := r.Header.Get("x-api-key"); got != "anthropic-key" {
			t.Fatalf("x-api-key = %q, want %q", got, "anthropic-key")
		}
		if got := r.Header.Get("anthropic-version"); got == "" {
			t.Fatal("anthropic-version header is empty")
		}

		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		if err := json.Unmarshal(payload, &requestBody); err != nil {
			t.Fatalf("Unmarshal() error = %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"Your login code is 123456."}]}`))
	}))
	defer server.Close()

	agent := newClaudeAgent("anthropic-key", "claude-sonnet-4-20250514", server.Client())
	agent.baseURL = server.URL

	summary, err := agent.Summarize(context.Background(), "Here is your login code: 123456")
	if err != nil {
		t.Fatalf("Summarize() error = %v", err)
	}
	if summary != "Your login code is 123456." {
		t.Fatalf("Summarize() = %q, want %q", summary, "Your login code is 123456.")
	}
	if got, _ := requestBody["model"].(string); got != "claude-sonnet-4-20250514" {
		t.Fatalf("request model = %v, want %q", requestBody["model"], "claude-sonnet-4-20250514")
	}

	systemBlocks, ok := requestBody["system"].([]any)
	if !ok || len(systemBlocks) != 1 {
		t.Fatalf("request system = %#v, want single system block", requestBody["system"])
	}
	systemBlock, ok := systemBlocks[0].(map[string]any)
	if !ok || !strings.Contains(systemBlock["text"].(string), "terse, readable Telegram chat message") {
		t.Fatalf("request system block = %#v, want system prompt text", systemBlocks[0])
	}
	if !strings.Contains(systemBlock["text"].(string), "Return plain text only.") {
		t.Fatalf("request system block = %#v, want plain text instructions", systemBlocks[0])
	}
	if !strings.Contains(systemBlock["text"].(string), "Do not use Markdown, HTML tags") {
		t.Fatalf("request system block = %#v, want no-markup instructions", systemBlocks[0])
	}
	if strings.Contains(systemBlock["text"].(string), "simple Telegram Markdown") {
		t.Fatalf("request system block = %#v, should not mention Markdown formatting instructions", systemBlocks[0])
	}
	if !strings.Contains(systemBlock["text"].(string), "MSGURL001TOKEN") {
		t.Fatalf("request system block = %#v, want placeholder preservation instructions", systemBlocks[0])
	}

	messages, ok := requestBody["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("request messages = %#v, want single message", requestBody["messages"])
	}
	message, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatalf("request message = %#v, want object", messages[0])
	}
	contentBlocks, ok := message["content"].([]any)
	if !ok || len(contentBlocks) != 1 {
		t.Fatalf("request content = %#v, want single text block", message["content"])
	}
	contentBlock, ok := contentBlocks[0].(map[string]any)
	if !ok || contentBlock["text"] != "Here is your login code: 123456" {
		t.Fatalf("request content block = %#v, want email body text", contentBlocks[0])
	}
}

func TestClaudeAgentSummarizeReturnsErrorForHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	agent := newClaudeAgent("anthropic-key", "claude-sonnet-4-20250514", server.Client())
	agent.baseURL = server.URL

	_, err := agent.Summarize(context.Background(), "body")
	if err == nil {
		t.Fatal("Summarize() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "401 Unauthorized") {
		t.Fatalf("Summarize() error = %v, want 401 Unauthorized", err)
	}
}

func TestClaudeAgentSummarizeReturnsErrorWhenResponseHasNoText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[{"type":"tool_use"}]}`))
	}))
	defer server.Close()

	agent := newClaudeAgent("anthropic-key", "claude-sonnet-4-20250514", server.Client())
	agent.baseURL = server.URL

	_, err := agent.Summarize(context.Background(), "body")
	if err == nil {
		t.Fatal("Summarize() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "did not include text content") {
		t.Fatalf("Summarize() error = %v, want missing text content error", err)
	}
}
