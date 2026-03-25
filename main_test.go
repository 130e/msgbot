package main

import (
	"fmt"
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

func TestBuildTelegramMessageIncludesHeaderAndBody(t *testing.T) {
	env := testEnvelope("alerts@example.com", "Status", "Plain body", "")

	message := buildTelegramMessage(env)

	if !strings.Contains(message, "From: alerts@example.com") {
		t.Fatalf("buildTelegramMessage() missing From header: %q", message)
	}
	if !strings.Contains(message, "Subject: Status") {
		t.Fatalf("buildTelegramMessage() missing Subject header: %q", message)
	}
	if !strings.Contains(message, "Plain body") {
		t.Fatalf("buildTelegramMessage() missing body: %q", message)
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

func TestSaveEmailWritesExactRawMessageToUniqueEMLFile(t *testing.T) {
	tmpDir := t.TempDir()
	previousEmailDir := emailDir
	emailDir = tmpDir
	t.Cleanup(func() {
		emailDir = previousEmailDir
	})

	raw := []byte("From: alerts@example.com\r\nSubject: Status\r\n\r\nHello team\r\n")

	firstPath, err := saveEmail(raw)
	if err != nil {
		t.Fatalf("saveEmail() first call error = %v", err)
	}
	secondPath, err := saveEmail(raw)
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
