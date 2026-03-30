package chat

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/130e/msgbot/internal/module/agent"
	telegrammodule "github.com/130e/msgbot/internal/module/telegram"
	"github.com/go-telegram/bot/models"
)

const (
	agentFailureReply        = "Sorry, I couldn't generate a reply right now."
	defaultContextMaxMessage = 24
	defaultMaxReplyMessages  = 2
	defaultContextWindow     = 12 * time.Hour
	botSpeaker               = "bot"
)

type Config struct {
	Enabled            bool   `toml:"enabled"`
	ContextMaxMessages int    `toml:"context_max_messages"`
	ContextWindow      string `toml:"context_window"`
	MaxReplyMessages   int    `toml:"max_reply_messages"`
	AgentMaxTokens     int64  `toml:"agent_max_tokens"`
}

type resolvedConfig struct {
	contextMaxMessages int
	contextWindow      time.Duration
	maxReplyMessages   int
	agentMaxTokens     int64
}

type chatAgent interface {
	ReplyChat(context.Context, agent.ChatRequest) (string, error)
}

type replySender interface {
	Reply(context.Context, telegrammodule.ReplyTarget, string) error
	SendToThread(context.Context, telegrammodule.ThreadTarget, string) error
	LogFailure(error, string)
}

type task struct {
	cfg      resolvedConfig
	agent    chatAgent
	telegram replySender

	mu      sync.Mutex
	history map[int64][]historyEntry
}

type historyEntry struct {
	sentAt  time.Time
	speaker string
	text    string
	isBot   bool
}

type byteRange struct {
	start int
	end   int
}

func New(cfg Config, agentModule *agent.Module, telegramModule *telegrammodule.Module) (telegrammodule.UpdateHandler, error) {
	resolved, err := cfg.resolve()
	if err != nil {
		return nil, err
	}

	t := &task{
		cfg:      resolved,
		agent:    agentModule,
		telegram: telegramModule,
		history:  make(map[int64][]historyEntry),
	}

	return t.Handle, nil
}

func (c Config) resolve() (resolvedConfig, error) {
	contextMaxMessages := c.ContextMaxMessages
	if contextMaxMessages == 0 {
		contextMaxMessages = defaultContextMaxMessage
	}
	if contextMaxMessages < 0 {
		return resolvedConfig{}, fmt.Errorf("tasks.chat.context_max_messages must be greater than 0")
	}
	if contextMaxMessages == 0 {
		return resolvedConfig{}, fmt.Errorf("tasks.chat.context_max_messages must be greater than 0")
	}

	contextWindow := defaultContextWindow
	if strings.TrimSpace(c.ContextWindow) != "" {
		duration, err := time.ParseDuration(strings.TrimSpace(c.ContextWindow))
		if err != nil {
			return resolvedConfig{}, fmt.Errorf("invalid tasks.chat.context_window %q: %w", c.ContextWindow, err)
		}
		if duration <= 0 {
			return resolvedConfig{}, fmt.Errorf("tasks.chat.context_window must be greater than 0")
		}
		contextWindow = duration
	}

	maxReplyMessages := c.MaxReplyMessages
	if maxReplyMessages == 0 {
		maxReplyMessages = defaultMaxReplyMessages
	}
	if maxReplyMessages < 0 {
		return resolvedConfig{}, fmt.Errorf("tasks.chat.max_reply_messages must be greater than 0")
	}
	if maxReplyMessages == 0 {
		return resolvedConfig{}, fmt.Errorf("tasks.chat.max_reply_messages must be greater than 0")
	}

	agentMaxTokens := c.AgentMaxTokens
	if agentMaxTokens == 0 {
		agentMaxTokens = agent.DefaultChatMaxTokens
	}
	if agentMaxTokens < 0 {
		return resolvedConfig{}, fmt.Errorf("tasks.chat.agent_max_tokens must be greater than 0")
	}
	if agentMaxTokens == 0 {
		return resolvedConfig{}, fmt.Errorf("tasks.chat.agent_max_tokens must be greater than 0")
	}

	return resolvedConfig{
		contextMaxMessages: contextMaxMessages,
		contextWindow:      contextWindow,
		maxReplyMessages:   maxReplyMessages,
		agentMaxTokens:     agentMaxTokens,
	}, nil
}

