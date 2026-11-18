package main

import (
	"fmt"
	"io"
)

// runDetachHop refuses on Windows, where detached runs are not supported.
func runDetachHop(_ []string, stderr io.Writer) int {
	fmt.Fprintln(stderr, "loopai:", errDetachUnsupported)
	return 1
}
