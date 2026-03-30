package mail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jaytaylor/html2text"
	"github.com/jhillyerd/enmime"
)

const (
	DefaultListenAddr           = "127.0.0.1"
	DefaultPort                 = 8181
	DefaultPath                 = "/email/notify"
	EmptyBodyNotice             = "[email has no text body]"
	urlPlaceholderFormat        = "MSGURL%03dTOKEN"
	urlPlaceholderVariantFormat = "MSGURLV%d%%03dTOKEN"
	processingTimeout           = 30 * time.Second
)

var bodyURLPattern = regexp.MustCompile(`(?:https?://|www\.|[A-Za-z0-9.-]+\.[A-Za-z]{2,}/)\S+`)

type Config struct {
	ListenAddr    string `toml:"listen_addr"`
	Port          int    `toml:"port"`
	Path          string `toml:"path"`
	WebhookSecret string `toml:"webhook_secret"`
	EmailDir      string `toml:"email_dir"`
}

type Message struct {
	RawPath         string
	From            string
	To              string
	Subject         string
	Body            string
	URLPlaceholders map[string]string
	AttachmentCount int
}

type Handler func(context.Context, Message) error

type Module struct {
	cfg      Config
	handler  Handler
	server   *http.Server
	listener net.Listener
	errCh    chan error
}

func New(cfg Config) *Module {
	return &Module{
		cfg:   cfg,
		errCh: make(chan error, 1),
	}
}

func (m *Module) SetHandler(handler Handler) {
	m.handler = handler
}

func (m *Module) Errors() <-chan error {
	return m.errCh
}

func (m *Module) Up(context.Context) error {
	if m.handler == nil {
		return fmt.Errorf("mail handler not configured")
	}

	log.Printf("Mail module starting")

	cfg, err := normalizedConfig(m.cfg)
	if err != nil {
		return err
	}

	m.cfg = cfg

	mux := http.NewServeMux()
	mux.HandleFunc(cfg.Path, m.handleNotify)

	server := &http.Server{
		Addr:              net.JoinHostPort(cfg.ListenAddr, strconv.Itoa(cfg.Port)),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("start mail listener on %s: %w", server.Addr, err)
	}

	m.server = server
	m.listener = listener

	go func() {
		if err := m.server.Serve(m.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case m.errCh <- fmt.Errorf("mail server stopped: %w", err):
			default:
			}
		}
	}()

	log.Printf("Mail module listening on http://%s%s", server.Addr, cfg.Path)
	return nil
}

func (m *Module) Down(ctx context.Context) error {
	if m.server == nil {
		return nil
	}

	log.Printf("Mail module stopping")
	err := m.server.Shutdown(ctx)
	m.server = nil
	m.listener = nil
	return err
}

func (m *Module) handleNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		log.Printf("Rejected webhook request from %s: method=%s", r.RemoteAddr, r.Method)
		return
	}

	if r.Header.Get("X-Webhook-Secret") != m.cfg.WebhookSecret {
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

	path, err := saveEmail(m.cfg.EmailDir, raw)
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

	body, placeholderToURL := replaceURLsWithPlaceholders(emailBody(env))
	message := Message{
		RawPath:         path,
		From:            headerValue(env, "From", "(unknown sender)"),
		To:              headerValue(env, "To", "(unknown receiver)"),
		Subject:         headerValue(env, "Subject", "(no subject)"),
		Body:            body,
		URLPlaceholders: placeholderToURL,
		AttachmentCount: len(env.Attachments),
	}

	workCtx, cancel := detachedTimeoutContext(r.Context(), processingTimeout)
	defer cancel()

	if err := m.handler(workCtx, message); err != nil {
		log.Printf("Mail task failed for %s saved_to=%q: %v", logSummary(message), path, err)
		http.Error(w, "Failed to process email", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func normalizedConfig(cfg Config) (Config, error) {
	if strings.TrimSpace(cfg.ListenAddr) == "" {
		cfg.ListenAddr = DefaultListenAddr
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	if strings.TrimSpace(cfg.Path) == "" {
		cfg.Path = DefaultPath
	}

	cfg.WebhookSecret = strings.TrimSpace(cfg.WebhookSecret)
	cfg.EmailDir = strings.TrimSpace(cfg.EmailDir)

	switch {
	case cfg.WebhookSecret == "":
		return Config{}, fmt.Errorf("modules.mail.webhook_secret is required")
	case cfg.EmailDir == "":
		return Config{}, fmt.Errorf("modules.mail.email_dir is required")
	case !filepath.IsAbs(cfg.EmailDir):
		return Config{}, fmt.Errorf("modules.mail.email_dir must be an absolute path")
	case !strings.HasPrefix(cfg.Path, "/"):
		return Config{}, fmt.Errorf("modules.mail.path must start with /")
	case cfg.Port < 1 || cfg.Port > 65535:
		return Config{}, fmt.Errorf("modules.mail.port must be between 1 and 65535")
	}

	return cfg, nil
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

	if htmlBody := strings.TrimSpace(env.HTML); htmlBody != "" {
		body, err := html2text.FromString(htmlBody, html2text.Options{})
		if err != nil {
			log.Printf("Failed to convert HTML email body to text: %v", err)
		} else if body = strings.TrimSpace(body); body != "" {
			return body
		}
	}

	return EmptyBodyNotice
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
	for index, url := range orderedURLs {
		placeholder := fmt.Sprintf(placeholderFormat, index+1)
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
		for index := 1; index <= count; index++ {
			if strings.Contains(body, fmt.Sprintf(format, index)) {
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

func logSummary(message Message) string {
	return fmt.Sprintf(
		`from=%q to=%q subject=%q attachments=%d`,
		message.From,
		message.To,
		message.Subject,
		message.AttachmentCount,
	)
}

func detachedTimeoutContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}
