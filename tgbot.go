package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	telegrambot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const telegramMessageLimit = 4096
const telegramTruncationNotice = "\n\n[message truncated]"

var defaultTelegramHTTPClient = &http.Client{Timeout: 10 * time.Second}

type TelegramSender struct {
	chatID string
	client *telegrambot.Bot
}

func newTelegramSender(token string, chatID string, client *http.Client) (*TelegramSender, error) {
	if client == nil {
		client = defaultTelegramHTTPClient
	}

	botClient, err := telegrambot.New(
		token,
		telegrambot.WithSkipGetMe(),
		telegrambot.WithHTTPClient(10*time.Second, client),
	)
	if err != nil {
		return nil, fmt.Errorf("build telegram bot: %w", err)
	}

	return &TelegramSender{
		chatID: chatID,
		client: botClient,
	}, nil
}

func truncateTelegramMessage(message string) string {
	if visibleTelegramLength(message) <= telegramMessageLimit {
		return message
	}

	maxContentLength := telegramMessageLimit - len([]rune(telegramTruncationNotice))
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
	builder.WriteString(telegramTruncationNotice)
	return builder.String()
}

func (b *TelegramSender) SendMessage(ctx context.Context, message string) error {
	_, err := b.client.SendMessage(ctx, &telegrambot.SendMessageParams{
		ChatID:    b.chatID,
		Text:      message,
		ParseMode: models.ParseModeHTML,
	})
	if err != nil {
		return fmt.Errorf("send telegram message: %w", err)
	}

	return nil
}

func isPermanentTelegramError(err error) bool {
	return errors.Is(err, telegrambot.ErrorBadRequest) ||
		errors.Is(err, telegrambot.ErrorUnauthorized) ||
		errors.Is(err, telegrambot.ErrorForbidden)
}

func (b *TelegramSender) LogFailure(err error, summary string) {
	switch {
	case isPermanentTelegramError(err):
		log.Printf("Permanent Telegram delivery failure %s: %v", summary, err)
	default:
		log.Printf("Transient Telegram delivery failure %s: %v", summary, err)
	}
}
