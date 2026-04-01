package agent

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const (
	TypePassthrough      = "passthrough"
	TypeClaude           = "claude"
	DefaultClaudeModel   = "claude-haiku-4-5"
	DefaultChatMaxTokens = 2048
	requestTimeout       = 20 * time.Second
	serverToolTimeout    = 60 * time.Second
	mailMaxTokens        = 160
	defaultWebSearchUses = 5
	maxChatSources       = 3
)

var mailPromptText = strings.Join([]string{
	"Rewrite the email body into a terse, readable Telegram chat message.",
	"Return plain text only.",
	"Do not use Markdown, HTML tags, or any other markup.",
	"Preserve concrete facts like login codes, verification links, dates, deadlines, and requested actions.",
	"Preserve any placeholder tokens like MSGURL001TOKEN exactly as written when they are relevant; never alter, escape, rename, or replace them with generic phrases.",
	"Omit greetings, signatures, and filler.",
	"Do not invent facts.",
}, " ")

var chatPromptText = strings.Join([]string{
	"You are replying as [bot] in a Telegram group chat.",
	"The user message contains a recent IRC-style transcript in chronological order.",
	"Each transcript line starts with a speaker prefix like [alice] or [bot].",
	"Write only the next [bot] reply to the latest conversation turn.",
	"Be helpful, direct, and conversational.",
	"Return plain text only.",
	"Do not include a speaker prefix, Markdown, HTML tags, or any other markup.",
	"Do not invent facts.",
}, " ")

type Config struct {
	Type   string       `toml:"type"`
	Claude ClaudeConfig `toml:"claude"`
}

type ClaudeConfig struct {
	APIKey string `toml:"api_key"`
	Model  string `toml:"model"`
}

type ChatRequest struct {
	Transcript       string
	MaxTokens        int64
	Model            string
	WebSearchEnabled bool
	WebSearchMaxUses int64
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

type sourceCitation struct {
	title string
	url   string
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
	case TypeClaude:
		apiKey := strings.TrimSpace(cfg.Claude.APIKey)
		if apiKey == "" {
			return nil, fmt.Errorf("modules.agent.claude.api_key is required when modules.agent.type=%q", TypeClaude)
		}

		model := strings.TrimSpace(cfg.Claude.Model)
		if model == "" {
			model = DefaultClaudeModel
		}

		return newClaudeAgent(apiKey, model, nil), nil
	default:
		return nil, fmt.Errorf("unsupported modules.agent.type %q", kind)
	}
}

func newClaudeAgent(apiKey string, model string, client *http.Client) *ClaudeAgent {
	if strings.TrimSpace(model) == "" {
		model = DefaultClaudeModel
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

	return a.generate(ctx, body, mailPromptText, a.model, mailMaxTokens, a.requestTimeout)
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

	model := a.model
	if strings.TrimSpace(request.Model) != "" {
		model = anthropic.Model(strings.TrimSpace(request.Model))
	}

	if !request.WebSearchEnabled {
		return a.generate(ctx, transcript, chatPromptText, model, maxTokens, a.requestTimeout)
	}

	maxUses := request.WebSearchMaxUses
	if maxUses <= 0 {
		maxUses = defaultWebSearchUses
	}

	timeout := a.requestTimeout
	if timeout < serverToolTimeout {
		timeout = serverToolTimeout
	}

	return a.generateChatWithWebSearch(ctx, transcript, model, maxTokens, maxUses, timeout)
}

func (a *ClaudeAgent) generate(ctx context.Context, input string, prompt string, model anthropic.Model, maxTokens int64, timeout time.Duration) (string, error) {
	ctx, cancel := withOptionalTimeout(ctx, timeout)
	defer cancel()

	params := anthropic.MessageNewParams{
		MaxTokens: maxTokens,
		Model:     model,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(input)),
		},
		System: []anthropic.TextBlockParam{
			anthropic.TextBlock{Text: prompt}.ToParam(),
		},
	}

	message, err := a.client.Messages.New(ctx, params, option.WithRequestTimeout(timeout))
	if err != nil {
		return "", fmt.Errorf("agent create message: %w", err)
	}

	summary := joinTextBlocks(message.Content)
	if summary == "" {
		return "", fmt.Errorf("agent response did not include text content")
	}

	return summary, nil
}

