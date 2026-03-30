package core

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/130e/msgbot/internal/module/agent"
	"github.com/130e/msgbot/internal/module/mail"
	"github.com/130e/msgbot/internal/module/telegram"
	"github.com/130e/msgbot/internal/task/chat"
	"github.com/130e/msgbot/internal/task/forwardmail"
	"github.com/BurntSushi/toml"
)

const defaultShutdownTimeout = 10 * time.Second

type Config struct {
	Core    CoreConfig   `toml:"core"`
	Modules ModuleConfig `toml:"modules"`
	Tasks   TaskConfig   `toml:"tasks"`
}

type CoreConfig struct {
	ShutdownTimeout string `toml:"shutdown_timeout"`
}

type ModuleConfig struct {
	Mail     mail.Config     `toml:"mail"`
	Telegram telegram.Config `toml:"telegram"`
	Agent    agent.Config    `toml:"agent"`
}

type TaskConfig struct {
	ForwardMail forwardmail.Config `toml:"forwardmail"`
	Chat        chat.Config        `toml:"chat"`
}

func LoadConfig(path string) (Config, error) {
	resolvedPath, err := expandPath(path)
	if err != nil {
		return Config{}, err
	}

	var cfg Config
	metadata, err := toml.DecodeFile(resolvedPath, &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("decode config %q: %w", resolvedPath, err)
	}

	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		return Config{}, fmt.Errorf("config %q contains unknown keys: %s", resolvedPath, joinUndecodedKeys(undecoded))
	}

	return cfg, nil
}

func (c CoreConfig) ShutdownTimeoutDuration() (time.Duration, error) {
	value := strings.TrimSpace(c.ShutdownTimeout)
	if value == "" {
		return defaultShutdownTimeout, nil
	}

	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid core.shutdown_timeout %q: %w", value, err)
	}

	return duration, nil
}

func expandPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("config path is required")
	}

	if path == "~" || strings.HasPrefix(path, "~/") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}

		if path == "~" {
			path = homeDir
		} else {
			path = filepath.Join(homeDir, path[2:])
		}
	}

	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve config path %q: %w", path, err)
	}

	return absolutePath, nil
}

func joinUndecodedKeys(keys []toml.Key) string {
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, strings.Join(key, "."))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
