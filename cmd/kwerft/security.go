package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	"github.com/ehilzinger/kwerft/internal/auth"
)

// dataKey returns the key that encrypts secrets in the database (TOTP seeds).
// Production reads KWERFT_DATA_KEY, which the Helm chart fills from the
// kwerft-data-key Secret. --dev falls back to a key kept beside the database.
func dataKey(log *slog.Logger, dev bool, dataDir string) ([]byte, error) {
	if v := os.Getenv("KWERFT_DATA_KEY"); v != "" {
		key, err := auth.ParseDataKey(v)
		if err != nil {
			return nil, fmt.Errorf("KWERFT_DATA_KEY: %w; generate one with `openssl rand -base64 32`", err)
		}
		return key, nil
	}
	if !dev {
		return nil, errors.New("KWERFT_DATA_KEY is not set; it encrypts secrets in the database. " +
			"The Helm chart sets it from the Secret kwerft-data-key; for a manual run, generate one with `openssl rand -base64 32`")
	}
	path := filepath.Join(dataDir, "dev-data-key")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		b = []byte(auth.NewDataKey())
		err = os.WriteFile(path, b, 0o600)
	}
	if err != nil {
		return nil, fmt.Errorf("development data key %s: %w", path, err)
	}
	log.Warn("development mode: data key read from a file next to the database; set KWERFT_DATA_KEY in production", "path", path)
	return auth.ParseDataKey(string(b))
}

// devPasskeyOrigins are the origins a browser reports in --dev: the console
// itself and the Vite dev server, both on localhost (the relying party ID).
func devPasskeyOrigins(listen string) []string {
	origins := []string{"http://localhost:5173"}
	if _, port, err := net.SplitHostPort(listen); err == nil && port != "" {
		origins = append([]string{"http://localhost:" + port}, origins...)
	}
	return origins
}
