package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/umputun/ralphex/pkg/processor"
)

type reviewCheckpointStore struct {
	path string
}

func newReviewCheckpointStore(progressLogPath string) *reviewCheckpointStore {
	base := strings.TrimSuffix(filepath.Base(progressLogPath), ".txt")
	return &reviewCheckpointStore{
		path: filepath.Join(filepath.Dir(progressLogPath), base+".review.json"),
	}
}

func reviewCheckpointStoreForMode(mode processor.Mode, progressLogPath string) processor.ReviewCheckpointStore {
	if !modeUsesReviewCheckpoints(mode) {
		return nil
	}
	return newReviewCheckpointStore(progressLogPath)
}

func (s *reviewCheckpointStore) Load() (processor.ReviewCheckpoint, bool, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return processor.ReviewCheckpoint{}, false, nil
	}
	if err != nil {
		return processor.ReviewCheckpoint{}, false, fmt.Errorf("read review checkpoint: %w", err)
	}
	var checkpoint processor.ReviewCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return processor.ReviewCheckpoint{}, false, fmt.Errorf("%w: parse review checkpoint: %w", processor.ErrReviewCheckpointCorrupt, err)
	}
	return checkpoint, true, nil
}

func (s *reviewCheckpointStore) Save(checkpoint processor.ReviewCheckpoint) error {
	return saveJSONState(s.path, ".review-checkpoint-*.tmp", "review checkpoint", checkpoint)
}

func (s *reviewCheckpointStore) Remove() error {
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove review checkpoint: %w", err)
	}
	return nil
}
