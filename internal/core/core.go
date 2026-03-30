package core

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/130e/msgbot/internal/module"
	"github.com/130e/msgbot/internal/module/agent"
	"github.com/130e/msgbot/internal/module/mail"
	"github.com/130e/msgbot/internal/module/telegram"
	"github.com/130e/msgbot/internal/task/chat"
	"github.com/130e/msgbot/internal/task/forwardmail"
)

type App struct {
	shutdownTimeout time.Duration
	telegram        *telegram.Module
	mail            *mail.Module
	telegramErrors  <-chan error
	mailErrors      <-chan error
	modules         []module.Module
	started         []module.Module
}

func New(cfg Config) (*App, error) {
	shutdownTimeout, err := cfg.Core.ShutdownTimeoutDuration()
	if err != nil {
		return nil, err
	}
	if !cfg.Tasks.ForwardMail.Enabled && !cfg.Tasks.Chat.Enabled {
		return nil, fmt.Errorf("no tasks enabled")
	}

	agentModule := agent.New(cfg.Modules.Agent)
	telegramModule := telegram.New(cfg.Modules.Telegram)
	modules := []module.Module{agentModule, telegramModule}
	var mailModule *mail.Module
	var telegramErrors <-chan error
	var mailErrors <-chan error

	if cfg.Tasks.ForwardMail.Enabled {
		if strings.TrimSpace(cfg.Modules.Telegram.ChatID) == "" {
			return nil, fmt.Errorf("modules.telegram.chat_id is required when tasks.forwardmail.enabled=true")
		}

		telegramModule.RequireConfiguredChat()
		mailModule = mail.New(cfg.Modules.Mail)
		mailModule.SetHandler(forwardmail.New(cfg.Tasks.ForwardMail, agentModule, telegramModule))
		mailErrors = mailModule.Errors()
		modules = append(modules, mailModule)
		log.Println("Mail forwarder enabled")
	}
	if cfg.Tasks.Chat.Enabled {
		handler, err := chat.New(cfg.Tasks.Chat, agentModule, telegramModule)
		if err != nil {
			return nil, err
		}
		telegramModule.SetDefaultHandler(handler)
		telegramErrors = telegramModule.Errors()
		log.Println("Chat enabled")
	}

	return &App{
		shutdownTimeout: shutdownTimeout,
		telegram:        telegramModule,
		mail:            mailModule,
		telegramErrors:  telegramErrors,
		mailErrors:      mailErrors,
		modules:         modules,
	}, nil
}

func (a *App) Run(ctx context.Context) error {
	if err := a.start(ctx); err != nil {
		return err
	}

	telegramErrors := a.telegramErrors
	mailErrors := a.mailErrors

	for {
		select {
		case err, ok := <-telegramErrors:
			if !ok {
				log.Printf("Telegram module error channel closed; disabling runtime error monitoring")
				telegramErrors = nil
				continue
			}
			if err == nil {
				log.Printf("Telegram module reported a nil runtime error")
				continue
			}
			log.Printf("Telegram module runtime error: %v", err)
		case err, ok := <-mailErrors:
			if !ok {
				log.Printf("Mail module error channel closed; disabling runtime error monitoring")
				mailErrors = nil
				continue
			}
			if err == nil {
				log.Printf("Mail module reported a nil runtime error")
				continue
			}
			log.Printf("Mail module runtime error: %v", err)
		case <-ctx.Done():
			log.Printf("Shutting down msgbot")
			return a.shutdown()
		}
	}
}

func (a *App) start(ctx context.Context) error {
	a.started = a.started[:0]
	for _, current := range a.modules {
		log.Printf("Core boot: starting module=%T", current)
		if err := current.Up(ctx); err != nil {
			return errors.Join(err, a.shutdown())
		}
		a.started = append(a.started, current)
		log.Printf("Core boot: started module=%T", current)
	}
	return nil
}

func (a *App) shutdown() error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.shutdownTimeout)
	defer cancel()

	var shutdownErr error
	for index := len(a.started) - 1; index >= 0; index-- {
		log.Printf("Core shutdown: stopping module=%T", a.started[index])
		if err := a.started[index].Down(shutdownCtx); err != nil {
			shutdownErr = errors.Join(shutdownErr, err)
		}
	}

	a.started = nil
	return shutdownErr
}
