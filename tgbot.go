package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jhillyerd/enmime"
)

const telegramMessageLimit = 4096
const telegramTruncationNotice = "\n\n[message truncated]"

var defaultTelegramHTTPClient = &http.Client{Timeout: 10 * time.Second}

type TelegramSender interface {
	SendMessage(context.Context, string) error
}

type TelegramBot struct {
	token  string
	chatID string
	client *http.Client
}

type telegramSendResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	ErrorCode   int    `json:"error_code"`
}

func newTelegramBot(token string, chatID string, client *http.Client) *TelegramBot {
	if client == nil {
		client = defaultTelegramHTTPClient
	}

	return &TelegramBot{
		token:  token,
		chatID: chatID,
		client: client,
	}
}

func truncateTelegramMessage(message string) string {
	runes := []rune(message)
	if len(runes) <= telegramMessageLimit {
		return message
	}

	maxContentLength := telegramMessageLimit - len([]rune(telegramTruncationNotice))
	if maxContentLength < 0 {
		maxContentLength = 0
	}

	return string(runes[:maxContentLength]) + telegramTruncationNotice
}

func buildTelegramMessage(env *enmime.Envelope, body string) string {
	message := fmt.Sprintf(
		"From: %s\nTo: %s\nSubject: %s\n\n%s",
		headerValue(env, "From", "(unknown sender)"),
		headerValue(env, "To", "(unknown receiver)"),
		headerValue(env, "Subject", "(no subject)"),
		body,
	)
	return truncateTelegramMessage(message)
}

func (b *TelegramBot) SendMessage(ctx context.Context, message string) error {
	payload, err := json.Marshal(map[string]string{
		"chat_id": b.chatID,
		"text":    message,
	})
	if err != nil {
		return fmt.Errorf("marshal telegram payload: %w", err)
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", b.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build telegram request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("post telegram message: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read telegram response: %w", err)
	}

	var telegramResp telegramSendResponse
	if len(body) > 0 {
		if err := json.Unmarshal(body, &telegramResp); err != nil {
			return fmt.Errorf("decode telegram response: %w", err)
		}
	}

	if resp.StatusCode != http.StatusOK || !telegramResp.OK {
		description := strings.TrimSpace(telegramResp.Description)
		if description == "" {
			description = strings.TrimSpace(string(body))
		}
		if description == "" {
			description = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("telegram sendMessage failed with status %d: %s", resp.StatusCode, description)
	}

	return nil
}

func logTelegramFailure(err error, summary string) {
	message := err.Error()
	switch {
	case strings.Contains(message, "status 400"),
		strings.Contains(message, "status 401"),
		strings.Contains(message, "status 403"):
		log.Printf("Permanent Telegram delivery failure %s: %v", summary, err)
	default:
		log.Printf("Transient Telegram delivery failure %s: %v", summary, err)
	}
}
