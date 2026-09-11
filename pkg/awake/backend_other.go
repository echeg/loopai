//go:build !darwin && !linux && !windows

package awake

// platformBackend reports that this platform has no supported sleep inhibitor.
func platformBackend() Backend { return nil }
