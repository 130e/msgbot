package chat

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/130e/msgbot/internal/module/agent"
	telegrammodule "github.com/130e/msgbot/internal/module/telegram"
	"github.com/go-telegram/bot/models"
)

type fakeAgent struct {
	replies  []string
	err      error
	requests []agent.ChatRequest
}

func (f *fakeAgent) ReplyChat(_ context.Context, request agent.ChatRequest) (string, error) {
	f.requests = append(f.requests, request)
	if f.err != nil {
		return "", f.err
	}
	if len(f.replies) == 0 {
		return "", nil
	}

	reply := f.replies[0]
	f.replies = f.replies[1:]
	return reply, nil
}

type sentReply struct {
	target  telegrammodule.ReplyTarget
	message string
}

type sentThreadMessage struct {
	target  telegrammodule.ThreadTarget
	message string
}

type fakeTelegram struct {
	replyErr       error
	threadErr      error
	replies        []sentReply
	threadMessages []sentThreadMessage
	logFailures    []error
	logSummaries   []string
}

func (f *fakeTelegram) Reply(_ context.Context, target telegrammodule.ReplyTarget, message string) error {
	f.replies = append(f.replies, sentReply{
		target:  target,
		message: message,
	})
	return f.replyErr
}

func (f *fakeTelegram) SendToThread(_ context.Context, target telegrammodule.ThreadTarget, message string) error {
	f.threadMessages = append(f.threadMessages, sentThreadMessage{
		target:  target,
		message: message,
	})
	return f.threadErr
}

func (f *fakeTelegram) LogFailure(err error, summary string) {
	f.logFailures = append(f.logFailures, err)
	f.logSummaries = append(f.logSummaries, summary)
}