func (t *task) Handle(ctx context.Context, update telegrammodule.Update) error {
	message := update.Message
	if message == nil {
		log.Printf("Chat task ignored update: no message payload")
		return nil
	}
	if message.ChatType != models.ChatTypeGroup && message.ChatType != models.ChatTypeSupergroup {
		log.Printf("Chat task ignored message %s: unsupported chat_type=%s", messageSummary(message), message.ChatType)
		return nil
	}
	if message.FromIsBot {
		log.Printf("Chat task ignored message %s: sender is bot", messageSummary(message))
		return nil
	}
	if strings.TrimSpace(message.Text) == "" {
		log.Printf("Chat task ignored message %s: empty text", messageSummary(message))
		return nil
	}

	t.recordHumanMessage(message)
	if !hasMatchingMention(message) {
		log.Printf("Chat task ignored message %s: no matching bot mention", messageSummary(message))
		return nil
	}

	transcript, contextMessages := t.transcript(message.ChatID, messageTime(message))
	if transcript == "" {
		log.Printf("Chat task ignored message %s: empty transcript", messageSummary(message))
		return nil
	}

	log.Printf(
		"Chat task dispatching agent call %s context_messages=%d transcript_len=%d",
		messageSummary(message),
		contextMessages,
		len(transcript),
	)

	target := telegrammodule.ReplyTarget{
		ChatID:          message.ChatID,
		MessageID:       message.MessageID,
		MessageThreadID: message.MessageThreadID,
	}

	reply, err := t.agent.ReplyChat(ctx, agent.ChatRequest{
		Transcript: transcript,
		MaxTokens:  t.cfg.agentMaxTokens,
	})
	if err != nil {
		log.Printf("Failed to generate chat reply %s: %v", messageSummary(message), err)
		return t.sendReply(ctx, target, agentFailureReply, true, message)
	}

	return t.sendReply(ctx, target, reply, false, message)
}

func (t *task) sendReply(ctx context.Context, target telegrammodule.ReplyTarget, reply string, isFallback bool, message *telegrammodule.IncomingMessage) error {
	chunks := telegrammodule.SplitText(reply, t.cfg.maxReplyMessages)
	if len(chunks) == 0 {
		return fmt.Errorf("empty chat reply")
	}

	threadTarget := telegrammodule.ThreadTarget{
		ChatID:          target.ChatID,
		MessageThreadID: target.MessageThreadID,
	}

	for index, chunk := range chunks {
		outgoing := telegrammodule.RenderHTML(chunk, nil)

		var err error
		if index == 0 {
			err = t.telegram.Reply(ctx, target, outgoing)
		} else {
			err = t.telegram.SendToThread(ctx, threadTarget, outgoing)
		}
		if err != nil {
			t.telegram.LogFailure(err, messageSummary(message))
			return err
		}
	}

	if isFallback {
		log.Printf("Sent fallback chat reply %s chunks=%d", messageSummary(message), len(chunks))
		return nil
	}

	t.recordBotReply(message.ChatID, messageTime(message), chunks)
	log.Printf("Processed chat message %s reply_chunks=%d", messageSummary(message), len(chunks))
	return nil
}

func (t *task) recordHumanMessage(message *telegrammodule.IncomingMessage) {
	t.appendHistory(message.ChatID, historyEntry{
		sentAt:  messageTime(message),
		speaker: speakerName(message),
		text:    message.Text,
	})
}

func (t *task) recordBotReply(chatID int64, sentAt time.Time, chunks []string) {
	for _, chunk := range chunks {
		t.appendHistory(chatID, historyEntry{
			sentAt:  sentAt,
			speaker: botSpeaker,
			text:    chunk,
			isBot:   true,
		})
	}
}

func (t *task) appendHistory(chatID int64, entry historyEntry) {
	reference := normalizeTime(entry.sentAt)

	t.mu.Lock()
	defer t.mu.Unlock()

	entries := t.pruneEntriesLocked(chatID, reference)
	entries = append(entries, entry)
	if len(entries) > t.cfg.contextMaxMessages {
		entries = entries[len(entries)-t.cfg.contextMaxMessages:]
	}
	t.history[chatID] = entries
}

