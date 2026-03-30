package core

import (
	"context"
	"log"
)

func Run(ctx context.Context, configPath string) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}

	log.Printf("Loaded config from %q", configPath)

	app, err := New(cfg)
	if err != nil {
		return err
	}

	return app.Run(ctx)
}
