package main

import (
	"testing"
)

func TestLoadConfigUsesDefaults(t *testing.T) {
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("TELEGRAM_BOT_TOKEN", "bot-token")
	t.Setenv("TELEGRAM_CHAT_ID", "chat-id")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-key")
	t.Setenv("EMAIL_DIR", "/tmp/msgbot-mail")
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("PORT", "")
	t.Setenv("ANTHROPIC_MODEL", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}

	if cfg.ListenAddr != defaultListenAddr {
		t.Fatalf("loadConfig() ListenAddr = %q, want %q", cfg.ListenAddr, defaultListenAddr)
	}
	if cfg.Port != defaultPort {
		t.Fatalf("loadConfig() Port = %q, want %q", cfg.Port, defaultPort)
	}
	if cfg.AnthropicModel != defaultAnthropicModel {
		t.Fatalf("loadConfig() AnthropicModel = %q, want %q", cfg.AnthropicModel, defaultAnthropicModel)
	}
}

func TestLoadConfigRequiresWebhookSecret(t *testing.T) {
	t.Setenv("WEBHOOK_SECRET", "")
	t.Setenv("TELEGRAM_BOT_TOKEN", "bot-token")
	t.Setenv("TELEGRAM_CHAT_ID", "chat-id")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-key")
	t.Setenv("EMAIL_DIR", "/tmp/msgbot-mail")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("loadConfig() error = nil, want missing WEBHOOK_SECRET error")
	}
}

func TestLoadConfigAllowsMissingAnthropicAPIKey(t *testing.T) {
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("TELEGRAM_BOT_TOKEN", "bot-token")
	t.Setenv("TELEGRAM_CHAT_ID", "chat-id")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("EMAIL_DIR", "/tmp/msgbot-mail")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.AnthropicAPIKey != "" {
		t.Fatalf("loadConfig() AnthropicAPIKey = %q, want empty string", cfg.AnthropicAPIKey)
	}
}

func TestLoadConfigUsesConfiguredAnthropicModel(t *testing.T) {
	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("TELEGRAM_BOT_TOKEN", "bot-token")
	t.Setenv("TELEGRAM_CHAT_ID", "chat-id")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-key")
	t.Setenv("ANTHROPIC_MODEL", "claude-opus-4-20250514")
	t.Setenv("EMAIL_DIR", "/tmp/msgbot-mail")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.AnthropicModel != "claude-opus-4-20250514" {
		t.Fatalf("loadConfig() AnthropicModel = %q, want %q", cfg.AnthropicModel, "claude-opus-4-20250514")
	}
}
