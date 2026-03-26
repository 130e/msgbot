package main

import (
	"strings"
	"testing"
)

func TestBuildTelegramMessageIncludesHeaderAndBody(t *testing.T) {
	env := testEnvelope("alerts@example.com", "Status", "Plain body", "")

	message := buildTelegramMessage(env, "Short summary")

	if !strings.Contains(message, "From: alerts@example.com") {
		t.Fatalf("buildTelegramMessage() missing From header: %q", message)
	}
	if !strings.Contains(message, "Subject: Status") {
		t.Fatalf("buildTelegramMessage() missing Subject header: %q", message)
	}
	if !strings.Contains(message, "Short summary") {
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
