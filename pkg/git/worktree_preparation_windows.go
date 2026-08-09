//go:build windows

package git

// Windows does not expose a supported equivalent of POSIX fsync for a directory handle. The
// marker file itself is flushed before close; namespace updates use the strongest portable
// guarantee available on Windows.
func syncDirectory(string) error {
	return nil
}
