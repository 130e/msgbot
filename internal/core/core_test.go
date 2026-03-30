package core

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/130e/msgbot/internal/module"
	"github.com/130e/msgbot/internal/module/agent"
	"github.com/130e/msgbot/internal/module/telegram"
	"github.com/130e/msgbot/internal/task/chat"
	"github.com/130e/msgbot/internal/task/forwardmail"
)

type fakeModule struct {
	upCalls   int
	downCalls int
	upErr     error
	downErr   error
}

func (m *fakeModule) Up(context.Context) error {
	m.upCalls++
	return m.upErr
}

func (m *fakeModule) Down(context.Context) error {
	m.downCalls++
	return m.downErr
}

func TestNewRejectsNoTasks(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Fatal("New() error = nil, want no tasks enabled error")
	}
}

func TestNewChatOnlyDoesNotCreateMailModule(t *testing.T) {
	app, err := New(Config{
		Modules: ModuleConfig{
			Agent:    agent.Config{Type: agent.TypePassthrough},
			Telegram: telegram.Config{BotToken: "token"},
		},
		Tasks: TaskConfig{
			Chat: chat.Config{Enabled: true},
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if app.mail != nil {
		t.Fatal("app.mail != nil, want nil for chat-only config")
	}
	if app.mailErrors != nil {
		t.Fatal("app.mailErrors != nil, want nil for chat-only config")
	}
	if app.telegramErrors == nil {
		t.Fatal("app.telegramErrors = nil, want telegram runtime errors channel")
	}
	if len(app.modules) != 2 {
		t.Fatalf("len(app.modules) = %d, want 2", len(app.modules))
	}
}

func TestNewRequiresTelegramChatIDForForwardMail(t *testing.T) {
	_, err := New(Config{
		Modules: ModuleConfig{
			Agent:    agent.Config{Type: agent.TypePassthrough},
			Telegram: telegram.Config{BotToken: "token"},
		},
		Tasks: TaskConfig{
			ForwardMail: forwardmail.Config{Enabled: true},
		},
	})
	if err == nil {
		t.Fatal("New() error = nil, want missing modules.telegram.chat_id error")
	}
}

func TestNewAllowsBothTasks(t *testing.T) {
	app, err := New(Config{
		Modules: ModuleConfig{
			Agent:    agent.Config{Type: agent.TypePassthrough},
			Telegram: telegram.Config{BotToken: "token", ChatID: "-100123"},
		},
		Tasks: TaskConfig{
			ForwardMail: forwardmail.Config{Enabled: true},
			Chat:        chat.Config{Enabled: true},
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if app.mail == nil {
		t.Fatal("app.mail = nil, want mail module when forwardmail is enabled")
	}
	if app.mailErrors == nil {
		t.Fatal("app.mailErrors = nil, want mail errors channel")
	}
	if app.telegramErrors == nil {
		t.Fatal("app.telegramErrors = nil, want telegram errors channel when chat is enabled")
	}
	if len(app.modules) != 3 {
		t.Fatalf("len(app.modules) = %d, want 3", len(app.modules))
	}
}

func TestNewRejectsInvalidChatConfig(t *testing.T) {
	_, err := New(Config{
		Modules: ModuleConfig{
			Agent:    agent.Config{Type: agent.TypePassthrough},
			Telegram: telegram.Config{BotToken: "token"},
		},
		Tasks: TaskConfig{
			Chat: chat.Config{
				Enabled:       true,
				ContextWindow: "nope",
			},
		},
	})
	if err == nil {
		t.Fatal("New() error = nil, want invalid chat config error")
	}
}

func TestRunLogsModuleRuntimeErrorsAndContinuesUntilCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	moduleA := &fakeModule{}
	telegramErrors := make(chan error, 1)

	app := &App{
		shutdownTimeout: time.Second,
		telegramErrors:  telegramErrors,
		modules:         []module.Module{moduleA},
	}

	originalWriter := log.Writer()
	logBuffer := &strings.Builder{}
	log.SetOutput(logBuffer)
	defer log.SetOutput(originalWriter)

	runDone := make(chan error, 1)
	go func() {
		runDone <- app.Run(ctx)
	}()

	telegramErrors <- errors.New("transient telegram failure")

	select {
	case err := <-runDone:
		t.Fatalf("Run() returned early with %v, want it to keep running", err)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil after context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}

	if moduleA.upCalls != 1 {
		t.Fatalf("module Up() calls = %d, want 1", moduleA.upCalls)
	}
	if moduleA.downCalls != 1 {
		t.Fatalf("module Down() calls = %d, want 1", moduleA.downCalls)
	}
	if !strings.Contains(logBuffer.String(), "Telegram module runtime error: transient telegram failure") {
		t.Fatalf("log output = %q, want runtime error entry", logBuffer.String())
	}
}

func TestRunDisablesClosedModuleErrorChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	moduleA := &fakeModule{}
	telegramErrors := make(chan error)
	close(telegramErrors)

	app := &App{
		shutdownTimeout: time.Second,
		telegramErrors:  telegramErrors,
		modules:         []module.Module{moduleA},
	}

	originalWriter := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(originalWriter)

	runDone := make(chan error, 1)
	go func() {
		runDone <- app.Run(ctx)
	}()

	select {
	case err := <-runDone:
		t.Fatalf("Run() returned early with %v, want it to continue after disabling the closed channel", err)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil after context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}
}
