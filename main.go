package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultPort = "8181"
const defaultListenAddr = "127.0.0.1"

type Config struct {
	WebhookSecret    string
	TelegramBotToken string
	TelegramChatID   string
	AnthropicAPIKey  string
	AnthropicModel   string
	EmailDir         string
	ListenAddr       string
	Port             string
}

func requiredEnv(key string) (string, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return "", fmt.Errorf("%s environment variable not set", key)
	}
	return value, nil
}

func optionalEnv(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func loadConfig() (Config, error) {
	webhookSecret, err := requiredEnv("WEBHOOK_SECRET")
	if err != nil {
		return Config{}, err
	}

	telegramBotToken, err := requiredEnv("TELEGRAM_BOT_TOKEN")
	if err != nil {
		return Config{}, err
	}

	telegramChatID, err := requiredEnv("TELEGRAM_CHAT_ID")
	if err != nil {
		return Config{}, err
	}

	emailDir, err := requiredEnv("EMAIL_DIR")
	if err != nil {
		return Config{}, err
	}

	return Config{
		WebhookSecret:    webhookSecret,
		TelegramBotToken: telegramBotToken,
		TelegramChatID:   telegramChatID,
		AnthropicAPIKey:  strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
		AnthropicModel:   optionalEnv("ANTHROPIC_MODEL", defaultAnthropicModel),
		EmailDir:         emailDir,
		ListenAddr:       optionalEnv("LISTEN_ADDR", defaultListenAddr),
		Port:             optionalEnv("PORT", defaultPort),
	}, nil
}

func newServer(cfg Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              net.JoinHostPort(cfg.ListenAddr, cfg.Port),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	mux, err := newMux(cfg)
	if err != nil {
		log.Fatalf("Failed to build application: %v", err)
	}

	server := newServer(cfg, mux)

	log.Printf("Starting msgbot on %s with email_dir=%q", server.Addr, cfg.EmailDir)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Server stopped: %v", err)
	}
}
