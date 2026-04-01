package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

type sequentialTransport struct {
	t         *testing.T
	responses []string
	requests  [][]byte
}

func (s *sequentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.t.Helper()

	body, err := io.ReadAll(req.Body)
	if err != nil {
		s.t.Fatalf("ReadAll(req.Body) error = %v", err)
	}
	s.requests = append(s.requests, body)

	if len(s.responses) == 0 {
		s.t.Fatal("unexpected claude request")
	}

	response := s.responses[0]
	s.responses = s.responses[1:]
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(response)),
	}, nil
}

type delayedSequentialTransport struct {
	t         *testing.T
	delays    []time.Duration
	responses []string
	requests  [][]byte
}

func (s *delayedSequentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.t.Helper()

	body, err := io.ReadAll(req.Body)
	if err != nil {
		s.t.Fatalf("ReadAll(req.Body) error = %v", err)
	}
	s.requests = append(s.requests, body)

	if len(s.responses) == 0 {
		s.t.Fatal("unexpected claude request")
	}

	if len(s.delays) > 0 {
		delay := s.delays[0]
		s.delays = s.delays[1:]

		timer := time.NewTimer(delay)
		defer timer.Stop()

		select {
		case <-timer.C:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}

	response := s.responses[0]
	s.responses = s.responses[1:]
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(response)),
	}, nil
}

func TestClaudeAgentReplyChatUsesWebSearchAndFormatsSources(t *testing.T) {
	transport := &sequentialTransport{
		t: t,
		responses: []string{
			claudeMessageResponse("claude-haiku-4-5", "end_turn", []map[string]any{
				{
					"type": "text",
					"text": "Fresh answer",
					"citations": []map[string]any{
						webSearchCitation("Result One", "https://example.com/1", "enc-1", "Fact 1"),
						webSearchCitation("Result One", "https://example.com/1", "enc-1b", "Fact 1 duplicate"),
						webSearchCitation("Result Two", "https://example.com/2", "enc-2", "Fact 2"),
						webSearchCitation("Result Three", "https://example.com/3", "enc-3", "Fact 3"),
						webSearchCitation("Result Four", "https://example.com/4", "enc-4", "Fact 4"),
					},
				},
			}),
		},
	}

	client := &http.Client{Transport: transport}
	agent := newClaudeAgent("test-api-key", DefaultClaudeModel, client)

	reply, err := agent.ReplyChat(context.Background(), ChatRequest{
		Transcript:       "[@alice] what changed today?",
		MaxTokens:        256,
		Model:            "claude-haiku-4-5",
		WebSearchEnabled: true,
		WebSearchMaxUses: 7,
	})
	if err != nil {
		t.Fatalf("ReplyChat() error = %v", err)
	}

	if !strings.Contains(reply, "Fresh answer") {
		t.Fatalf("reply = %q, want answer text", reply)
	}
	if !strings.Contains(reply, "Sources:\nResult One - https://example.com/1\nResult Two - https://example.com/2\nResult Three - https://example.com/3") {
		t.Fatalf("reply = %q, want sources footer", reply)
	}
	if strings.Contains(reply, "https://example.com/4") {
		t.Fatalf("reply = %q, want sources capped at 3 unique URLs", reply)
	}

	var payload map[string]any
	if err := json.Unmarshal(transport.requests[0], &payload); err != nil {
		t.Fatalf("json.Unmarshal(request) error = %v", err)
	}

	if got := payload["model"]; got != "claude-haiku-4-5" {
		t.Fatalf("request model = %#v, want %#v", got, "claude-haiku-4-5")
	}

	toolChoice, ok := payload["tool_choice"].(map[string]any)
	if !ok || toolChoice["type"] != "auto" {
		t.Fatalf("tool_choice = %#v, want auto", payload["tool_choice"])
	}

	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v, want single web search tool", payload["tools"])
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tool entry = %#v, want object", tools[0])
	}
	if got := tool["type"]; got != "web_search_20260209" {
		t.Fatalf("tool type = %#v, want %#v", got, "web_search_20260209")
	}
	if got := tool["max_uses"]; got != float64(7) {
		t.Fatalf("tool max_uses = %#v, want %#v", got, float64(7))
	}

}

