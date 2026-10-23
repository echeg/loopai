package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/umputun/ralphex/pkg/runs"
)

// acpLauncherWaitDelay bounds how long a finished launcher's output pipes may stay open. The detached
// child never holds them, since its output goes to its console log, so this only guards against a
// stray descendant.
const acpLauncherWaitDelay = 5 * time.Second

var (
	// acpLaunchDetached starts the detached run a loopai thread asks for and returns its id; the error
	// text is what the launcher said when it refused. TestMain replaces it, so no test starts the test
	// binary as a launcher by accident.
	acpLaunchDetached = launchACPRun
	// acpLauncherArgs are inserted between the executable and the launcher's arguments: nothing in
	// production, a helper test selector when tests run the launcher as the test binary.
	acpLauncherArgs []string
)

// acpLaunch is one detached launch a loopai thread asks for.
type acpLaunch struct {
	cwd        string            // the session cwd, where the launcher runs
	threadID   string            // the thread's T3 id; "" when the provider could not look it up
	sessionID  string            // the ACP session
	args       []string          // the run's arguments: the model flags and the plan
	envOptions map[string]string // the option variables a relaunch restores; nil for a new run
}

// launcherArgs is the launcher's command line after the executable: --detach, the thread and session
// that make the run this thread's, and the run's own arguments. --acp-thread is left out when the
// thread id is unknown.
func (l acpLaunch) launcherArgs() []string {
	args := []string{"--detach"}
	if l.threadID != "" {
		args = append(args, "--acp-thread="+l.threadID)
	}
	args = append(args, "--acp-session="+l.sessionID)
	return append(args, l.args...)
}

// launchACPRun runs "loopai --detach" in the session cwd for l and returns the run id it printed. The
// launcher gets this process's environment without what a detached run must never inherit, and with
// the option variables l records in place of its own, so a relaunch starts the run as it was started
// before. A launcher that fails returns what it printed on stderr, its refusal, as the error.
func launchACPRun(ctx context.Context, l acpLaunch) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve the loopai executable: %w", err)
	}
	cmd := exec.CommandContext(ctx, exe, append(slices.Clone(acpLauncherArgs), l.launcherArgs()...)...)
	cmd.Dir = l.cwd
	cmd.Env = acpLaunchEnv(os.Environ(), l.envOptions)
	cmd.WaitDelay = acpLauncherWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if runErr := cmd.Run(); runErr != nil {
		if refusal := strings.TrimSpace(stderr.String()); refusal != "" {
			return "", errors.New(refusal)
		}
		return "", fmt.Errorf("start the detached run: %w", runErr)
	}
	if id := launchedRunID(stdout.String()); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("the detached run launcher printed no run id: %q", strings.TrimSpace(stdout.String()))
}

// acpLaunchEnv is the launcher's environment: environ without the variables detachScrubbed names,
// with each option variable options holds replacing the one environ sets.
func acpLaunchEnv(environ []string, options map[string]string) []string {
	env := make([]string, 0, len(environ)+len(options))
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		if detachScrubbed(key) {
			continue
		}
		if _, recorded := options[key]; recorded && slices.Contains(cmuxEnvOptions, key) {
			continue
		}
		env = append(env, kv)
	}
	for _, key := range cmuxEnvOptions {
		if value, ok := options[key]; ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// launchedRunID returns the run id of the launcher's "run: <id>" line, or "" when it printed none.
func launchedRunID(stdout string) string {
	sc := bufio.NewScanner(strings.NewReader(stdout))
	for sc.Scan() {
		if id, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "run: "); ok && runs.ValidID(id) {
			return id
		}
	}
	return ""
}
