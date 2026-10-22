//go:build windows

package main

import (
	"context"
	"errors"
)

// errDetachUnsupported is what every detached-run entry point returns on Windows.
var errDetachUnsupported = errors.New("detached runs are not supported on Windows")

// runDetachedChild refuses: detached runs are not supported on Windows.
func runDetachedChild(context.Context, opts) error {
	return errDetachUnsupported
}
