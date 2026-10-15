package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/ronsor/sippycup/libsippycup"
)

// serveConfig publishes file-backed configurations for new calls. Rejected
// reloads leave the current configuration, transports, and active calls intact.
func serveConfig(ctx context.Context, path string, config *libsippycup.Config, reload <-chan os.Signal) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	engine := libsippycup.NewEngine(config)
	if err := engine.Start(); err != nil {
		return err
	}

	defer func() { _ = engine.Close() }()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-engine.Done():
			return fmt.Errorf("gateway stopped unexpectedly")
		case _, ok := <-reload:
			if !ok {
				reload = nil

				continue
			}

			next, err := LoadConfig(path)
			if err != nil {
				slog.Error("configuration reload rejected; keeping current configuration", "error", err)

				continue
			}

			if ctx.Err() != nil {
				return nil
			}

			if err := engine.Reload(next); err != nil {
				slog.Error("configuration reload rejected; keeping current configuration", "error", err)

				continue
			}

			slog.Info("configuration reloaded", "config", path)
		}
	}
}
