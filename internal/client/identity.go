// Package client provides persistent client identity management.
package client

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

const clientIDFileName = "client-id"

// LoadOrCreateClientID returns a persistent client identifier.
// It reads the ID from ~/.monofs/client-id if it exists, otherwise
// generates a new UUID and persists it for future runs.
func LoadOrCreateClientID() (string, error) {
	dir, err := clientIDDir()
	if err != nil {
		return "", fmt.Errorf("determine config directory: %w", err)
	}

	idPath := filepath.Join(dir, clientIDFileName)

	data, err := os.ReadFile(idPath)
	if err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			return id, nil
		}
	}

	// Generate new UUID
	id := uuid.New().String()

	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create config directory: %w", err)
	}

	if err := os.WriteFile(idPath, []byte(id+"\n"), 0600); err != nil {
		return "", fmt.Errorf("write client ID: %w", err)
	}

	return id, nil
}

func clientIDDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".monofs"), nil
}
