package main

import (
	"bytes"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"regexp"
	"strings"

	"github.com/jaytaylor/html2text"
	"github.com/jhillyerd/enmime"
)

const emptyBodyNotice = "[email has no text body]"
const urlPlaceholderFormat = "MSGURL%03dTOKEN"
const urlPlaceholderVariantFormat = "MSGURLV%d%%03dTOKEN"

var bodyURLPattern = regexp.MustCompile(`(?:https?://|www\.|[A-Za-z0-9.-]+\.[A-Za-z]{2,}/)\S+`)

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

func replaceURLsWithPlaceholders(body string) (string, map[string]string) {
	matches := bodyURLPattern.FindAllString(body, -1)
	if len(matches) == 0 {
		return body, nil
	}

	orderedURLs := make([]string, 0, len(matches))
	seenURLs := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		url, _ := splitTrailingURLPunctuation(match)
		if url == "" {
			continue
		}
		if _, ok := seenURLs[url]; ok {
			continue
		}
		seenURLs[url] = struct{}{}
		orderedURLs = append(orderedURLs, url)
	}
	if len(orderedURLs) == 0 {
		return body, nil
	}

	placeholderFormat := chooseURLPlaceholderFormat(body, len(orderedURLs))
	urlToPlaceholder := make(map[string]string, len(orderedURLs))
	placeholderToURL := make(map[string]string, len(orderedURLs))
	for i, url := range orderedURLs {
		placeholder := fmt.Sprintf(placeholderFormat, i+1)
		urlToPlaceholder[url] = placeholder
		placeholderToURL[placeholder] = url
	}

	replaced := bodyURLPattern.ReplaceAllStringFunc(body, func(match string) string {
		url, suffix := splitTrailingURLPunctuation(match)
		if url == "" {
			return match
		}
		placeholder, ok := urlToPlaceholder[url]
		if !ok {
			return match
		}
		return placeholder + suffix
	})

	return replaced, placeholderToURL
}

func chooseURLPlaceholderFormat(body string, count int) string {
	for variant := 0; ; variant++ {
		format := urlPlaceholderFormat
		if variant > 0 {
			format = fmt.Sprintf(urlPlaceholderVariantFormat, variant)
		}

		collision := false
		for i := 1; i <= count; i++ {
			if strings.Contains(body, fmt.Sprintf(format, i)) {
				collision = true
				break
			}
		}
		if !collision {
			return format
		}
	}
}

func splitTrailingURLPunctuation(candidate string) (string, string) {
	cut := len(candidate)
	for cut > 0 {
		last := rune(candidate[cut-1])
		if !strings.ContainsRune("\"'.,;:!?)]}>", last) {
			break
		}
		cut--
	}
	return candidate[:cut], candidate[cut:]
}

func formatURLLabel(rawURL string) string {
	href := rawURL
	if !strings.Contains(href, "://") {
		href = "https://" + rawURL
	}

	parsed, err := neturl.Parse(href)
	if err != nil {
		return rawURL
	}

	host := parsed.Hostname()
	if host == "" {
		return rawURL
	}

	label := host
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		label += "/..."
	}

	return label
}

func formatURLHref(rawURL string) string {
	if strings.Contains(rawURL, "://") {
		return rawURL
	}

	return "https://" + rawURL
}

func buildTelegramHTMLLink(rawURL string) string {
	href := formatURLHref(rawURL)
	label := formatURLLabel(rawURL)
	return fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(href), html.EscapeString(label))
}

func nextURLPlaceholder(text string, start int, placeholderToURL map[string]string) (int, int, string, bool) {
	bestStart := len(text)
	bestEnd := 0
	var bestURL string
	found := false

	for placeholder, url := range placeholderToURL {
		relative := strings.Index(text[start:], placeholder)
		if relative < 0 {
			continue
		}

		matchStart := start + relative
		if !found || matchStart < bestStart {
			bestStart = matchStart
			bestEnd = matchStart + len(placeholder)
			bestURL = url
			found = true
		}
	}

	return bestStart, bestEnd, bestURL, found
}

