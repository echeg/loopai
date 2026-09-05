package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/umputun/ralphex/pkg/processor"
)

const reviewCheckpointStateVersion = 1

type reviewCheckpointStore struct {
	path string
}

func newReviewCheckpointStore(progressLogPath string) *reviewCheckpointStore {
	base := strings.TrimSuffix(filepath.Base(progressLogPath), ".txt")
	return &reviewCheckpointStore{
		path: filepath.Join(filepath.Dir(progressLogPath), base+".review.json"),
	}
}

func (s *reviewCheckpointStore) Load() (processor.ReviewCheckpoint, bool, error) {
	data, err := os.ReadFile(s.path) //nolint:gosec // path is derived from loopai's progress log path
	if os.IsNotExist(err) {
		return processor.ReviewCheckpoint{}, false, nil
	}
	if err != nil {
		return processor.ReviewCheckpoint{}, false, fmt.Errorf("read review checkpoint: %w", err)
	}
	var checkpoint processor.ReviewCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return processor.ReviewCheckpoint{}, false, fmt.Errorf("parse review checkpoint: %w", err)
	}
	return checkpoint, true, nil
}

func (s *reviewCheckpointStore) Save(checkpoint processor.ReviewCheckpoint) error {
	checkpoint.Version = reviewCheckpointStateVersion
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create review checkpoint directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".review-checkpoint-*.tmp")
	if err != nil {
		return fmt.Errorf("create review checkpoint: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup after atomic replacement
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure review checkpoint: %w", err)
	}
	encodeErr := json.NewEncoder(tmp).Encode(checkpoint)
	closeErr := tmp.Close()
	if encodeErr != nil {
		return fmt.Errorf("write review checkpoint: %w", encodeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close review checkpoint: %w", closeErr)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("replace review checkpoint: %w", err)
	}
	return nil
}

func (s *reviewCheckpointStore) Remove() error {
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove review checkpoint: %w", err)
	}
	return nil
}