func TestHasMatchingMention(t *testing.T) {
	cases := []struct {
		name    string
		message *telegrammodule.IncomingMessage
		want    bool
	}{
		{
			name: "mention at start",
			message: &telegrammodule.IncomingMessage{
				Text:        "@mybot hello",
				Entities:    []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 0, Length: 6}},
				BotUsername: "mybot",
			},
			want: true,
		},
		{
			name: "mention at end",
			message: &telegrammodule.IncomingMessage{
				Text:        "hello @mybot",
				Entities:    []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 6, Length: 6}},
				BotUsername: "mybot",
			},
			want: true,
		},
		{
			name: "multiple mentions",
			message: &telegrammodule.IncomingMessage{
				Text:        "@mybot hello @mybot again",
				Entities:    []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 0, Length: 6}, {Type: models.MessageEntityTypeMention, Offset: 13, Length: 6}},
				BotUsername: "mybot",
			},
			want: true,
		},
		{
			name: "unicode before mention",
			message: &telegrammodule.IncomingMessage{
				Text:        "😀 @mybot hi",
				Entities:    []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 3, Length: 6}},
				BotUsername: "mybot",
			},
			want: true,
		},
		{
			name: "text mention for bot user",
			message: &telegrammodule.IncomingMessage{
				Text:      "hey Bot hi",
				Entities:  []models.MessageEntity{{Type: models.MessageEntityTypeTextMention, Offset: 4, Length: 3, User: &models.User{ID: 42}}},
				BotUserID: 42,
			},
			want: true,
		},
		{
			name: "mention only still triggers",
			message: &telegrammodule.IncomingMessage{
				Text:        "@mybot",
				Entities:    []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 0, Length: 6}},
				BotUsername: "mybot",
			},
			want: true,
		},
		{
			name: "other bot mention ignored",
			message: &telegrammodule.IncomingMessage{
				Text:        "@otherbot hello",
				Entities:    []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 0, Length: 9}},
				BotUsername: "mybot",
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hasMatchingMention(tc.message)
			if got != tc.want {
				t.Fatalf("hasMatchingMention() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolveDefaultsAndValidation(t *testing.T) {
	resolved, err := (Config{}).resolve()
	if err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	if resolved.contextMaxMessages != 24 {
		t.Fatalf("contextMaxMessages = %d, want 24", resolved.contextMaxMessages)
	}
	if resolved.contextWindow != 12*time.Hour {
		t.Fatalf("contextWindow = %v, want %v", resolved.contextWindow, 12*time.Hour)
	}
	if resolved.maxReplyMessages != 2 {
		t.Fatalf("maxReplyMessages = %d, want 2", resolved.maxReplyMessages)
	}
	if resolved.agentMaxTokens != agent.DefaultChatMaxTokens {
		t.Fatalf("agentMaxTokens = %d, want %d", resolved.agentMaxTokens, agent.DefaultChatMaxTokens)
	}
	if resolved.agentModel != "" {
		t.Fatalf("agentModel = %q, want empty string", resolved.agentModel)
	}
	if resolved.webSearchEnabled {
		t.Fatal("webSearchEnabled = true, want false by default")
	}
	if resolved.webSearchMaxUses != defaultWebSearchMaxUses {
		t.Fatalf("webSearchMaxUses = %d, want %d", resolved.webSearchMaxUses, defaultWebSearchMaxUses)
	}

	cases := []Config{
		{ContextMaxMessages: -1},
		{ContextWindow: "oops"},
		{ContextWindow: "0s"},
		{MaxReplyMessages: -1},
		{AgentMaxTokens: -1},
		{WebSearchMaxUses: -1},
	}
	for _, cfg := range cases {
		if _, err := cfg.resolve(); err == nil {
			t.Fatalf("resolve(%+v) error = nil, want validation error", cfg)
		}
	}
}

func TestHandleUsesConversationTranscriptAndStoresBotReplies(t *testing.T) {
	agent := &fakeAgent{replies: []string{"Sure.", "You're welcome."}}
	telegram := &fakeTelegram{}
	handler := &task{
		cfg: resolvedConfig{
			contextMaxMessages: 10,
			contextWindow:      12 * time.Hour,
			maxReplyMessages:   2,
			agentMaxTokens:     321,
			agentModel:         "claude-sonnet-4-6",
			webSearchEnabled:   true,
			webSearchMaxUses:   6,
		},
		agent:    agent,
		telegram: telegram,
		history:  make(map[int64][]historyEntry),
	}

	base := time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC)
	inputs := []*telegrammodule.IncomingMessage{
		{
			ChatID:          1001,
			ChatType:        models.ChatTypeSupergroup,
			MessageID:       1,
			MessageThreadID: 11,
			SentAt:          base,
			FromUserID:      1,
			FromDisplayName: "@alice",
			Text:            "hello everyone",
			BotUsername:     "mybot",
		},
		{
			ChatID:          1001,
			ChatType:        models.ChatTypeSupergroup,
			MessageID:       2,
			MessageThreadID: 22,
			SentAt:          base.Add(time.Minute),
			FromUserID:      2,
			FromDisplayName: "@bob",
			Text:            "I have a question",
			BotUsername:     "mybot",
		},
		{
			ChatID:          1001,
			ChatType:        models.ChatTypeSupergroup,
			MessageID:       3,
			MessageThreadID: 11,
			SentAt:          base.Add(2 * time.Minute),
			FromUserID:      3,
			FromDisplayName: "@carol",
			Text:            "@mybot can you help?",
			Entities:        []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 0, Length: 6}},
			BotUsername:     "mybot",
		},
		{
			ChatID:          1001,
			ChatType:        models.ChatTypeSupergroup,
			MessageID:       4,
			MessageThreadID: 11,
			SentAt:          base.Add(3 * time.Minute),
			FromUserID:      2,
			FromDisplayName: "@bob",
			Text:            "thanks @mybot",
			Entities:        []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 7, Length: 6}},
			BotUsername:     "mybot",
		},
	}

	for _, message := range inputs {
		if err := handler.Handle(context.Background(), telegrammodule.Update{Message: message}); err != nil {
			t.Fatalf("Handle(%d) error = %v", message.MessageID, err)
		}
	}

	if len(agent.requests) != 2 {
		t.Fatalf("agent requests = %d, want 2", len(agent.requests))
	}

	wantFirstTranscript := strings.Join([]string{
		"[@alice] hello everyone",
		"[@bob] I have a question",
		"[@carol] @mybot can you help?",
	}, "\n")
	if agent.requests[0].Transcript != wantFirstTranscript {
		t.Fatalf("first transcript = %q, want %q", agent.requests[0].Transcript, wantFirstTranscript)
	}
	if agent.requests[0].MaxTokens != 321 {
		t.Fatalf("first max tokens = %d, want 321", agent.requests[0].MaxTokens)
	}
	if agent.requests[0].Model != "claude-sonnet-4-6" {
		t.Fatalf("first model = %q, want %q", agent.requests[0].Model, "claude-sonnet-4-6")
	}
	if !agent.requests[0].WebSearchEnabled {
		t.Fatal("first webSearchEnabled = false, want true")
	}
	if agent.requests[0].WebSearchMaxUses != 6 {
		t.Fatalf("first webSearchMaxUses = %d, want 6", agent.requests[0].WebSearchMaxUses)
	}

	wantSecondTranscript := strings.Join([]string{
		"[@alice] hello everyone",
		"[@bob] I have a question",
		"[@carol] @mybot can you help?",
		"[bot] Sure.",
		"[@bob] thanks @mybot",
	}, "\n")
	if agent.requests[1].Transcript != wantSecondTranscript {
		t.Fatalf("second transcript = %q, want %q", agent.requests[1].Transcript, wantSecondTranscript)
	}

	if len(telegram.replies) != 2 {
		t.Fatalf("telegram replies = %d, want 2", len(telegram.replies))
	}
	if len(telegram.threadMessages) != 0 {
		t.Fatalf("thread messages = %d, want 0", len(telegram.threadMessages))
	}
	if telegram.replies[0].target.MessageThreadID != 11 || telegram.replies[1].target.MessageThreadID != 11 {
		t.Fatalf("reply thread targets = %#v, want thread 11", telegram.replies)
	}
}