func renderTelegramHTML(text string, placeholderToURL map[string]string) string {
	var builder strings.Builder
	index := 0

	for index < len(text) {
		urlStart, urlEnd, rawURL, hasURL := 0, 0, "", false
		if loc := bodyURLPattern.FindStringIndex(text[index:]); loc != nil {
			urlStart = index + loc[0]
			urlEnd = index + loc[1]
			rawURL = text[urlStart:urlEnd]
			hasURL = true
		}

		placeholderStart, placeholderEnd, placeholderURL, hasPlaceholder := nextURLPlaceholder(text, index, placeholderToURL)

		switch {
		case hasPlaceholder && (!hasURL || placeholderStart <= urlStart):
			builder.WriteString(html.EscapeString(text[index:placeholderStart]))
			builder.WriteString(buildTelegramHTMLLink(placeholderURL))
			index = placeholderEnd
		case hasURL:
			builder.WriteString(html.EscapeString(text[index:urlStart]))

			url, suffix := splitTrailingURLPunctuation(rawURL)
			if url == "" {
				builder.WriteString(html.EscapeString(rawURL))
			} else {
				builder.WriteString(buildTelegramHTMLLink(url))
				builder.WriteString(html.EscapeString(suffix))
			}

			index = urlEnd
		default:
			builder.WriteString(html.EscapeString(text[index:]))
			index = len(text)
		}
	}

	return builder.String()
}

func visibleTelegramLength(message string) int {
	length := 0
	inTag := false
	for _, r := range message {
		switch {
		case r == '<':
			inTag = true
		case r == '>' && inTag:
			inTag = false
		case !inTag:
			length++
		}
	}
	return length
}

func appendClosingTags(builder *strings.Builder, openAnchors int) {
	for ; openAnchors > 0; openAnchors-- {
		builder.WriteString("</a>")
	}
}

func containsAnyURLPlaceholder(body string, placeholderToURL map[string]string) bool {
	for placeholder := range placeholderToURL {
		if strings.Contains(body, placeholder) {
			return true
		}
	}
	return false
}

func saveEmail(emailDir string, raw []byte) (string, error) {
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

func buildEmailMessage(env *enmime.Envelope, body string, placeholderToURL map[string]string) string {
	var builder strings.Builder
	builder.WriteString("From: ")
	builder.WriteString(renderTelegramHTML(headerValue(env, "From", "(unknown sender)"), nil))
	builder.WriteString("\nTo: ")
	builder.WriteString(renderTelegramHTML(headerValue(env, "To", "(unknown receiver)"), nil))
	builder.WriteString("\nSubject: ")
	builder.WriteString(renderTelegramHTML(headerValue(env, "Subject", "(no subject)"), nil))
	builder.WriteString("\n\n")
	builder.WriteString(renderTelegramHTML(body, placeholderToURL))
	return truncateTelegramMessage(builder.String())
}

func newEmailHandler(cfg Config, summarizer Summarizer, sender Sender) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			log.Printf("Rejected webhook request from %s: method=%s", r.RemoteAddr, r.Method)
			return
		}

		if r.Header.Get("X-Webhook-Secret") != cfg.WebhookSecret {
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

		path, err := saveEmail(cfg.EmailDir, raw)
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
		body := emailBody(env)
		messageBody := body
		messagePlaceholders := map[string]string(nil)
		if body != emptyBodyNotice {
			summaryInput := body
			placeholderToURL := map[string]string(nil)
			summaryInput, placeholderToURL = replaceURLsWithPlaceholders(body)

			summaryText, err := summarizer.Summarize(r.Context(), summaryInput)
			if err != nil {
				log.Printf("Failed to summarize email %s: %v", summary, err)
			} else {
				if len(placeholderToURL) > 0 {
					if !containsAnyURLPlaceholder(summaryText, placeholderToURL) {
						log.Printf("Summarizer omitted all %d URL placeholders for email %s", len(placeholderToURL), summary)
					}
					messagePlaceholders = placeholderToURL
				}
				messageBody = summaryText
			}
		}

		message := buildEmailMessage(env, messageBody, messagePlaceholders)
		if err := sender.SendMessage(r.Context(), message); err != nil {
			sender.LogFailure(err, summary)
			http.Error(w, "Sender failed to deliver message", http.StatusBadGateway)
			return
		}

		log.Printf("Processed email %s saved_to=%q", summary, path)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}
