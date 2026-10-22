//go:build windows

package runs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLocksUnsupported(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFile)

	_, err := AcquireBounded(path, time.Second)
	require.ErrorIs(t, err, ErrUnsupported)
	_, err = Probe(path)
	require.ErrorIs(t, err, ErrUnsupported)
}