func (a *ClaudeAgent) generateChatWithWebSearch(ctx context.Context, transcript string, model anthropic.Model, maxTokens int64, maxUses int64, timeout time.Duration) (string, error) {
	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock(transcript)),
	}
	var combinedContent []anthropic.ContentBlockUnion
	maxTurns := maxUses + 1

	for turn := int64(0); turn < maxTurns; turn++ {
		params := anthropic.MessageNewParams{
			MaxTokens: maxTokens,
			Model:     model,
			Messages:  append([]anthropic.MessageParam(nil), messages...),
			System: []anthropic.TextBlockParam{
				anthropic.TextBlock{Text: chatPromptText}.ToParam(),
			},
			ToolChoice: anthropic.ToolChoiceUnionParam{
				OfAuto: &anthropic.ToolChoiceAutoParam{},
			},
			Tools: []anthropic.ToolUnionParam{{
				OfWebSearchTool20260209: &anthropic.WebSearchTool20260209Param{
					MaxUses: anthropic.Int(maxUses),
				},
			}},
		}

		requestCtx, cancel := withOptionalTimeout(ctx, timeout)
		message, err := a.client.Messages.New(requestCtx, params, option.WithRequestTimeout(timeout))
		cancel()
		if err != nil {
			return "", fmt.Errorf("agent create message: %w", err)
		}

		combinedContent = append(combinedContent, message.Content...)
		if message.StopReason != anthropic.StopReasonPauseTurn {
			reply, searchErrors, err := buildChatReply(combinedContent)
			if len(searchErrors) > 0 {
				log.Printf(
					"Claude chat web search produced tool errors model=%s errors=%s",
					model,
					strings.Join(searchErrors, ","),
				)
			}
			return reply, err
		}

		raw := strings.TrimSpace(message.RawJSON())
		if raw == "" {
			return "", fmt.Errorf("agent pause_turn replay requires message raw JSON")
		}
		var assistantMessage anthropic.MessageParam
		if err := assistantMessage.UnmarshalJSON([]byte(raw)); err != nil {
			return "", fmt.Errorf("agent decode pause_turn replay message: %w", err)
		}
		messages = append(messages, assistantMessage)
	}

	return "", fmt.Errorf("agent chat web search exceeded %d turns for %d web search uses", maxTurns, maxUses)
}

func buildChatReply(content []anthropic.ContentBlockUnion) (string, []string, error) {
	reply := joinTextBlocks(content)
	searchErrors := collectWebSearchErrors(content)
	if reply == "" {
		if len(searchErrors) > 0 {
			return "", searchErrors, fmt.Errorf("agent response did not include text content after web search error: %s", strings.Join(searchErrors, ", "))
		}
		return "", nil, fmt.Errorf("agent response did not include text content")
	}

	sources := collectWebSearchSources(content)
	if len(sources) > 0 {
		reply = appendSourcesFooter(reply, sources)
	}

	return reply, searchErrors, nil
}

func joinTextBlocks(content []anthropic.ContentBlockUnion) string {
	var parts []string
	for _, block := range content {
		if block.Type != "text" {
			continue
		}

		text := strings.TrimSpace(block.Text)
		if text != "" {
			parts = append(parts, text)
		}
	}

	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func collectWebSearchErrors(content []anthropic.ContentBlockUnion) []string {
	var codes []string
	seen := make(map[string]struct{})

	for _, block := range content {
		if block.Type != "web_search_tool_result" {
			continue
		}
		code := strings.TrimSpace(string(block.Content.ErrorCode))
		if code == "" {
			continue
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		codes = append(codes, code)
	}

	return codes
}

func collectWebSearchSources(content []anthropic.ContentBlockUnion) []sourceCitation {
	var sources []sourceCitation
	seen := make(map[string]struct{})

	for _, block := range content {
		if block.Type != "text" {
			continue
		}

		for _, citation := range block.Citations {
			if citation.Type != "web_search_result_location" {
				continue
			}

			source := citation.AsWebSearchResultLocation()
			url := strings.TrimSpace(source.URL)
			if url == "" {
				continue
			}
			if _, ok := seen[url]; ok {
				continue
			}

			seen[url] = struct{}{}
			sources = append(sources, sourceCitation{
				title: strings.TrimSpace(source.Title),
				url:   url,
			})
			if len(sources) == maxChatSources {
				return sources
			}
		}
	}

	return sources
}

func appendSourcesFooter(reply string, sources []sourceCitation) string {
	reply = strings.TrimSpace(reply)
	if reply == "" || len(sources) == 0 {
		return reply
	}

	var builder strings.Builder
	builder.WriteString(reply)
	builder.WriteString("\n\nSources:")
	for _, source := range sources {
		builder.WriteByte('\n')
		title := strings.TrimSpace(source.title)
		switch {
		case title == "", title == source.url:
			builder.WriteString(source.url)
		default:
			builder.WriteString(title)
			builder.WriteString(" - ")
			builder.WriteString(source.url)
		}
	}

	return builder.String()
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
