//go:build !darwin && !linux

package runs

// processStartToken is not supported outside macOS and Linux.
func processStartToken(int) (string, error) {
	return "", ErrUnsupported
}