func TestClaudeAgentReplyChatReplaysPauseTurnRawJSON(t *testing.T) {
	transport := &sequentialTransport{
		t: t,
		responses: []string{
			claudeMessageResponse("claude-sonnet-4-6", "pause_turn", []map[string]any{
				{
					"type":   "server_tool_use",
					"id":     "srv_1",
					"name":   "web_search",
					"input":  map[string]any{"query": "latest test"},
					"caller": map[string]any{"type": "direct"},
				},
				{
					"type":        "web_search_tool_result",
					"tool_use_id": "srv_1",
					"caller":      map[string]any{"type": "direct"},
					"content": []map[string]any{
						{
							"type":              "web_search_result",
							"title":             "Replay Doc",
							"url":               "https://example.com/replay",
							"encrypted_content": "enc-content",
							"page_age":          "1 day",
						},
					},
				},
				{
					"type": "text",
					"text": "Partial answer",
					"citations": []map[string]any{
						webSearchCitation("Replay Doc", "https://example.com/replay", "enc-index", "Partial answer"),
					},
				},
			}),
			claudeMessageResponse("claude-sonnet-4-6", "end_turn", []map[string]any{
				{
					"type": "text",
					"text": "Final answer",
				},
			}),
		},
	}

	client := &http.Client{Transport: transport}
	agent := newClaudeAgent("test-api-key", DefaultClaudeModel, client)

	reply, err := agent.ReplyChat(context.Background(), ChatRequest{
		Transcript:       "[@alice] latest test?",
		MaxTokens:        256,
		Model:            "claude-sonnet-4-6",
		WebSearchEnabled: true,
		WebSearchMaxUses: 3,
	})
	if err != nil {
		t.Fatalf("ReplyChat() error = %v", err)
	}

	if !strings.Contains(reply, "Partial answer\nFinal answer") {
		t.Fatalf("reply = %q, want accumulated text across pause_turn", reply)
	}
	if len(transport.requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(transport.requests))
	}

	var secondPayload map[string]any
	if err := json.Unmarshal(transport.requests[1], &secondPayload); err != nil {
		t.Fatalf("json.Unmarshal(second request) error = %v", err)
	}

	messages, ok := secondPayload["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("messages = %#v, want user + assistant replay", secondPayload["messages"])
	}
	assistant, ok := messages[1].(map[string]any)
	if !ok {
		t.Fatalf("assistant replay = %#v, want object", messages[1])
	}

	content, ok := assistant["content"].([]any)
	if !ok || len(content) != 3 {
		t.Fatalf("assistant replay content = %#v, want 3 blocks", assistant["content"])
	}

	serverToolUse, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("server_tool_use block = %#v, want object", content[0])
	}
	if caller := serverToolUse["caller"].(map[string]any)["type"]; caller != "direct" {
		t.Fatalf("server_tool_use caller = %#v, want direct", caller)
	}

	webSearchResult, ok := content[1].(map[string]any)
	if !ok {
		t.Fatalf("web_search_tool_result block = %#v, want object", content[1])
	}
	if caller := webSearchResult["caller"].(map[string]any)["type"]; caller != "direct" {
		t.Fatalf("web_search_tool_result caller = %#v, want direct", caller)
	}
	resultList, ok := webSearchResult["content"].([]any)
	if !ok || len(resultList) != 1 {
		t.Fatalf("web_search_tool_result content = %#v, want single result", webSearchResult["content"])
	}
	if encrypted := resultList[0].(map[string]any)["encrypted_content"]; encrypted != "enc-content" {
		t.Fatalf("encrypted_content = %#v, want %#v", encrypted, "enc-content")
	}

	textBlock, ok := content[2].(map[string]any)
	if !ok {
		t.Fatalf("text block = %#v, want object", content[2])
	}
	citations, ok := textBlock["citations"].([]any)
	if !ok || len(citations) != 1 {
		t.Fatalf("citations = %#v, want single replay citation", textBlock["citations"])
	}
	citation, ok := citations[0].(map[string]any)
	if !ok {
		t.Fatalf("citation = %#v, want object", citations[0])
	}
	if got := citation["encrypted_index"]; got != "enc-index" {
		t.Fatalf("citation encrypted_index = %#v, want %#v", got, "enc-index")
	}
	if got := citation["url"]; got != "https://example.com/replay" {
		t.Fatalf("citation url = %#v, want %#v", got, "https://example.com/replay")
	}
}

func TestClaudeAgentGenerateChatWithWebSearchRefreshesTimeoutEachTurn(t *testing.T) {
	transport := &delayedSequentialTransport{
		t:      t,
		delays: []time.Duration{30 * time.Millisecond, 30 * time.Millisecond},
		responses: []string{
			claudeMessageResponse("claude-sonnet-4-6", "pause_turn", []map[string]any{
				{
					"type": "text",
					"text": "Partial answer",
				},
			}),
			claudeMessageResponse("claude-sonnet-4-6", "end_turn", []map[string]any{
				{
					"type": "text",
					"text": "Final answer",
				},
			}),
		},
	}

	client := &http.Client{Transport: transport}
	agent := newClaudeAgent("test-api-key", DefaultClaudeModel, client)

	reply, err := agent.generateChatWithWebSearch(
		context.Background(),
		"[@alice] latest test?",
		anthropic.Model("claude-sonnet-4-6"),
		256,
		1,
		40*time.Millisecond,
	)
	if err != nil {
		t.Fatalf("generateChatWithWebSearch() error = %v", err)
	}

	if reply != "Partial answer\nFinal answer" {
		t.Fatalf("reply = %q, want accumulated text after refreshed timeouts", reply)
	}
	if len(transport.requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(transport.requests))
	}
}

