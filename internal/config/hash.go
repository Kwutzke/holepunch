package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// HashFile returns the SHA256 hash of the config file at the given path.
func HashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading config for hash: %w", err)
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]), nil
}

// SetupHashPath returns the path where the setup config hash is stored.
func SetupHashPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".holepunch", "setup.hash")
}

// SaveSetupHash writes the current config hash to disk.
func SaveSetupHash(configPath string) error {
	hash, err := HashFile(configPath)
	if err != nil {
		return err
	}
	hashPath := SetupHashPath()
	if err := os.MkdirAll(filepath.Dir(hashPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(hashPath, []byte(hash), 0o644)
}

// SetupStale returns true if the config has changed since the last setup.
// Returns false if no hash file exists (setup was never run).
func SetupStale(configPath string) bool {
	savedHash, err := os.ReadFile(SetupHashPath())
	if err != nil {
		return false // no hash = setup never run, let it fail naturally
	}
	currentHash, err := HashFile(configPath)
	if err != nil {
		return false
	}
	return string(savedHash) != currentHash
}
