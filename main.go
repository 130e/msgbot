package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jaytaylor/html2text"
	"github.com/jhillyerd/enmime"
)

var webhookSecret string
var emailDir string
var telegramBotToken string
var telegramChatID string
var listenAddr string
var port string

const telegramMessageLimit = 4096
const telegramTruncationNotice = "\n\n[message truncated]"
const emptyBodyNotice = "[email has no text body]"
const defaultPort = "8181"
const defaultListenAddr = "127.0.0.1"

var telegramHTTPClient = &http.Client{Timeout: 10 * time.Second}

type telegramSendResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	ErrorCode   int    `json:"error_code"`
}

func requiredEnv(key string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		log.Fatalf("%s environment variable not set", key)
	}
	return value
}

func optionalEnv(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func headerValue(env *enmime.Envelope, key string, fallback string) string {
	value := strings.TrimSpace(env.GetHeader(key))
	if value == "" {
		return fallback
	}
	return value
}

func emailBody(env *enmime.Envelope) string {
	if body := strings.TrimSpace(env.Text); body != "" {
		return body
	}

	if html := strings.TrimSpace(env.HTML); html != "" {
		body, err := html2text.FromString(html, html2text.Options{})
		if err != nil {
			log.Printf("Failed to convert HTML email body to text: %v", err)
		} else if body = strings.TrimSpace(body); body != "" {
			return body
		}
	}

	return emptyBodyNotice
}

func emailSummary(env *enmime.Envelope) string {
	messageID := headerValue(env, "Message-ID", "(missing)")
	return fmt.Sprintf(
		`from=%q to=%q subject=%q message_id=%q attachments=%d`,
		headerValue(env, "From", "(unknown sender)"),
		headerValue(env, "To", "(unknown receiver)"),
		headerValue(env, "Subject", "(no subject)"),
		messageID,
		len(env.Attachments),
	)
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

func buildTelegramMessage(env *enmime.Envelope) string {
	message := fmt.Sprintf(
		"From: %s\nTo: %s\nSubject: %s\n\n%s",
		headerValue(env, "From", "(unknown sender)"),
		headerValue(env, "To", "(unknown receiver)"),
		headerValue(env, "Subject", "(no subject)"),
		emailBody(env),
	)
	return truncateTelegramMessage(message)
}

func sendTelegramMessage(ctx context.Context, message string) error {
	payload, err := json.Marshal(map[string]string{
		"chat_id": telegramChatID,
		"text":    message,
	})
	if err != nil {
		return fmt.Errorf("marshal telegram payload: %w", err)
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", telegramBotToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build telegram request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := telegramHTTPClient.Do(req)
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

func saveEmail(raw []byte) (string, error) {
	if err := os.MkdirAll(emailDir, 0755); err != nil {
		return "", fmt.Errorf("prepare email directory %q: %w", emailDir, err)
	}

	file, err := os.CreateTemp(emailDir, "email-*.eml")
	if err != nil {
		return "", fmt.Errorf("create email dump file: %w", err)
	}
	path := file.Name()

	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write email dump file %q: %w", path, err)
	}

	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close email dump file %q: %w", path, err)
	}

	return path, nil
}

func emailHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		log.Printf("Rejected webhook request from %s: method=%s", r.RemoteAddr, r.Method)
		return
	}

	if r.Header.Get("X-Webhook-Secret") != webhookSecret {
		http.Error(w, "Forbidden", http.StatusForbidden)
		log.Printf("Rejected webhook request from %s: invalid secret", r.RemoteAddr)
		return
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Failed to read webhook body from %s: %v", r.RemoteAddr, err)
		http.Error(w, "Failed to read body", http.StatusInternalServerError)
		return
	}

	if len(raw) == 0 {
		log.Printf("Rejected webhook request from %s: empty body", r.RemoteAddr)
		http.Error(w, "Empty request body", http.StatusBadRequest)
		return
	}

	path, err := saveEmail(raw)
	if err != nil {
		log.Printf("Failed to save raw email from %s: %v", r.RemoteAddr, err)
		http.Error(w, "Failed to save email", http.StatusInternalServerError)
		return
	}
	log.Printf("Saved email to %q", path)

	env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
	if err != nil {
		log.Printf("Failed to parse email from %s after saving to %q: %v", r.RemoteAddr, path, err)
		http.Error(w, fmt.Sprintf("Failed to parse email: %v", err), http.StatusBadRequest)
		return
	}

	summary := emailSummary(env)
	telegramMessage := buildTelegramMessage(env)
	if err := sendTelegramMessage(r.Context(), telegramMessage); err != nil {
		logTelegramFailure(err, summary)
		http.Error(w, "Failed to deliver message to Telegram", http.StatusBadGateway)
		return
	}

	log.Printf("Processed email %s saved_to=%q", summary, path)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func loadConfig() {
	webhookSecret = requiredEnv("WEBHOOK_SECRET")
	telegramBotToken = requiredEnv("TELEGRAM_BOT_TOKEN")
	telegramChatID = requiredEnv("TELEGRAM_CHAT_ID")
	emailDir = requiredEnv("EMAIL_DIR")
	listenAddr = optionalEnv("LISTEN_ADDR", defaultListenAddr)
	port = optionalEnv("PORT", defaultPort)
}

func main() {
	loadConfig()

	mux := http.NewServeMux()
	mux.HandleFunc("/email/notify", emailHandler)

	server := &http.Server{
		Addr:              listenAddr + ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("Starting msgbot on %s with email_dir=%q", server.Addr, emailDir)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Server stopped: %v", err)
	}
}
