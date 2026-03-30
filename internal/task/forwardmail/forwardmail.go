package forwardmail

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/130e/msgbot/internal/module/agent"
	"github.com/130e/msgbot/internal/module/mail"
	"github.com/130e/msgbot/internal/module/telegram"
)

type Config struct {
	Enabled bool `toml:"enabled"`
}

type mailAgent interface {
	SummarizeMail(context.Context, string) (string, error)
}

type mailSender interface {
	SendMessage(context.Context, string) error
	LogFailure(error, string)
}

type task struct {
	agent    mailAgent
	telegram mailSender
}

func New(cfg Config, agentModule *agent.Module, telegramModule *telegram.Module) mail.Handler {
	_ = cfg

	t := task{
		agent:    agentModule,
		telegram: telegramModule,
	}

	return t.Handle
}

func (t task) Handle(ctx context.Context, message mail.Message) error {
	body := message.Body
	if body != mail.EmptyBodyNotice {
		summaryText, err := t.agent.SummarizeMail(ctx, body)
		if err != nil {
			log.Printf("Failed to summarize email %s: %v", messageSummary(message), err)
		} else {
			if len(message.URLPlaceholders) > 0 && !containsAnyPlaceholder(summaryText, message.URLPlaceholders) {
				log.Printf("Summarizer omitted all %d URL placeholders for email %s", len(message.URLPlaceholders), messageSummary(message))
			}
			body = summaryText
		}
	}

	outgoing := buildMessage(message, body)
	if err := t.telegram.SendMessage(ctx, outgoing); err != nil {
		t.telegram.LogFailure(err, messageSummary(message))
		return err
	}

	log.Printf("Processed email %s saved_to=%q", messageSummary(message), message.RawPath)
	return nil
}

func buildMessage(message mail.Message, body string) string {
	var builder strings.Builder
	builder.WriteString("From: ")
	builder.WriteString(telegram.RenderHTML(message.From, nil))
	builder.WriteString("\nTo: ")
	builder.WriteString(telegram.RenderHTML(message.To, nil))
	builder.WriteString("\nSubject: ")
	builder.WriteString(telegram.RenderHTML(message.Subject, nil))
	builder.WriteString("\n\n")
	builder.WriteString(telegram.RenderHTML(body, message.URLPlaceholders))
	return telegram.TruncateMessage(builder.String())
}

func messageSummary(message mail.Message) string {
	return fmt.Sprintf(
		`from=%q to=%q subject=%q attachments=%d`,
		message.From,
		message.To,
		message.Subject,
		message.AttachmentCount,
	)
}

func containsAnyPlaceholder(body string, placeholderToURL map[string]string) bool {
	for placeholder := range placeholderToURL {
		if strings.Contains(body, placeholder) {
			return true
		}
	}
	return false
}
