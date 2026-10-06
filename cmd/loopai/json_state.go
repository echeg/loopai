package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func saveJSONState(path, tempPattern, label string, value any) error {
	return writeFileAtomic(path, tempPattern, label, func(w io.Writer) error {
		return json.NewEncoder(w).Encode(value)
	})
}

// writeFileAtomic writes path through a mode 0600 temp file in the same directory and
// renames it into place, so a concurrent reader never sees a partial file.
func writeFileAtomic(path, tempPattern, label string, write func(io.Writer) error) error {
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
	writeErr := write(tmp)
	closeErr := tmp.Close()
	if writeErr != nil {
		return fmt.Errorf("write %s: %w", label, writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", label, closeErr)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", label, err)
	}
	return nil
}
