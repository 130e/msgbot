package telegram

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	neturl "net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	telegrambot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const (
	MessageLimit     = 4096
	TruncationNotice = "\n\n[message truncated]"
	sendTimeout      = 10 * time.Second
)

var bodyURLPattern = regexp.MustCompile(`(?:https?://|www\.|[A-Za-z0-9.-]+\.[A-Za-z]{2,}/)\S+`)

type Config struct {
	BotToken string `toml:"bot_token"`
	ChatID   string `toml:"chat_id"`
}

type Update struct {
	Message *IncomingMessage
}

type IncomingMessage struct {
	ChatID          int64
	ChatType        models.ChatType
	MessageID       int
	MessageThreadID int
	SentAt          time.Time
	FromUserID      int64
	FromDisplayName string
	FromIsBot       bool
	Text            string
	Entities        []models.MessageEntity
	BotUserID       int64
	BotUsername     string
}

type ReplyTarget struct {
	ChatID          int64
	MessageID       int
	MessageThreadID int
}

type ThreadTarget struct {
	ChatID          int64
	MessageThreadID int
}

type UpdateHandler func(context.Context, Update) error

type Module struct {
	cfg                 Config
	client              *telegrambot.Bot
	requestTimeout      time.Duration
	errCh               chan error
	defaultHandler      UpdateHandler
	requireConfiguredID bool
	runtimeCancel       context.CancelFunc
	botUserID           int64
	botUsername         string
}

func New(cfg Config) *Module {
	return &Module{
		cfg:            cfg,
		requestTimeout: sendTimeout,
		errCh:          make(chan error, 1),
	}
}

func (m *Module) RequireConfiguredChat() {
	m.requireConfiguredID = true
}

func (m *Module) SetDefaultHandler(handler UpdateHandler) {
	m.defaultHandler = handler
}

func (m *Module) Errors() <-chan error {
	return m.errCh
}

func (m *Module) Up(ctx context.Context) error {
	token := strings.TrimSpace(m.cfg.BotToken)
	if token == "" {
		return fmt.Errorf("modules.telegram.bot_token is required")
	}

	chatID := strings.TrimSpace(m.cfg.ChatID)
	if m.requireConfiguredID && chatID == "" {
		return fmt.Errorf("modules.telegram.chat_id is required when forwardmail is enabled")
	}

	log.Printf(
		"Telegram module starting inbound_enabled=%t configured_chat=%t",
		m.defaultHandler != nil,
		chatID != "",
	)

	options := []telegrambot.Option{
		telegrambot.WithErrorsHandler(func(err error) {
			m.publishError(fmt.Errorf("telegram runtime error: %w", err))
		}),
	}
	if m.defaultHandler != nil {
		options = append(options, telegrambot.WithDefaultHandler(m.handleDefaultUpdate))
	}

	client, err := newBot(token, nil, options...)
	if err != nil {
		return err
	}

	identityCtx, cancel := withOptionalTimeout(ctx, m.requestTimeout)
	me, err := client.GetMe(identityCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("get telegram bot identity: %w", err)
	}

	m.cfg.BotToken = token
	m.cfg.ChatID = chatID
	m.client = client
	m.botUserID = me.ID
	m.botUsername = strings.TrimSpace(me.Username)

	log.Printf("Telegram module ready bot_id=%d bot_username=%q", m.botUserID, m.botUsername)

	if m.defaultHandler != nil {
		webhookCtx, webhookCancel := withOptionalTimeout(ctx, m.requestTimeout)
		webhookInfo, err := m.client.GetWebhookInfo(webhookCtx)
		webhookCancel()
		if err != nil {
			return fmt.Errorf("get telegram webhook info: %w", err)
		}
		if strings.TrimSpace(webhookInfo.URL) != "" {
			log.Printf(
				"Telegram module found active webhook url=%q pending_updates=%d, deleting before long polling",
				webhookInfo.URL,
				webhookInfo.PendingUpdateCount,
			)
			deleteCtx, deleteCancel := withOptionalTimeout(ctx, m.requestTimeout)
			_, err = m.client.DeleteWebhook(deleteCtx, &telegrambot.DeleteWebhookParams{})
			deleteCancel()
			if err != nil {
				return fmt.Errorf("delete telegram webhook before polling: %w", err)
			}
			log.Printf("Telegram module deleted active webhook and will use long polling")
		}

		runtimeCtx, runtimeCancel := context.WithCancel(ctx)
		m.runtimeCancel = runtimeCancel
		log.Printf("Telegram module starting long polling")
		go m.client.Start(runtimeCtx)
	} else {
		log.Printf("Telegram module running in outbound-only mode")
	}

	return nil
}

