package runs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// RequestStop writes the stop marker into run directory dir. A stop writes it before
// signaling the run, so a run killed before it records an outcome classifies as stopped
// rather than crashed. The marker holds the request time for diagnostics only.
func RequestStop(dir string) error {
	stamp := []byte(time.Now().UTC().Format(time.RFC3339) + "\n")
	if err := WriteFileAtomic(filepath.Join(dir, StopFile), stamp, 0o600); err != nil {
		return fmt.Errorf("write stop marker: %w", err)
	}
	return nil
}

// StopRequested reports whether the stop marker exists in run directory dir.
func StopRequested(dir string) (bool, error) {
	_, err := os.Lstat(filepath.Join(dir, StopFile))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("check stop marker: %w", err)
}

// ClearStop removes the stop marker from run directory dir before a resumed attempt starts.
// A missing marker is not an error.
func ClearStop(dir string) error {
	if err := os.Remove(filepath.Join(dir, StopFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clear stop marker: %w", err)
	}
	return nil
}