func TestHandleUsesBareMentionAndPrunesOldMessages(t *testing.T) {
	agent := &fakeAgent{replies: []string{"reply"}}
	telegram := &fakeTelegram{}
	handler := &task{
		cfg: resolvedConfig{
			contextMaxMessages: 24,
			contextWindow:      12 * time.Hour,
			maxReplyMessages:   2,
			agentMaxTokens:     100,
		},
		agent:    agent,
		telegram: telegram,
		history:  make(map[int64][]historyEntry),
	}

	now := time.Date(2026, 3, 29, 15, 0, 0, 0, time.UTC)
	oldMessage := &telegrammodule.IncomingMessage{
		ChatID:          1001,
		ChatType:        models.ChatTypeGroup,
		MessageID:       1,
		SentAt:          now.Add(-13 * time.Hour),
		FromUserID:      1,
		FromDisplayName: "@alice",
		Text:            "too old",
		BotUsername:     "mybot",
	}
	recentMessage := &telegrammodule.IncomingMessage{
		ChatID:          1001,
		ChatType:        models.ChatTypeGroup,
		MessageID:       2,
		SentAt:          now.Add(-time.Minute),
		FromUserID:      2,
		FromDisplayName: "@bob",
		Text:            "still relevant",
		BotUsername:     "mybot",
	}
	trigger := &telegrammodule.IncomingMessage{
		ChatID:          1001,
		ChatType:        models.ChatTypeGroup,
		MessageID:       3,
		SentAt:          now,
		FromUserID:      3,
		FromDisplayName: "@carol",
		Text:            "@mybot",
		Entities:        []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 0, Length: 6}},
		BotUsername:     "mybot",
	}

	for _, message := range []*telegrammodule.IncomingMessage{oldMessage, recentMessage, trigger} {
		if err := handler.Handle(context.Background(), telegrammodule.Update{Message: message}); err != nil {
			t.Fatalf("Handle(%d) error = %v", message.MessageID, err)
		}
	}

	if len(agent.requests) != 1 {
		t.Fatalf("agent requests = %d, want 1", len(agent.requests))
	}

	wantTranscript := strings.Join([]string{
		"[@bob] still relevant",
		"[@carol] @mybot",
	}, "\n")
	if agent.requests[0].Transcript != wantTranscript {
		t.Fatalf("transcript = %q, want %q", agent.requests[0].Transcript, wantTranscript)
	}
}