func (m *Module) Down(context.Context) error {
	log.Printf("Telegram module stopping")
	if m.runtimeCancel != nil {
		m.runtimeCancel()
		m.runtimeCancel = nil
	}

	m.client = nil
	m.botUserID = 0
	m.botUsername = ""
	return nil
}

func (m *Module) SendMessage(ctx context.Context, message string) error {
	if m.client == nil {
		return fmt.Errorf("telegram module not started")
	}

	if strings.TrimSpace(m.cfg.ChatID) == "" {
		return fmt.Errorf("telegram configured chat target is not set")
	}

	return m.send(ctx, &telegrambot.SendMessageParams{
		ChatID:    m.cfg.ChatID,
		Text:      message,
		ParseMode: models.ParseModeHTML,
	})
}

func (m *Module) Reply(ctx context.Context, target ReplyTarget, message string) error {
	if m.client == nil {
		return fmt.Errorf("telegram module not started")
	}
	if target.ChatID == 0 {
		return fmt.Errorf("telegram reply target chat_id is required")
	}
	if target.MessageID == 0 {
		return fmt.Errorf("telegram reply target message_id is required")
	}

	params := &telegrambot.SendMessageParams{
		ChatID:    target.ChatID,
		Text:      message,
		ParseMode: models.ParseModeHTML,
		ReplyParameters: &models.ReplyParameters{
			MessageID:                target.MessageID,
			ChatID:                   target.ChatID,
			AllowSendingWithoutReply: true,
		},
	}
	if target.MessageThreadID > 0 {
		params.MessageThreadID = target.MessageThreadID
	}

	return m.send(ctx, params)
}

func (m *Module) SendToThread(ctx context.Context, target ThreadTarget, message string) error {
	if m.client == nil {
		return fmt.Errorf("telegram module not started")
	}
	if target.ChatID == 0 {
		return fmt.Errorf("telegram thread target chat_id is required")
	}

	params := &telegrambot.SendMessageParams{
		ChatID:    target.ChatID,
		Text:      message,
		ParseMode: models.ParseModeHTML,
	}
	if target.MessageThreadID > 0 {
		params.MessageThreadID = target.MessageThreadID
	}

	return m.send(ctx, params)
}

func (m *Module) send(ctx context.Context, params *telegrambot.SendMessageParams) error {
	ctx, cancel := withOptionalTimeout(ctx, m.requestTimeout)
	defer cancel()

	_, err := m.client.SendMessage(ctx, params)
	if err != nil {
		return fmt.Errorf("send telegram message: %w", err)
	}

	return nil
}

func (m *Module) LogFailure(err error, summary string) {
	switch {
	case isPermanentTelegramError(err):
		log.Printf("Permanent Telegram delivery failure %s: %v", summary, err)
	default:
		log.Printf("Transient Telegram delivery failure %s: %v", summary, err)
	}
}

func newBot(token string, client *http.Client, extraOptions ...telegrambot.Option) (*telegrambot.Bot, error) {
	options := []telegrambot.Option{telegrambot.WithSkipGetMe()}
	if client != nil {
		options = append(options, telegrambot.WithHTTPClient(sendTimeout, client))
	}
	options = append(options, extraOptions...)

	botClient, err := telegrambot.New(token, options...)
	if err != nil {
		return nil, fmt.Errorf("build telegram bot: %w", err)
	}

	return botClient, nil
}

func (m *Module) handleDefaultUpdate(ctx context.Context, _ *telegrambot.Bot, update *models.Update) {
	if m.defaultHandler == nil {
		return
	}

	translated := m.translateUpdate(update)
	if translated.Message == nil {
		log.Printf("Telegram inbound update ignored: unsupported update type")
		return
	}

	log.Printf(
		"Telegram inbound message chat_id=%d chat_type=%s message_id=%d from=%q text_len=%d entities=%d",
		translated.Message.ChatID,
		translated.Message.ChatType,
		translated.Message.MessageID,
		translated.Message.FromDisplayName,
		len(translated.Message.Text),
		len(translated.Message.Entities),
	)

	if err := m.defaultHandler(ctx, translated); err != nil {
		m.publishError(fmt.Errorf("telegram update handler: %w", err))
	}
}