func (t *task) transcript(chatID int64, reference time.Time) (string, int) {
	reference = normalizeTime(reference)

	t.mu.Lock()
	entries := append([]historyEntry(nil), t.pruneEntriesLocked(chatID, reference)...)
	t.mu.Unlock()

	if len(entries) == 0 {
		return "", 0
	}

	var builder strings.Builder
	for index, entry := range entries {
		if index > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString("[")
		if entry.isBot {
			builder.WriteString(botSpeaker)
		} else {
			builder.WriteString(entry.speaker)
		}
		builder.WriteString("] ")
		builder.WriteString(compactLine(entry.text))
	}

	return builder.String(), len(entries)
}

func (t *task) pruneEntriesLocked(chatID int64, reference time.Time) []historyEntry {
	entries := t.history[chatID]
	if len(entries) == 0 {
		return nil
	}

	cutoff := reference.Add(-t.cfg.contextWindow)
	kept := entries[:0]
	for _, entry := range entries {
		if entry.sentAt.Before(cutoff) {
			continue
		}
		kept = append(kept, entry)
	}
	if len(kept) > t.cfg.contextMaxMessages {
		kept = kept[len(kept)-t.cfg.contextMaxMessages:]
	}

	if len(kept) == 0 {
		delete(t.history, chatID)
		return nil
	}

	t.history[chatID] = kept
	return kept
}

func hasMatchingMention(message *telegrammodule.IncomingMessage) bool {
	return len(matchingMentionRanges(message)) > 0
}

func speakerName(message *telegrammodule.IncomingMessage) string {
	name := compactLine(message.FromDisplayName)
	if name != "" {
		return name
	}
	if message.FromUserID != 0 {
		return fmt.Sprintf("user:%d", message.FromUserID)
	}
	return "user"
}

func compactLine(text string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
}

func normalizeTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}

func messageTime(message *telegrammodule.IncomingMessage) time.Time {
	if message == nil {
		return time.Now().UTC()
	}
	return normalizeTime(message.SentAt)
}

func matchingMentionRanges(message *telegrammodule.IncomingMessage) []byteRange {
	if message == nil {
		return nil
	}

	var ranges []byteRange
	expectedMention := ""
	if strings.TrimSpace(message.BotUsername) != "" {
		expectedMention = "@" + strings.TrimSpace(message.BotUsername)
	}

	for _, entity := range message.Entities {
		start, end, ok := entityByteRange(message.Text, entity)
		if !ok || start >= end {
			continue
		}

		switch entity.Type {
		case models.MessageEntityTypeMention:
			if expectedMention == "" {
				continue
			}
			if strings.EqualFold(message.Text[start:end], expectedMention) {
				ranges = append(ranges, byteRange{start: start, end: end})
			}
		case models.MessageEntityTypeTextMention:
			if entity.User != nil && entity.User.ID == message.BotUserID {
				ranges = append(ranges, byteRange{start: start, end: end})
			}
		}
	}

	if len(ranges) == 0 {
		return nil
	}

	sort.Slice(ranges, func(i int, j int) bool {
		if ranges[i].start == ranges[j].start {
			return ranges[i].end < ranges[j].end
		}
		return ranges[i].start < ranges[j].start
	})

	merged := ranges[:0]
	for _, current := range ranges {
		if len(merged) == 0 || current.start > merged[len(merged)-1].end {
			merged = append(merged, current)
			continue
		}
		if current.end > merged[len(merged)-1].end {
			merged[len(merged)-1].end = current.end
		}
	}

	return merged
}

func entityByteRange(text string, entity models.MessageEntity) (int, int, bool) {
	if entity.Offset < 0 || entity.Length <= 0 {
		return 0, 0, false
	}

	boundaries := utf16Boundaries(text)
	endOffset := entity.Offset + entity.Length
	if entity.Offset >= len(boundaries) || endOffset >= len(boundaries) {
		return 0, 0, false
	}

	return boundaries[entity.Offset], boundaries[endOffset], true
}

func utf16Boundaries(text string) []int {
	boundaries := []int{0}
	for index, r := range text {
		next := index + utf8.RuneLen(r)
		for range utf16.Encode([]rune{r}) {
			boundaries = append(boundaries, next)
		}
	}

	return boundaries
}

func messageSummary(message *telegrammodule.IncomingMessage) string {
	return fmt.Sprintf(
		`chat_id=%d message_id=%d from_user_id=%d from=%q`,
		message.ChatID,
		message.MessageID,
		message.FromUserID,
		message.FromDisplayName,
	)
}