func TestHandleDoesNotRecordFallbackReplyInHistory(t *testing.T) {
	agent := &fakeAgent{err: errors.New("boom")}
	telegram := &fakeTelegram{}
	handler := &task{
		cfg: resolvedConfig{
			contextMaxMessages: 24,
			contextWindow:      12 * time.Hour,
			maxReplyMessages:   2,
			agentMaxTokens:     100,
		},
		agent:    agent,
		telegram: telegram,
		history:  make(map[int64][]historyEntry),
	}

	base := time.Date(2026, 3, 29, 18, 0, 0, 0, time.UTC)
	first := &telegrammodule.IncomingMessage{
		ChatID:          1001,
		ChatType:        models.ChatTypeGroup,
		MessageID:       1,
		SentAt:          base,
		FromUserID:      1,
		FromDisplayName: "@alice",
		Text:            "@mybot help",
		Entities:        []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 0, Length: 6}},
		BotUsername:     "mybot",
	}
	if err := handler.Handle(context.Background(), telegrammodule.Update{Message: first}); err != nil {
		t.Fatalf("Handle(first) error = %v", err)
	}

	agent.err = nil
	agent.replies = []string{"all set"}
	second := &telegrammodule.IncomingMessage{
		ChatID:          1001,
		ChatType:        models.ChatTypeGroup,
		MessageID:       2,
		SentAt:          base.Add(time.Minute),
		FromUserID:      2,
		FromDisplayName: "@bob",
		Text:            "@mybot status?",
		Entities:        []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 0, Length: 6}},
		BotUsername:     "mybot",
	}
	if err := handler.Handle(context.Background(), telegrammodule.Update{Message: second}); err != nil {
		t.Fatalf("Handle(second) error = %v", err)
	}

	if len(agent.requests) != 2 {
		t.Fatalf("agent requests = %d, want 2", len(agent.requests))
	}
	if strings.Contains(agent.requests[1].Transcript, agentFailureReply) {
		t.Fatalf("second transcript = %q, should not include fallback reply", agent.requests[1].Transcript)
	}
}

func TestHandleSplitsLongRepliesAcrossThreadMessages(t *testing.T) {
	reply := strings.Repeat("chunk ", 900)
	agent := &fakeAgent{replies: []string{reply}}
	telegram := &fakeTelegram{}
	handler := &task{
		cfg: resolvedConfig{
			contextMaxMessages: 24,
			contextWindow:      12 * time.Hour,
			maxReplyMessages:   2,
			agentMaxTokens:     2048,
		},
		agent:    agent,
		telegram: telegram,
		history:  make(map[int64][]historyEntry),
	}

	err := handler.Handle(context.Background(), telegrammodule.Update{
		Message: &telegrammodule.IncomingMessage{
			ChatID:          1001,
			ChatType:        models.ChatTypeSupergroup,
			MessageID:       77,
			MessageThreadID: 12,
			SentAt:          time.Date(2026, 3, 29, 19, 0, 0, 0, time.UTC),
			FromUserID:      5,
			FromDisplayName: "@alice",
			Text:            "@mybot summarize",
			Entities:        []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 0, Length: 6}},
			BotUsername:     "mybot",
		},
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	chunks := telegrammodule.SplitText(reply, 2)
	if len(chunks) != 2 {
		t.Fatalf("SplitText() chunks = %d, want 2", len(chunks))
	}
	if len(telegram.replies) != 1 {
		t.Fatalf("telegram replies = %d, want 1", len(telegram.replies))
	}
	if len(telegram.threadMessages) != 1 {
		t.Fatalf("thread messages = %d, want 1", len(telegram.threadMessages))
	}

	if telegram.replies[0].target.ChatID != 1001 || telegram.replies[0].target.MessageID != 77 || telegram.replies[0].target.MessageThreadID != 12 {
		t.Fatalf("reply target = %#v, want chat/thread/message preserved", telegram.replies[0].target)
	}
	if telegram.threadMessages[0].target.ChatID != 1001 || telegram.threadMessages[0].target.MessageThreadID != 12 {
		t.Fatalf("thread target = %#v, want chat/thread preserved", telegram.threadMessages[0].target)
	}

	if telegram.replies[0].message != telegrammodule.RenderHTML(chunks[0], nil) {
		t.Fatalf("first chunk = %q, want %q", telegram.replies[0].message, telegrammodule.RenderHTML(chunks[0], nil))
	}
	if telegram.threadMessages[0].message != telegrammodule.RenderHTML(chunks[1], nil) {
		t.Fatalf("second chunk = %q, want %q", telegram.threadMessages[0].message, telegrammodule.RenderHTML(chunks[1], nil))
	}
}

