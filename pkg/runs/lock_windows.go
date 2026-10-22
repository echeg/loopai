//go:build windows

package runs

import "time"

// AcquireBounded is not supported on Windows, where detached runs are refused.
func AcquireBounded(string, time.Duration) (func() error, error) {
	return nil, ErrUnsupported
}

// Probe is not supported on Windows, where detached runs are refused.
func Probe(string) (held bool, err error) {
	return false, ErrUnsupported
}