func (m *Module) translateUpdate(update *models.Update) Update {
	if update == nil || update.Message == nil {
		return Update{}
	}

	return Update{
		Message: &IncomingMessage{
			ChatID:          update.Message.Chat.ID,
			ChatType:        update.Message.Chat.Type,
			MessageID:       update.Message.ID,
			MessageThreadID: update.Message.MessageThreadID,
			SentAt:          time.Unix(int64(update.Message.Date), 0).UTC(),
			FromUserID:      userID(update.Message.From),
			FromDisplayName: displayName(update.Message.From),
			FromIsBot:       update.Message.From != nil && update.Message.From.IsBot,
			Text:            update.Message.Text,
			Entities:        append([]models.MessageEntity(nil), update.Message.Entities...),
			BotUserID:       m.botUserID,
			BotUsername:     m.botUsername,
		},
	}
}

func (m *Module) publishError(err error) {
	if err == nil {
		return
	}

	select {
	case m.errCh <- err:
	default:
	}
}

func userID(user *models.User) int64 {
	if user == nil {
		return 0
	}

	return user.ID
}

func displayName(user *models.User) string {
	if user == nil {
		return ""
	}

	switch {
	case strings.TrimSpace(user.Username) != "":
		return "@" + strings.TrimSpace(user.Username)
	case strings.TrimSpace(user.FirstName) != "" && strings.TrimSpace(user.LastName) != "":
		return strings.TrimSpace(user.FirstName + " " + user.LastName)
	case strings.TrimSpace(user.FirstName) != "":
		return strings.TrimSpace(user.FirstName)
	case strings.TrimSpace(user.LastName) != "":
		return strings.TrimSpace(user.LastName)
	default:
		return ""
	}
}

func RenderHTML(text string, placeholderToURL map[string]string) string {
	var builder strings.Builder
	index := 0

	for index < len(text) {
		urlStart, urlEnd, rawURL, hasURL := 0, 0, "", false
		if loc := bodyURLPattern.FindStringIndex(text[index:]); loc != nil {
			urlStart = index + loc[0]
			urlEnd = index + loc[1]
			rawURL = text[urlStart:urlEnd]
			hasURL = true
		}

		placeholderStart, placeholderEnd, placeholderURL, hasPlaceholder := nextURLPlaceholder(text, index, placeholderToURL)

		switch {
		case hasPlaceholder && (!hasURL || placeholderStart <= urlStart):
			builder.WriteString(html.EscapeString(text[index:placeholderStart]))
			builder.WriteString(buildHTMLLink(placeholderURL))
			index = placeholderEnd
		case hasURL:
			builder.WriteString(html.EscapeString(text[index:urlStart]))

			url, suffix := splitTrailingURLPunctuation(rawURL)
			if url == "" {
				builder.WriteString(html.EscapeString(rawURL))
			} else {
				builder.WriteString(buildHTMLLink(url))
				builder.WriteString(html.EscapeString(suffix))
			}

			index = urlEnd
		default:
			builder.WriteString(html.EscapeString(text[index:]))
			index = len(text)
		}
	}

	return builder.String()
}

func TruncateMessage(message string) string {
	if visibleTelegramLength(message) <= MessageLimit {
		return message
	}

	maxContentLength := MessageLimit - len([]rune(TruncationNotice))
	if maxContentLength < 0 {
		maxContentLength = 0
	}

	var builder strings.Builder
	visibleCount := 0
	openAnchors := 0

	for index := 0; index < len(message); {
		if message[index] == '<' {
			tagEnd := strings.IndexByte(message[index:], '>')
			if tagEnd < 0 {
				break
			}

			tag := message[index : index+tagEnd+1]
			builder.WriteString(tag)
			switch {
			case strings.HasPrefix(tag, "<a "):
				openAnchors++
			case tag == "</a>" && openAnchors > 0:
				openAnchors--
			}
			index += tagEnd + 1
			continue
		}

		r, size := utf8.DecodeRuneInString(message[index:])
		if r == utf8.RuneError && size == 1 {
			break
		}
		if visibleCount >= maxContentLength {
			break
		}

		builder.WriteString(message[index : index+size])
		visibleCount++
		index += size
	}

	appendClosingTags(&builder, openAnchors)
	builder.WriteString(TruncationNotice)
	return builder.String()
}

