package git

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const worktreePreparationMarkerPrefix = "loopai-worktree-prepare-"

// HasWorktreePreparationMarker reports whether a prior process left durable evidence that
// initialization of path had started but had not completed. Callers serialize this check with
// worktree creation through AcquireWorktreeCreationLock.
func (s *Service) HasWorktreePreparationMarker(path string) (bool, error) {
	markerPath, err := s.worktreePreparationMarkerPath(path)
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(markerPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect worktree preparation marker: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("worktree preparation marker %s is not a regular file", markerPath)
	}
	return true, nil
}

// MarkWorktreePreparation durably records that initialization of path is in progress. The marker
// must be created before the worktree path can appear and cleared only after the run lock is held.
func (s *Service) MarkWorktreePreparation(path string) error {
	markerPath, err := s.worktreePreparationMarkerPath(path)
	if err != nil {
		return err
	}
	marker, err := os.OpenFile(markerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // path is inside Git metadata
	if err != nil {
		return fmt.Errorf("create worktree preparation marker: %w", err)
	}
	removeOnError := true
	defer func() {
		if removeOnError {
			_ = os.Remove(markerPath)
		}
	}()
	if _, err := marker.WriteString(filepath.Clean(path) + "\n"); err != nil {
		_ = marker.Close()
		return fmt.Errorf("write worktree preparation marker: %w", err)
	}
	if err := marker.Sync(); err != nil {
		_ = marker.Close()
		return fmt.Errorf("sync worktree preparation marker: %w", err)
	}
	if err := marker.Close(); err != nil {
		return fmt.Errorf("close worktree preparation marker: %w", err)
	}
	removeOnError = false
	return nil
}

// ClearWorktreePreparation removes the durable initialization marker for path.
func (s *Service) ClearWorktreePreparation(path string) error {
	markerPath, err := s.worktreePreparationMarkerPath(path)
	if err != nil {
		return err
	}
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove worktree preparation marker: %w", err)
	}
	return nil
}

func (s *Service) worktreePreparationMarkerPath(path string) (string, error) {
	commonDir, err := s.repo.gitCommonDir()
	if err != nil {
		return "", fmt.Errorf("locate worktree preparation marker directory: %w", err)
	}
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	name := worktreePreparationMarkerPrefix + hex.EncodeToString(sum[:]) + ".marker"
	markerPath := filepath.Join(commonDir, name)
	if filepath.Dir(markerPath) != filepath.Clean(commonDir) {
		return "", errors.New("worktree preparation marker escaped Git metadata directory")
	}
	return markerPath, nil
}
