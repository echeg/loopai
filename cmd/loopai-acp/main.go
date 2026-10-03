// Command loopai-acp lets T3 Code host loopai as a provider session through its Grok driver.
// It answers the Grok CLI probe commands the driver runs and, for the ACP session argv, starts
// "loopai --acp" with inherited stdio. The loopai CLI keeps its own argument grammar this way:
// positional "models" or "agent" never reach it.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
)

// loopaiEnv names an explicit loopai binary, taking precedence over a sibling and PATH.
const loopaiEnv = "LOOPAI_ACP_LOOPAI"

// exitUsage is the exit code for argv the launcher does not recognize.
const exitUsage = 2

const usage = `usage: loopai-acp --version
       loopai-acp models
       loopai-acp [--permission-mode MODE] agent [--always-approve] stdio

loopai-acp is a launcher for T3 Code's Grok driver: point a Grok provider instance's
binaryPath at it, and "agent stdio" starts "loopai --acp" serving plan runs over ACP.
`

var revision = "unknown"

func main() {
	os.Exit(run(os.Args[1:], defaultEnv()))
}

// env holds the launcher's process dependencies so tests can replace them.
type env struct {
	stdout, stderr io.Writer
	getenv         func(string) string
	executable     func() (string, error)
	lookPath       func(string) (string, error)
	stat           func(string) (os.FileInfo, error)
	launch         func(path string, args []string) (int, error)
}

func defaultEnv() env {
	return env{
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		getenv:     os.Getenv,
		executable: os.Executable,
		lookPath:   exec.LookPath,
		stat:       os.Stat,
		launch:     launchInherited,
	}
}

// run dispatches argv and returns the process exit code.
func run(args []string, e env) int {
	switch {
	case len(args) == 1 && (args[0] == "--version" || args[0] == "-v"):
		_, _ = fmt.Fprintf(e.stdout, "loopai-acp %s\n", resolveVersion())
		return 0
	case len(args) == 1 && args[0] == "models":
		// no login line: T3 Code reads "logged in" text as an auth verdict, and loopai's
		// providers authenticate on their own
		_, _ = fmt.Fprint(e.stdout, "Available models:\n  * loopai (default)\n")
		return 0
	case len(args) == 2 && args[0] == "inspect" && args[1] == "--json":
		_, _ = fmt.Fprintln(e.stderr, "loopai-acp: inspect is not supported")
		return 1
	case len(args) == 1 && args[0] == "update":
		_, _ = fmt.Fprintln(e.stderr, "loopai-acp: update is not supported; rebuild loopai from source")
		return 1
	case isStdioSession(args):
		bin, err := resolveLoopai(e)
		if err != nil {
			_, _ = fmt.Fprintf(e.stderr, "loopai-acp: %v\n", err)
			return 1
		}
		code, err := e.launch(bin, []string{"--acp"})
		if err != nil {
			_, _ = fmt.Fprintf(e.stderr, "loopai-acp: run %s: %v\n", bin, err)
			return 1
		}
		return code
	default:
		_, _ = fmt.Fprint(e.stderr, usage)
		return exitUsage
	}
}

// isStdioSession reports whether args is the Grok driver's ACP session argv:
// [--permission-mode MODE] agent [--always-approve] stdio.
func isStdioSession(args []string) bool {
	if len(args) > 0 && strings.HasPrefix(args[0], "--permission-mode=") {
		args = args[1:]
	} else if len(args) > 1 && args[0] == "--permission-mode" {
		args = args[2:]
	}
	if len(args) == 0 || args[0] != "agent" {
		return false
	}
	args = args[1:]
	if len(args) > 0 && args[0] == "--always-approve" {
		args = args[1:]
	}
	return len(args) == 1 && args[0] == "stdio"
}

// resolveLoopai finds the loopai binary: LOOPAI_ACP_LOOPAI, then a loopai beside the launcher,
// then PATH. The sibling is preferred over PATH so a built .bin pair stays consistent. On Windows
// only loopai.exe qualifies: process creation cannot start an extensionless file there.
func resolveLoopai(e env) (string, error) {
	if p := e.getenv(loopaiEnv); p != "" {
		return p, nil
	}
	name := "loopai"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if self, err := e.executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), name)
		if fi, statErr := e.stat(sibling); statErr == nil && fi.Mode().IsRegular() {
			return sibling, nil
		}
	}
	p, err := e.lookPath("loopai")
	if err != nil {
		return "", fmt.Errorf("loopai binary not found: set %s, place loopai beside loopai-acp, or add it to PATH: %w", loopaiEnv, err)
	}
	return p, nil
}

// launchInherited runs bin with the launcher's stdin, stdout, and stderr, forwards interrupt and
// termination signals to it, and returns its exit code. Where a signal cannot be forwarded (Windows),
// the child still stops: it shares the console's Ctrl+C and sees EOF when its stdin closes.
func launchInherited(bin string, args []string) (int, error) {
	cmd := exec.CommandContext(context.Background(), bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start: %w", err)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case sig := <-sigs:
				_ = cmd.Process.Signal(sig)
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		if code := exitErr.ExitCode(); code > 0 {
			return code, nil
		}
		return 1, nil // terminated by a signal
	}
	if err != nil {
		return 0, fmt.Errorf("wait: %w", err)
	}
	return 0, nil
}

// resolveVersion mirrors loopai's: ldflags revision, then module version, then VCS commit.
func resolveVersion() string {
	if revision != "unknown" {
		return revision
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return revision
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			return s.Value[:7]
		}
	}
	return revision
}
