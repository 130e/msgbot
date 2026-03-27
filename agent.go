package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const defaultAnthropicModel = "claude-haiku-4-5"

var mailPrompt = []string{"Rewrite the email body into a terse, readable Telegram chat message.",
	"Return plain text only.",
	"Do not use Markdown, HTML tags, or any other markup.",
	"Preserve concrete facts like login codes, verification links, dates, deadlines, and requested actions.",
	"Preserve any placeholder tokens like MSGURL001TOKEN exactly as written when they are relevant; never alter, escape, rename, or replace them with generic phrases.",
	"Omit greetings, signatures, and filler.",
	"Do not invent facts."}

var defaultAnthropicHTTPClient = &http.Client{Timeout: 20 * time.Second}

type passthroughSummarizer struct{}

type ClaudeAgent struct {
	client       anthropic.Client
	model        anthropic.Model
	baseURL      string
	maxTokens    int64
	systemPrompt string
}

func newClaudeAgent(apiKey string, model string, client *http.Client) *ClaudeAgent {
	if client == nil {
		client = defaultAnthropicHTTPClient
	}
	if strings.TrimSpace(model) == "" {
		model = defaultAnthropicModel
	}

	sdkClient := anthropic.NewClient(
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(client),
	)

	return &ClaudeAgent{
		client:       sdkClient,
		model:        anthropic.Model(model),
		maxTokens:    160,
		systemPrompt: strings.Join(mailPrompt[:], " "),
	}
}

func (passthroughSummarizer) Summarize(_ context.Context, body string) (string, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "", fmt.Errorf("empty body")
	}

	return body, nil
}

func (a *ClaudeAgent) Summarize(ctx context.Context, body string) (string, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "", fmt.Errorf("empty body")
	}

	params := anthropic.MessageNewParams{
		MaxTokens: a.maxTokens,
		Model:     a.model,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(body)),
		},
		System: []anthropic.TextBlockParam{
			{Text: a.systemPrompt},
		},
	}

	var opts []option.RequestOption
	if a.baseURL != "" {
		opts = append(opts, option.WithBaseURL(a.baseURL))
	}

	message, err := a.client.Messages.New(ctx, params, opts...)
	if err != nil {
		return "", fmt.Errorf("agent create message: %w", err)
	}

	var parts []string
	for _, block := range message.Content {
		if block.Type == "text" {
			text := strings.TrimSpace(block.Text)
			if text != "" {
				parts = append(parts, text)
			}
		}
	}

	summary := strings.TrimSpace(strings.Join(parts, "\n"))
	if summary == "" {
		return "", fmt.Errorf("agent response did not include text content")
	}

	return summary, nil
}
