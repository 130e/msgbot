package main

import (
	"context"
	"net/http"
)

type Summarizer interface {
	Summarize(ctx context.Context, body string) (string, error)
}

type Sender interface {
	SendMessage(ctx context.Context, message string) error
	LogFailure(err error, summary string)
}

func newSummarizer(cfg Config) Summarizer {
	if cfg.AnthropicAPIKey == "" {
		return passthroughSummarizer{}
	}

	return newClaudeAgent(cfg.AnthropicAPIKey, cfg.AnthropicModel, nil)
}

func newSender(cfg Config) (Sender, error) {
	return newTelegramSender(cfg.TelegramBotToken, cfg.TelegramChatID, nil)
}

func newEmailNotifyHandler(cfg Config) (http.HandlerFunc, error) {
	sender, err := newSender(cfg)
	if err != nil {
		return nil, err
	}

	return newEmailHandler(cfg, newSummarizer(cfg), sender), nil
}

func newMux(cfg Config) (*http.ServeMux, error) {
	handler, err := newEmailNotifyHandler(cfg)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/email/notify", handler)
	return mux, nil
}
