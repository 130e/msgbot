package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/130e/msgbot/internal/core"
)

const defaultConfigPath = "~/.config/msgbot/config.toml"

func main() {
	configPath := flag.String("config", defaultConfigPath, "Path to msgbot TOML config")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := core.Run(ctx, *configPath); err != nil {
		log.Fatalf("msgbot failed: %v", err)
	}
}