func TestHandleIgnoresUnsupportedMessages(t *testing.T) {
	agent := &fakeAgent{replies: []string{"answer"}}
	telegram := &fakeTelegram{}
	handler := &task{
		cfg: resolvedConfig{
			contextMaxMessages: 24,
			contextWindow:      12 * time.Hour,
			maxReplyMessages:   2,
			agentMaxTokens:     100,
		},
		agent:    agent,
		telegram: telegram,
		history:  make(map[int64][]historyEntry),
	}

	cases := []*telegrammodule.IncomingMessage{
		{
			ChatID:      1001,
			ChatType:    models.ChatTypePrivate,
			MessageID:   1,
			Text:        "@mybot hi",
			Entities:    []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 0, Length: 6}},
			BotUsername: "mybot",
		},
		{
			ChatID:      1001,
			ChatType:    models.ChatTypeGroup,
			MessageID:   2,
			FromIsBot:   true,
			Text:        "@mybot hi",
			Entities:    []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 0, Length: 6}},
			BotUsername: "mybot",
		},
		{
			ChatID:      1001,
			ChatType:    models.ChatTypeGroup,
			MessageID:   3,
			Text:        "   ",
			BotUsername: "mybot",
		},
	}

	for _, message := range cases {
		if err := handler.Handle(context.Background(), telegrammodule.Update{Message: message}); err != nil {
			t.Fatalf("Handle(%d) error = %v", message.MessageID, err)
		}
	}

	if len(agent.requests) != 0 {
		t.Fatalf("agent requests = %d, want 0", len(agent.requests))
	}
	if len(telegram.replies) != 0 || len(telegram.threadMessages) != 0 {
		t.Fatalf("telegram messages = %d replies, %d thread messages, want none", len(telegram.replies), len(telegram.threadMessages))
	}
}

func TestHandleReturnsErrorWhenReplyDeliveryFails(t *testing.T) {
	agent := &fakeAgent{replies: []string{"answer"}}
	telegram := &fakeTelegram{replyErr: errors.New("send failed")}
	handler := &task{
		cfg: resolvedConfig{
			contextMaxMessages: 24,
			contextWindow:      12 * time.Hour,
			maxReplyMessages:   2,
			agentMaxTokens:     100,
		},
		agent:    agent,
		telegram: telegram,
		history:  make(map[int64][]historyEntry),
	}

	err := handler.Handle(context.Background(), telegrammodule.Update{
		Message: &telegrammodule.IncomingMessage{
			ChatID:      1001,
			ChatType:    models.ChatTypeGroup,
			MessageID:   77,
			FromUserID:  5,
			Text:        "@mybot help",
			Entities:    []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 0, Length: 6}},
			BotUsername: "mybot",
		},
	})
	if err == nil {
		t.Fatal("Handle() error = nil, want send error")
	}
	if len(telegram.logFailures) != 1 {
		t.Fatalf("log failures = %d, want 1", len(telegram.logFailures))
	}
}
