//go:build windows

package main

// signalTerm refuses: detached runs are not supported on Windows.
func signalTerm(int) error {
	return errDetachUnsupported
}
