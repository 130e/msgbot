package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	telegrambot "github.com/go-telegram/bot"
)

func TestBuildEmailMessageIncludesHeaderAndBody(t *testing.T) {
	env := testEnvelope("alerts@example.com", "Status", "Plain body", "")

	message := buildEmailMessage(env, "Short summary", nil)

	if !strings.Contains(message, "From: alerts@example.com") {
		t.Fatalf("buildEmailMessage() missing From header: %q", message)
	}
	if !strings.Contains(message, "Subject: Status") {
		t.Fatalf("buildEmailMessage() missing Subject header: %q", message)
	}
	if !strings.Contains(message, "Short summary") {
		t.Fatalf("buildEmailMessage() missing body: %q", message)
	}
}

func TestTruncateTelegramMessage(t *testing.T) {
	longMessage := strings.Repeat("a", telegramMessageLimit+100)

	got := truncateTelegramMessage(longMessage)

	if len([]rune(got)) != telegramMessageLimit {
		t.Fatalf("truncateTelegramMessage() length = %d, want %d", len([]rune(got)), telegramMessageLimit)
	}
	if !strings.HasSuffix(got, telegramTruncationNotice) {
		t.Fatalf("truncateTelegramMessage() missing truncation notice: %q", got[len(got)-len(telegramTruncationNotice):])
	}
}

func TestNewTelegramSenderSkipsGetMeOnInit(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("newTelegramSender() unexpectedly called Telegram during init: %s", req.URL.String())
			return nil, nil
		}),
	}

	sender, err := newTelegramSender("token", "12345", client)
	if err != nil {
		t.Fatalf("newTelegramSender() error = %v", err)
	}
	if sender == nil {
		t.Fatal("newTelegramSender() returned nil sender")
	}
}

func TestTelegramSenderSendMessageSetsHTMLParseMode(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("SendMessage() method = %q, want %q", req.Method, http.MethodPost)
			}
			if got, want := req.URL.Path, "/bottoken/sendMessage"; got != want {
				t.Fatalf("SendMessage() path = %q, want %q", got, want)
			}

			mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
			if err != nil {
				t.Fatalf("ParseMediaType(Content-Type) error = %v", err)
			}
			if mediaType != "multipart/form-data" {
				t.Fatalf("SendMessage() content type = %q, want multipart/form-data", mediaType)
			}

			reader := multipart.NewReader(req.Body, params["boundary"])
			values := make(map[string]string)
			for {
				part, err := reader.NextPart()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("NextPart() error = %v", err)
				}

				body, err := io.ReadAll(part)
				if err != nil {
					t.Fatalf("ReadAll(part) error = %v", err)
				}
				values[part.FormName()] = string(body)
			}

			if got := values["chat_id"]; got != "12345" {
				t.Fatalf("SendMessage() chat_id = %q, want %q", got, "12345")
			}
			if got := values["text"]; got != `<a href="https://example.com">example.com</a>` {
				t.Fatalf("SendMessage() text = %q, want HTML message body", got)
			}
			if got := values["parse_mode"]; got != "HTML" {
				t.Fatalf("SendMessage() parse_mode = %q, want %q", got, "HTML")
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":12345,"type":"private"}}}`)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	sender, err := newTelegramSender("token", "12345", client)
	if err != nil {
		t.Fatalf("newTelegramSender() error = %v", err)
	}
	if err := sender.SendMessage(context.Background(), `<a href="https://example.com">example.com</a>`); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
}

func TestIsPermanentTelegramError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "bad request", err: fmt.Errorf("wrapped: %w", telegrambot.ErrorBadRequest), want: true},
		{name: "unauthorized", err: fmt.Errorf("wrapped: %w", telegrambot.ErrorUnauthorized), want: true},
		{name: "forbidden", err: fmt.Errorf("wrapped: %w", telegrambot.ErrorForbidden), want: true},
		{name: "too many requests", err: &telegrambot.TooManyRequestsError{Message: "rate limited", RetryAfter: 1}, want: false},
		{name: "network", err: &url.Error{Op: "Post", URL: "https://api.telegram.org", Err: io.EOF}, want: false},
		{name: "other", err: errors.New("boom"), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPermanentTelegramError(tc.err); got != tc.want {
				t.Fatalf("isPermanentTelegramError() = %v, want %v", got, tc.want)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