func SplitText(text string, maxMessages int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	if maxMessages <= 0 {
		maxMessages = 1
	}

	chunks := make([]string, 0, maxMessages)
	remaining := text

	for len(chunks)+1 < maxMessages && plainTextLength(remaining) > MessageLimit {
		chunk, rest := splitTextChunk(remaining, MessageLimit)
		if chunk == "" {
			break
		}
		chunks = append(chunks, chunk)
		remaining = rest
	}

	if plainTextLength(remaining) > MessageLimit {
		maxContentLength := MessageLimit - len([]rune(TruncationNotice))
		if maxContentLength < 0 {
			maxContentLength = 0
		}

		chunk, _ := splitTextChunk(remaining, maxContentLength)
		if chunk == "" {
			remaining = TruncationNotice
		} else {
			remaining = chunk + TruncationNotice
		}
	}

	if remaining != "" {
		chunks = append(chunks, remaining)
	}

	return chunks
}

func isPermanentTelegramError(err error) bool {
	return errors.Is(err, telegrambot.ErrorBadRequest) ||
		errors.Is(err, telegrambot.ErrorUnauthorized) ||
		errors.Is(err, telegrambot.ErrorForbidden)
}

func splitTrailingURLPunctuation(candidate string) (string, string) {
	cut := len(candidate)
	for cut > 0 {
		last := rune(candidate[cut-1])
		if !strings.ContainsRune("\"'.,;:!?)]}>", last) {
			break
		}
		cut--
	}
	return candidate[:cut], candidate[cut:]
}

func formatURLLabel(rawURL string) string {
	href := rawURL
	if !strings.Contains(href, "://") {
		href = "https://" + rawURL
	}

	parsed, err := neturl.Parse(href)
	if err != nil {
		return rawURL
	}

	host := parsed.Hostname()
	if host == "" {
		return rawURL
	}

	label := host
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		label += "/..."
	}

	return label
}

func formatURLHref(rawURL string) string {
	if strings.Contains(rawURL, "://") {
		return rawURL
	}

	return "https://" + rawURL
}

func buildHTMLLink(rawURL string) string {
	href := formatURLHref(rawURL)
	label := formatURLLabel(rawURL)
	return fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(href), html.EscapeString(label))
}

func nextURLPlaceholder(text string, start int, placeholderToURL map[string]string) (int, int, string, bool) {
	bestStart := len(text)
	bestEnd := 0
	var bestURL string
	found := false

	for placeholder, url := range placeholderToURL {
		relative := strings.Index(text[start:], placeholder)
		if relative < 0 {
			continue
		}

		matchStart := start + relative
		if !found || matchStart < bestStart {
			bestStart = matchStart
			bestEnd = matchStart + len(placeholder)
			bestURL = url
			found = true
		}
	}

	return bestStart, bestEnd, bestURL, found
}

func visibleTelegramLength(message string) int {
	length := 0
	inTag := false
	for _, r := range message {
		switch {
		case r == '<':
			inTag = true
		case r == '>' && inTag:
			inTag = false
		case !inTag:
			length++
		}
	}
	return length
}

func plainTextLength(text string) int {
	return len([]rune(text))
}

func splitTextChunk(text string, limit int) (string, string) {
	if limit <= 0 {
		return "", strings.TrimSpace(text)
	}

	runes := []rune(text)
	if len(runes) <= limit {
		return text, ""
	}

	split := limit
	for index := limit - 1; index >= 0; index-- {
		if unicode.IsSpace(runes[index]) {
			split = index
			break
		}
	}
	if split == 0 {
		split = limit
	}

	chunk := strings.TrimRightFunc(string(runes[:split]), unicode.IsSpace)
	remainder := strings.TrimLeftFunc(string(runes[split:]), unicode.IsSpace)
	if chunk == "" {
		chunk = string(runes[:limit])
		remainder = strings.TrimLeftFunc(string(runes[limit:]), unicode.IsSpace)
	}

	return chunk, remainder
}

func appendClosingTags(builder *strings.Builder, openAnchors int) {
	for ; openAnchors > 0; openAnchors-- {
		builder.WriteString("</a>")
	}
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