func TestClaudeAgentReplyChatAllowsFinalTurnAfterMaxWebSearchUses(t *testing.T) {
	const maxUses = 5

	responses := make([]string, 0, maxUses+1)
	for i := 1; i <= maxUses; i++ {
		responses = append(responses, claudeMessageResponse("claude-sonnet-4-6", "pause_turn", []map[string]any{
			{
				"type": "text",
				"text": fmt.Sprintf("Search step %d", i),
			},
		}))
	}
	responses = append(responses, claudeMessageResponse("claude-sonnet-4-6", "end_turn", []map[string]any{
		{
			"type": "text",
			"text": "Final answer",
		},
	}))

	transport := &sequentialTransport{
		t:         t,
		responses: responses,
	}

	client := &http.Client{Transport: transport}
	agent := newClaudeAgent("test-api-key", DefaultClaudeModel, client)

	reply, err := agent.ReplyChat(context.Background(), ChatRequest{
		Transcript:       "[@alice] latest test?",
		MaxTokens:        256,
		Model:            "claude-sonnet-4-6",
		WebSearchEnabled: true,
		WebSearchMaxUses: maxUses,
	})
	if err != nil {
		t.Fatalf("ReplyChat() error = %v", err)
	}

	if !strings.Contains(reply, "Search step 5\nFinal answer") {
		t.Fatalf("reply = %q, want final reply after %d pause turns", reply, maxUses)
	}
	if len(transport.requests) != maxUses+1 {
		t.Fatalf("request count = %d, want %d", len(transport.requests), maxUses+1)
	}
}

func TestClaudeAgentReplyChatHandlesWebSearchToolErrors(t *testing.T) {
	t.Run("returns text when available", func(t *testing.T) {
		transport := &sequentialTransport{
			t: t,
			responses: []string{
				claudeMessageResponse("claude-sonnet-4-6", "end_turn", []map[string]any{
					{
						"type":        "web_search_tool_result",
						"tool_use_id": "srv_1",
						"caller":      map[string]any{"type": "direct"},
						"content": map[string]any{
							"type":       "web_search_tool_result_error",
							"error_code": "unavailable",
						},
					},
					{
						"type": "text",
						"text": "Fallback answer",
					},
				}),
			},
		}

		client := &http.Client{Transport: transport}
		agent := newClaudeAgent("test-api-key", DefaultClaudeModel, client)

		reply, err := agent.ReplyChat(context.Background(), ChatRequest{
			Transcript:       "[@alice] test",
			MaxTokens:        128,
			Model:            "claude-sonnet-4-6",
			WebSearchEnabled: true,
		})
		if err != nil {
			t.Fatalf("ReplyChat() error = %v, want nil when text is present", err)
		}
		if reply != "Fallback answer" {
			t.Fatalf("reply = %q, want %q", reply, "Fallback answer")
		}
	})

	t.Run("fails when there is no text", func(t *testing.T) {
		transport := &sequentialTransport{
			t: t,
			responses: []string{
				claudeMessageResponse("claude-sonnet-4-6", "end_turn", []map[string]any{
					{
						"type":        "web_search_tool_result",
						"tool_use_id": "srv_1",
						"caller":      map[string]any{"type": "direct"},
						"content": map[string]any{
							"type":       "web_search_tool_result_error",
							"error_code": "unavailable",
						},
					},
				}),
			},
		}

		client := &http.Client{Transport: transport}
		agent := newClaudeAgent("test-api-key", DefaultClaudeModel, client)

		_, err := agent.ReplyChat(context.Background(), ChatRequest{
			Transcript:       "[@alice] test",
			MaxTokens:        128,
			Model:            "claude-sonnet-4-6",
			WebSearchEnabled: true,
		})
		if err == nil {
			t.Fatal("ReplyChat() error = nil, want tool-error failure when no text is present")
		}
		if !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("ReplyChat() error = %v, want unavailable error code", err)
		}
	})
}

func claudeMessageResponse(model string, stopReason string, content []map[string]any) string {
	return mustJSON(map[string]any{
		"id":    "msg_test",
		"type":  "message",
		"role":  "assistant",
		"model": model,
		"container": map[string]any{
			"id":         "container_test",
			"expires_at": time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
		},
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": "",
		"usage": map[string]any{
			"cache_creation": map[string]any{
				"ephemeral_1h_input_tokens": 0,
				"ephemeral_5m_input_tokens": 0,
			},
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     0,
			"inference_geo":               "",
			"input_tokens":                1,
			"output_tokens":               1,
			"server_tool_use": map[string]any{
				"web_fetch_requests":  0,
				"web_search_requests": 0,
			},
			"service_tier": "standard",
		},
	})
}

func webSearchCitation(title string, url string, encryptedIndex string, citedText string) map[string]any {
	return map[string]any{
		"type":            "web_search_result_location",
		"title":           title,
		"url":             url,
		"encrypted_index": encryptedIndex,
		"cited_text":      citedText,
	}
}

func mustJSON(v any) string {
	body, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(body)
}
