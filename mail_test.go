package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhillyerd/enmime"
)

func testEnvelope(from string, subject string, text string, html string) *enmime.Envelope {
	var body string
	contentType := "text/plain; charset=UTF-8"
	switch {
	case strings.TrimSpace(text) != "" && strings.TrimSpace(html) != "":
		contentType = "multipart/alternative; boundary=boundary42"
		body = fmt.Sprintf("--boundary42\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n--boundary42\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n%s\r\n--boundary42--\r\n", text, html)
	case strings.TrimSpace(html) != "":
		contentType = "text/html; charset=UTF-8"
		body = html
	default:
		body = text
	}

	raw := fmt.Sprintf("From: %s\r\nSubject: %s\r\nContent-Type: %s\r\n\r\n%s", from, subject, contentType, body)
	env, err := enmime.ReadEnvelope(strings.NewReader(raw))
	if err != nil {
		panic(err)
	}

	return env
}

type fakeTelegramSender struct {
	err      error
	messages []string
}

type fakeSummarizer struct {
	err           error
	summary       string
	bodies        []string
	summarizeFunc func(string) (string, error)
}

func (f *fakeTelegramSender) SendMessage(_ context.Context, message string) error {
	f.messages = append(f.messages, message)
	return f.err
}

func (f *fakeSummarizer) Summarize(_ context.Context, body string) (string, error) {
	f.bodies = append(f.bodies, body)
	if f.summarizeFunc != nil {
		return f.summarizeFunc(body)
	}
	if f.err != nil {
		return "", f.err
	}
	return f.summary, nil
}

func TestEmailBodyPrefersPlainText(t *testing.T) {
	env := testEnvelope("alerts@example.com", "Status", "Plain body", "<p>HTML body</p>")

	body := emailBody(env)

	if body != "Plain body" {
		t.Fatalf("emailBody() = %q, want %q", body, "Plain body")
	}
}

func TestEmailBodyFallsBackToHTMLText(t *testing.T) {
	env := testEnvelope("alerts@example.com", "Status", "", "<p>Hello <strong>team</strong></p>")

	body := emailBody(env)

	if !strings.Contains(body, "Hello") || !strings.Contains(body, "team") {
		t.Fatalf("emailBody() = %q, want converted HTML text", body)
	}
}

func TestEmailBodyUsesPlaceholderWhenEmpty(t *testing.T) {
	env := testEnvelope("alerts@example.com", "Status", " \n\t ", " ")

	body := emailBody(env)

	if body != emptyBodyNotice {
		t.Fatalf("emailBody() = %q, want %q", body, emptyBodyNotice)
	}
}

func TestSaveEmailWritesExactRawMessageToUniqueEMLFile(t *testing.T) {
	tmpDir := t.TempDir()
	raw := []byte("From: alerts@example.com\r\nSubject: Status\r\n\r\nHello team\r\n")

	firstPath, err := saveEmail(tmpDir, raw)
	if err != nil {
		t.Fatalf("saveEmail() first call error = %v", err)
	}
	secondPath, err := saveEmail(tmpDir, raw)
	if err != nil {
		t.Fatalf("saveEmail() second call error = %v", err)
	}

	if filepath.Dir(firstPath) != tmpDir {
		t.Fatalf("saveEmail() first path dir = %q, want %q", filepath.Dir(firstPath), tmpDir)
	}
	if filepath.Ext(firstPath) != ".eml" {
		t.Fatalf("saveEmail() first path ext = %q, want %q", filepath.Ext(firstPath), ".eml")
	}
	if firstPath == secondPath {
		t.Fatalf("saveEmail() reused path %q, want unique file names", firstPath)
	}

	got, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", firstPath, err)
	}
	if string(got) != string(raw) {
		t.Fatalf("saved raw email = %q, want %q", string(got), string(raw))
	}
}

func TestReplaceURLsWithPlaceholdersReusesTokensAndRestoresURLs(t *testing.T) {
	body := "Verify here: https://example.com/verify?token=abc.\nRead more: example.com/help/reset.\nRepeat: https://example.com/verify?token=abc"

	replaced, placeholderToURL := replaceURLsWithPlaceholders(body)

	if strings.Contains(replaced, "https://example.com/verify?token=abc") {
		t.Fatalf("replaceURLsWithPlaceholders() = %q, want verification URL replaced", replaced)
	}
	if strings.Contains(replaced, "example.com/help/reset") {
		t.Fatalf("replaceURLsWithPlaceholders() = %q, want help URL replaced", replaced)
	}
	if strings.Count(replaced, "[[MSG_URL_001]]") != 2 {
		t.Fatalf("replaceURLsWithPlaceholders() = %q, want repeated URL to reuse placeholder", replaced)
	}
	if strings.Count(replaced, "[[MSG_URL_002]]") != 1 {
		t.Fatalf("replaceURLsWithPlaceholders() = %q, want second URL placeholder", replaced)
	}

	restored := restoreURLPlaceholders("Verify: [[MSG_URL_001]]", placeholderToURL)
	if restored != "Verify: https://example.com/verify?token=abc" {
		t.Fatalf("restoreURLPlaceholders() = %q, want restored verification URL", restored)
	}
}

