package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func saveJSONState(path, tempPattern, label string, value any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create %s directory: %w", label, err)
	}
	tmp, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return fmt.Errorf("create %s: %w", label, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup after atomic replacement
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure %s: %w", label, err)
	}
	encodeErr := json.NewEncoder(tmp).Encode(value)
	closeErr := tmp.Close()
	if encodeErr != nil {
		return fmt.Errorf("write %s: %w", label, encodeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", label, closeErr)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", label, err)
	}
	return nil
}
