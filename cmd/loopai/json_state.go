package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/umputun/ralphex/pkg/runs"
)

func saveJSONState(path, label string, value any) error {
	return writeFileAtomic(path, label, func(w io.Writer) error {
		return json.NewEncoder(w).Encode(value)
	})
}

// writeFileAtomic renders the content through write, creates the directory of path when
// missing, and replaces path through runs.WriteFileAtomic with mode 0600, so a concurrent
// reader never sees a partial file.
func writeFileAtomic(path, label string, write func(io.Writer) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create %s directory: %w", label, err)
	}
	var buf bytes.Buffer
	if err := write(&buf); err != nil {
		return fmt.Errorf("write %s: %w", label, err)
	}
	if err := runs.WriteFileAtomic(path, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("replace %s: %w", label, err)
	}
	return nil
}