func TestReplaceURLsWithPlaceholdersAvoidsPlaceholderCollisions(t *testing.T) {
	body := "Already present [[MSG_URL_001]] and actual link https://example.com/verify?token=abc"

	replaced, placeholderToURL := replaceURLsWithPlaceholders(body)

	if strings.Contains(replaced, "[[MSG_URL_001]] and actual link [[MSG_URL_001]]") {
		t.Fatalf("replaceURLsWithPlaceholders() = %q, want alternate placeholder format", replaced)
	}
	if !strings.Contains(replaced, "[[MSG_URL_1_001]]") {
		t.Fatalf("replaceURLsWithPlaceholders() = %q, want collision-safe placeholder", replaced)
	}
	if got := restoreURLPlaceholders("[[MSG_URL_1_001]]", placeholderToURL); got != "https://example.com/verify?token=abc" {
		t.Fatalf("restoreURLPlaceholders() = %q, want restored collision-safe placeholder", got)
	}
}

func TestNewEmailHandlerReturnsOKAndSavesEmail(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := Config{
		WebhookSecret: "secret",
		EmailDir:      tmpDir,
	}
	summarizer := &fakeSummarizer{
		summarizeFunc: func(body string) (string, error) {
			if body != "Hello team" {
				t.Fatalf("Summarize() body = %q, want raw body without placeholders", body)
			}
			return "Login code is 123456.", nil
		},
	}
	sender := &fakeTelegramSender{}
	raw := "From: alerts@example.com\r\nTo: bot@example.com\r\nSubject: Status\r\n\r\nHello team\r\n"

	req := httptest.NewRequest(http.MethodPost, "/email/notify", strings.NewReader(raw))
	req.Header.Set("X-Webhook-Secret", cfg.WebhookSecret)
	rec := httptest.NewRecorder()

	newEmailHandler(cfg, summarizer, sender).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("newEmailHandler() status = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"ok":true}` {
		t.Fatalf("newEmailHandler() body = %q, want %q", body, `{"ok":true}`)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("SendMessage() calls = %d, want 1", len(sender.messages))
	}
	if len(summarizer.bodies) != 1 {
		t.Fatalf("Summarize() calls = %d, want 1", len(summarizer.bodies))
	}
	if !strings.Contains(sender.messages[0], "Login code is 123456.") {
		t.Fatalf("telegram message = %q, want summarized body", sender.messages[0])
	}
	if strings.Contains(sender.messages[0], "Hello team") {
		t.Fatalf("telegram message = %q, should not contain raw body after successful summarization", sender.messages[0])
	}

	files, err := filepath.Glob(filepath.Join(tmpDir, "*.eml"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("saved .eml file count = %d, want 1", len(files))
	}
}

func TestNewEmailHandlerRejectsInvalidSecret(t *testing.T) {
	cfg := Config{
		WebhookSecret: "secret",
		EmailDir:      t.TempDir(),
	}
	summarizer := &fakeSummarizer{}
	sender := &fakeTelegramSender{}

	req := httptest.NewRequest(http.MethodPost, "/email/notify", strings.NewReader("hello"))
	req.Header.Set("X-Webhook-Secret", "wrong")
	rec := httptest.NewRecorder()

	newEmailHandler(cfg, summarizer, sender).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("newEmailHandler() status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if len(sender.messages) != 0 {
		t.Fatalf("SendMessage() calls = %d, want 0", len(sender.messages))
	}
}

func TestNewEmailHandlerReturnsBadGatewayOnTelegramFailure(t *testing.T) {
	cfg := Config{
		WebhookSecret: "secret",
		EmailDir:      t.TempDir(),
	}
	summarizer := &fakeSummarizer{summary: "Summarized text."}
	sender := &fakeTelegramSender{err: errors.New("telegram sendMessage failed with status 500: upstream down")}
	raw := "From: alerts@example.com\r\nTo: bot@example.com\r\nSubject: Status\r\n\r\nHello team\r\n"

	req := httptest.NewRequest(http.MethodPost, "/email/notify", strings.NewReader(raw))
	req.Header.Set("X-Webhook-Secret", cfg.WebhookSecret)
	rec := httptest.NewRecorder()

	newEmailHandler(cfg, summarizer, sender).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("newEmailHandler() status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

func TestNewEmailHandlerFallsBackToRawBodyWhenSummarizerFails(t *testing.T) {
	cfg := Config{
		WebhookSecret: "secret",
		EmailDir:      t.TempDir(),
	}
	summarizer := &fakeSummarizer{err: errors.New("anthropic timeout")}
	sender := &fakeTelegramSender{}
	raw := "From: alerts@example.com\r\nTo: bot@example.com\r\nSubject: Status\r\n\r\nVerify here: https://example.com/verify?token=abc\r\n"

	req := httptest.NewRequest(http.MethodPost, "/email/notify", strings.NewReader(raw))
	req.Header.Set("X-Webhook-Secret", cfg.WebhookSecret)
	rec := httptest.NewRecorder()

	newEmailHandler(cfg, summarizer, sender).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("newEmailHandler() status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("SendMessage() calls = %d, want 1", len(sender.messages))
	}
	if !strings.Contains(sender.messages[0], "https://example.com/verify?token=abc") {
		t.Fatalf("telegram message = %q, want raw body fallback", sender.messages[0])
	}
	if strings.Contains(sender.messages[0], "[[MSG_URL_") {
		t.Fatalf("telegram message = %q, should not leak URL placeholders after summarizer failure", sender.messages[0])
	}
}

func TestNewEmailHandlerUsesRawBodyWhenAnthropicDisabled(t *testing.T) {
	cfg := Config{
		WebhookSecret: "secret",
		EmailDir:      t.TempDir(),
	}
	sender := &fakeTelegramSender{}
	raw := "From: alerts@example.com\r\nTo: bot@example.com\r\nSubject: Status\r\n\r\nHello team\r\n"

	req := httptest.NewRequest(http.MethodPost, "/email/notify", strings.NewReader(raw))
	req.Header.Set("X-Webhook-Secret", cfg.WebhookSecret)
	rec := httptest.NewRecorder()

	newEmailHandler(cfg, newSummarizer(cfg), sender).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("newEmailHandler() status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("SendMessage() calls = %d, want 1", len(sender.messages))
	}
	if !strings.Contains(sender.messages[0], "Hello team") {
		t.Fatalf("telegram message = %q, want raw body when Anthropic is disabled", sender.messages[0])
	}
}

func TestNewEmailHandlerRestoresURLPlaceholdersInSummary(t *testing.T) {
	cfg := Config{
		WebhookSecret: "secret",
		EmailDir:      t.TempDir(),
	}
	verifyURL := "https://ablink.m.popeyes.com/ls/click?upn=verify-token"
	infoURL := "www.popeyes.com/rewards/details?lid=qxhvbg0r09i3"
	summarizer := &fakeSummarizer{
		summarizeFunc: func(body string) (string, error) {
			if strings.Contains(body, verifyURL) || strings.Contains(body, infoURL) {
				t.Fatalf("Summarize() body = %q, want URLs replaced with placeholders", body)
			}
			if !strings.Contains(body, "[[MSG_URL_001]]") || !strings.Contains(body, "[[MSG_URL_002]]") {
				t.Fatalf("Summarize() body = %q, want placeholderized URLs", body)
			}
			return "Popeyes email verification\n\nAction needed: Verify your email.\nClick to verify: [[MSG_URL_001]]", nil
		},
	}
	sender := &fakeTelegramSender{}
	raw := fmt.Sprintf("From: Popeyes <offers@m.popeyes.com>\r\nTo: bot@example.com\r\nSubject: You're Popeyes-official! Let's get you logged in.\r\n\r\nWelcome to Popeyes.\r\nVerify your email: %s\r\nMore info: %s\r\n", verifyURL, infoURL)

	req := httptest.NewRequest(http.MethodPost, "/email/notify", strings.NewReader(raw))
	req.Header.Set("X-Webhook-Secret", cfg.WebhookSecret)
	rec := httptest.NewRecorder()

	newEmailHandler(cfg, summarizer, sender).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("newEmailHandler() status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("SendMessage() calls = %d, want 1", len(sender.messages))
	}
	if !strings.Contains(sender.messages[0], verifyURL) {
		t.Fatalf("telegram message = %q, want restored verification URL", sender.messages[0])
	}
	if strings.Contains(sender.messages[0], "[[MSG_URL_") {
		t.Fatalf("telegram message = %q, should not leak URL placeholders", sender.messages[0])
	}
}
