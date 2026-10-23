// Command loopai-acp is the argument-free launcher of a loopai provider in T3 Code. Register its
// absolute path as the executable of a local ACP provider instance: whatever arguments it is given, it
// runs "loopai --acp", which serves the Agent Client Protocol on the inherited stdin and stdout.
//
// The loopai binary comes from LOOPAI_ACP_LOOPAI, else a loopai beside the launcher, else PATH. On
// Unix the launcher execs it in place, so the provider T3 Code started stays one stable process with
// one pid: T3 Code runs the provider as the leader of its own process group and signals that group
// as a whole (SIGTERM, then SIGKILL a second later), and loopai receives those signals itself with
// nothing in between. Windows has no exec, so there the launcher starts loopai with inherited stdio,
// forwards interrupts, and exits with its code.
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
	"syscall"
)

// loopaiEnv names an explicit loopai binary, taking precedence over a sibling and PATH.
const loopaiEnv = "LOOPAI_ACP_LOOPAI"

func main() {
	os.Exit(run(os.Args[1:], defaultEnv()))
}

// env holds the launcher's process dependencies so tests can replace them.
type env struct {
	stderr     io.Writer
	getenv     func(string) string
	executable func() (string, error)
	lookPath   func(string) (string, error)
	stat       func(string) (os.FileInfo, error)
	launch     func(path string, args []string) (int, error)
}

func defaultEnv() env {
	return env{
		stderr:     os.Stderr,
		getenv:     os.Getenv,
		executable: os.Executable,
		lookPath:   exec.LookPath,
		stat:       os.Stat,
		launch:     platformLaunch,
	}
}

// run launches "loopai --acp" and returns the process exit code. The arguments are ignored: a T3
// Code provider instance may pass whatever its configured arguments hold, and loopai reads nothing
// from them.
func run(_ []string, e env) int {
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

// launchInherited runs bin with the launcher's stdin, stdout, stderr, and environment, forwards
// interrupt and termination signals to it, and returns its exit code. It is the Windows launch;
// where a signal cannot be forwarded, the child still stops: it shares the console's Ctrl+C and
// sees EOF when its stdin closes.
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
