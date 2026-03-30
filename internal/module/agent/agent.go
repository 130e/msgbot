package agent

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const (
	TypePassthrough       = "passthrough"
	TypeAnthropic         = "anthropic"
	DefaultAnthropicModel = "claude-haiku-4-5"
	DefaultChatMaxTokens  = 2048
	requestTimeout        = 20 * time.Second
	mailMaxTokens         = 160
)

var mailPrompt = []string{
	"Rewrite the email body into a terse, readable Telegram chat message.",
	"Return plain text only.",
	"Do not use Markdown, HTML tags, or any other markup.",
	"Preserve concrete facts like login codes, verification links, dates, deadlines, and requested actions.",
	"Preserve any placeholder tokens like MSGURL001TOKEN exactly as written when they are relevant; never alter, escape, rename, or replace them with generic phrases.",
	"Omit greetings, signatures, and filler.",
	"Do not invent facts.",
}

var chatPrompt = []string{
	"You are replying as [bot] in a Telegram group chat.",
	"The user message contains a recent IRC-style transcript in chronological order.",
	"Each transcript line starts with a speaker prefix like [alice] or [bot].",
	"Write only the next [bot] reply to the latest conversation turn.",
	"Be helpful, direct, and conversational.",
	"Return plain text only.",
	"Do not include a speaker prefix, Markdown, HTML tags, or any other markup.",
	"Do not invent facts.",
}

type Config struct {
	Type      string          `toml:"type"`
	Anthropic AnthropicConfig `toml:"anthropic"`
}

type AnthropicConfig struct {
	APIKey string `toml:"api_key"`
	Model  string `toml:"model"`
}

type ChatRequest struct {
	Transcript string
	MaxTokens  int64
}

type Module struct {
	cfg      Config
	provider provider
}

type provider interface {
	SummarizeMail(context.Context, string) (string, error)
	ReplyChat(context.Context, ChatRequest) (string, error)
}

type passthroughProvider struct{}

type ClaudeAgent struct {
	client         anthropic.Client
	model          anthropic.Model
	requestTimeout time.Duration
}

func New(cfg Config) *Module {
	return &Module{cfg: cfg}
}

func (m *Module) Up(context.Context) error {
	kind := strings.TrimSpace(m.cfg.Type)
	if kind == "" {
		kind = TypePassthrough
	}
	log.Printf("Agent module starting type=%s", kind)

	provider, err := buildProvider(m.cfg)
	if err != nil {
		return err
	}

	m.provider = provider
	log.Printf("Agent module ready type=%s", kind)
	return nil
}

func (m *Module) Down(context.Context) error {
	log.Printf("Agent module stopping")
	m.provider = nil
	return nil
}

func (m *Module) SummarizeMail(ctx context.Context, body string) (string, error) {
	if m.provider == nil {
		return "", fmt.Errorf("agent module not started")
	}

	summary, err := m.provider.SummarizeMail(ctx, body)
	if err != nil {
		return "", fmt.Errorf("agent summarize mail: %w", err)
	}

	return summary, nil
}

func (m *Module) ReplyChat(ctx context.Context, request ChatRequest) (string, error) {
	if m.provider == nil {
		return "", fmt.Errorf("agent module not started")
	}

	reply, err := m.provider.ReplyChat(ctx, request)
	if err != nil {
		return "", fmt.Errorf("agent reply chat: %w", err)
	}

	return reply, nil
}

func buildProvider(cfg Config) (provider, error) {
	kind := strings.TrimSpace(cfg.Type)
	if kind == "" {
		kind = TypePassthrough
	}

	switch kind {
	case TypePassthrough:
		return passthroughProvider{}, nil
	case TypeAnthropic:
		apiKey := strings.TrimSpace(cfg.Anthropic.APIKey)
		if apiKey == "" {
			return nil, fmt.Errorf("modules.agent.anthropic.api_key is required when modules.agent.type=%q", TypeAnthropic)
		}

		model := strings.TrimSpace(cfg.Anthropic.Model)
		if model == "" {
			model = DefaultAnthropicModel
		}

		return newClaudeAgent(apiKey, model, nil), nil
	default:
		return nil, fmt.Errorf("unsupported modules.agent.type %q", kind)
	}
}

func newClaudeAgent(apiKey string, model string, client *http.Client) *ClaudeAgent {
	if strings.TrimSpace(model) == "" {
		model = DefaultAnthropicModel
	}

	clientOptions := []option.RequestOption{option.WithAPIKey(apiKey)}
	if client != nil {
		clientOptions = append(clientOptions, option.WithHTTPClient(client))
	}

	return &ClaudeAgent{
		client:         anthropic.NewClient(clientOptions...),
		model:          anthropic.Model(model),
		requestTimeout: requestTimeout,
	}
}

func (passthroughProvider) SummarizeMail(_ context.Context, body string) (string, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "", fmt.Errorf("empty body")
	}

	return body, nil
}

func (passthroughProvider) ReplyChat(_ context.Context, request ChatRequest) (string, error) {
	transcript := strings.TrimSpace(request.Transcript)
	if transcript == "" {
		return "", fmt.Errorf("empty transcript")
	}

	return transcript, nil
}

func (a *ClaudeAgent) SummarizeMail(ctx context.Context, body string) (string, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "", fmt.Errorf("empty body")
	}

	return a.generate(ctx, body, strings.Join(mailPrompt, " "), mailMaxTokens)
}

func (a *ClaudeAgent) ReplyChat(ctx context.Context, request ChatRequest) (string, error) {
	transcript := strings.TrimSpace(request.Transcript)
	if transcript == "" {
		return "", fmt.Errorf("empty transcript")
	}

	maxTokens := request.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultChatMaxTokens
	}

	return a.generate(ctx, transcript, strings.Join(chatPrompt, " "), maxTokens)
}

func (a *ClaudeAgent) generate(ctx context.Context, input string, prompt string, maxTokens int64) (string, error) {
	ctx, cancel := withOptionalTimeout(ctx, a.requestTimeout)
	defer cancel()

	params := anthropic.MessageNewParams{
		MaxTokens: maxTokens,
		Model:     a.model,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(input)),
		},
		System: []anthropic.TextBlockParam{
			anthropic.TextBlock{Text: prompt}.ToParam(),
		},
	}

	message, err := a.client.Messages.New(ctx, params, option.WithRequestTimeout(a.requestTimeout))
	if err != nil {
		return "", fmt.Errorf("agent create message: %w", err)
	}

	var parts []string
	for _, block := range message.Content {
		if block.Type != "text" {
			continue
		}

		text := strings.TrimSpace(block.Text)
		if text != "" {
			parts = append(parts, text)
		}
	}

	summary := strings.TrimSpace(strings.Join(parts, "\n"))
	if summary == "" {
		return "", fmt.Errorf("agent response did not include text content")
	}

	return summary, nil
}

func withOptionalTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}

	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= timeout {
		return ctx, func() {}
	}

	return context.WithTimeout(ctx, timeout)
}
