package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/progress"
	"github.com/umputun/ralphex/pkg/runs"
)

const (
	// detachLaunchLockTimeout bounds the wait for another launcher of the same plan and branch.
	detachLaunchLockTimeout = 5 * time.Second
	// detachKeepSucceeded is how many succeeded run directories a new run keeps when it prunes.
	detachKeepSucceeded = 20
	// detachMaxNewIDs bounds the run ids a new run draws when the directory of one already exists.
	detachMaxNewIDs = 8
	// detachConsoleTailLines is how much of the console log a launch that failed shows.
	detachConsoleTailLines = 20
	// maxDetachConsoleTail caps the bytes of the console log read for that tail.
	maxDetachConsoleTail = 64 << 10
	// maxDetachAckLine caps the child's answer line.
	maxDetachAckLine = 64 << 10
)

var (
	// detachExecutable resolves the binary the launcher starts as the detached child.
	detachExecutable = os.Executable
	// detachChildArgs are inserted between the executable and the child's own arguments: nothing in
	// production, a helper test selector when tests point detachExecutable at the test binary.
	detachChildArgs []string
	// detachScopePrefix returns the command the child is started through, or nil to start it directly.
	detachScopePrefix = defaultDetachScopePrefix
	// detachAckTimeout bounds the wait for the child's answer.
	detachAckTimeout = 120 * time.Second
	// newRunID draws the id of a new run; tests replace it to collide with an existing directory.
	newRunID = runs.NewID
)

// detachValueFlags are the options that only route a launch and never reach a recorded run, removed
// by stripDetachArgs together with their value.
var detachValueFlags = []string{"--detached-run", "--t3-thread", "--acp-thread", "--acp-session"}

// t3ThreadIDPattern and maxT3ThreadIDLen are the shape of a T3 thread id loopai accepts,
// ^[A-Za-z0-9_.:%-]{1,1024}$; Go's regexp caps a repeat count at 1000, so the length is checked on
// its own. T3 treats the id as an opaque string: a UUID, or a provider-native id such as
// thread:provider:claudeAgent:native-thread:thread%3A… of some 330 characters. The set leaves out
// everything a shell splits or expands.
var t3ThreadIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:%-]+$`)

const maxT3ThreadIDLen = 1024

// validateDetachFlags keeps a detached run to what it supports: exactly one plan, in full or
// tasks-only mode, executed in a worktree of its own and never committing into the invoking checkout.
// It applies to the --detach launcher and to the hidden --detached-run child, which receives the
// launcher's arguments, so the child refuses what the launcher would.
func validateDetachFlags(o opts) error {
	childSet := o.detachedRunSet || o.DetachedRun != ""
	if err := validateThreadFlags(o, o.Detach || childSet); err != nil {
		return err
	}
	if !o.Detach && !childSet {
		return nil
	}
	// the child is routed before the launcher, so the pair would run as a child no launcher started
	if o.Detach && childSet {
		return errors.New("--detach cannot be combined with --detached-run: the launcher starts the detached run itself")
	}
	if o.detachedRunSet && strings.TrimSpace(o.DetachedRun) == "" {
		return errors.New("--detached-run requires a run id")
	}
	flag := "--detach"
	if !o.Detach {
		flag = "--detached-run"
	}
	for _, conflict := range []struct {
		flag string
		set  bool
	}{
		{"--review", o.Review},
		{"--external-only", o.ExternalOnly},
		{"--plan", o.PlanDescription != ""},
		{"--gen-agents", o.GenAgents},
		{"--serve", o.Serve},
		{"--watch", len(o.Watch) > 0},
		{"--cmux-workspace", o.CmuxWorkspace != ""},
		{"--init", o.Init},
		{"--reset", o.Reset},
		{"--dump-defaults", o.DumpDefaults != ""},
		{"--clear", o.Clear},
		{"--merge", mergeRequested(o)},
		{"--pr", prRequested(o)},
		{"--report", reportRequested(o)},
	} {
		if conflict.set {
			return fmt.Errorf("%s cannot be combined with %s", flag, conflict.flag)
		}
	}
	if o.Commit {
		return fmt.Errorf("%s cannot be combined with --commit: a detached run never commits into the invoking checkout", flag)
	}
	if len(o.PlanFiles) > 1 {
		return errors.New("a detached run takes exactly one plan; plan chains are not supported")
	}
	if len(o.extraArgs) > 0 {
		return fmt.Errorf("%s takes exactly one plan argument, got %d", flag, len(o.extraArgs)+1)
	}
	if strings.TrimSpace(o.PlanFile) == "" {
		return fmt.Errorf("%s requires a plan file argument", flag)
	}
	return nil
}

// validateThreadFlags accepts the flags that name the T3 thread a run belongs to only on a detached
// run, detached reporting whether the invocation is one, and only with an id t3ThreadIDPattern
// matches: --t3-thread, a terminal in an existing thread, and --acp-thread and --acp-session, which
// the provider of a loopai thread passes and which cannot name a terminal thread as well.
func validateThreadFlags(o opts, detached bool) error {
	t3Set := o.t3ThreadSet || o.T3Thread != ""
	acpSet := o.acpThreadSet || o.ACPThread != "" || o.acpSessionSet || o.ACPSession != ""
	if t3Set && acpSet {
		return errors.New("--t3-thread cannot be combined with --acp-thread or --acp-session")
	}
	for _, flag := range []struct {
		name, kind, value string
		set               bool
	}{
		{"--t3-thread", "thread id", o.T3Thread, o.t3ThreadSet}, {"--acp-thread", "thread id", o.ACPThread, o.acpThreadSet},
		{"--acp-session", "session id", o.ACPSession, o.acpSessionSet},
	} {
		if !flag.set && flag.value == "" {
			continue
		}
		if !detached {
			return fmt.Errorf("%s is valid only with --detach", flag.name)
		}
		if len(flag.value) > maxT3ThreadIDLen || !t3ThreadIDPattern.MatchString(flag.value) {
			return fmt.Errorf("%s: invalid %s %q: expected 1 to 1024 letters, digits, and _.:%%- characters",
				flag.name, flag.kind, flag.value)
		}
	}
	return nil
}

// stripDetachArgs returns the launcher's arguments as a run records and relaunches them: without
// --detach, and without each option of detachValueFlags in both its --flag=value form and its
// --flag value form, whose next argument is the value. Scanning stops at --, which is kept with
// everything after it. Every other argument is kept as given, explicit empty values such as
// --external-reviewers= included.
func stripDetachArgs(args []string) []string {
	kept := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, _, assigned := strings.Cut(arg, "=")
		switch {
		case arg == "--":
			return append(kept, args[i:]...)
		case arg == "--detach":
			// dropped: the child is the detached run
		case slices.Contains(detachValueFlags, name):
			if !assigned {
				i++ // the value is the next argument
			}
		default:
			kept = append(kept, arg)
		}
	}
	return kept
}

// refuseDetachInSandbox refuses a --detach launch inside the agent sandbox detectSandbox finds in
// getenv. It runs before config loading, whose defaults installer writes under HOME: a sandbox denies
// that write, and its error would otherwise replace this guidance, meant for exactly that environment.
func refuseDetachInSandbox(getenv func(string) string) error {
	signal := detectSandbox(getenv)
	if signal == "" {
		return nil
	}
	return fmt.Errorf("refusing to start a detached run inside an agent sandbox (%s): the run and every model "+
		"process it starts would inherit the sandbox; start the plan in a new loopai thread (Mode 2 of the "+
		"loopai-t3 skill) or from a shell outside the sandbox", signal)
}

// detectSandbox names the agent sandbox signal getenv reports, or returns "" outside one: Codex sets
// CODEX_SANDBOX (seatbelt on macOS) and CODEX_SANDBOX_NETWORK_DISABLED=1, Claude Code SANDBOX_RUNTIME=1.
func detectSandbox(getenv func(string) string) string {
	if value := getenv("CODEX_SANDBOX"); value != "" {
		return "CODEX_SANDBOX=" + value
	}
	if getenv("CODEX_SANDBOX_NETWORK_DISABLED") == "1" {
		return "CODEX_SANDBOX_NETWORK_DISABLED=1"
	}
	if getenv("SANDBOX_RUNTIME") == "1" {
		return "SANDBOX_RUNTIME=1"
	}
	return ""
}

// systemdScopePrefix returns the command that starts the child in a transient systemd user scope,
// "systemd-run --user --scope --quiet --collect --", on Linux when systemd-run is on PATH and the user
// manager's private socket exists, and nil otherwise. Without the scope a child started from a
// service's cgroup, such as a T3 provider's slice, is stopped with that cgroup. systemd-run execs the
// command in place, so the handshake descriptors on fds 3 and 4 stay open across it.
func systemdScopePrefix(goos string, lookPath func(string) (string, error), stat func(string) (os.FileInfo, error),
	getenv func(string) string) []string {
	if goos != "linux" {
		return nil
	}
	bin, err := lookPath("systemd-run")
	if err != nil {
		return nil
	}
	runtimeDir := getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		return nil
	}
	if _, err := stat(filepath.Join(runtimeDir, "systemd", "private")); err != nil {
		return nil
	}
	return []string{bin, "--user", "--scope", "--quiet", "--collect", "--"}
}

// defaultDetachScopePrefix is systemdScopePrefix for this process.
func defaultDetachScopePrefix() []string {
	return systemdScopePrefix(runtime.GOOS, exec.LookPath, os.Stat, os.Getenv)
}

// detachScrubbed reports whether the environment variable key must never reach a detached child: a T3
// provider's MCP endpoint and credential, a T3 token, which travels over the handshake pipe only, and
// cmux's workspace, which the run, reporting to no sidebar card, would otherwise address.
func detachScrubbed(key string) bool {
	return strings.HasPrefix(key, "T3_ACP_MCP_") || key == "LOOPAI_T3_TOKEN" || strings.HasPrefix(key, "CMUX_")
}

// detachChildEnv returns the child's environment: environ without the scrubbed variables, plus each
// option variable recorded holds and environ does not set, so a run resumed from a shell that lacks
// one keeps the value it was started with while an explicit value wins.
func detachChildEnv(environ []string, recorded map[string]string) []string {
	env := make([]string, 0, len(environ)+len(recorded))
	set := make(map[string]bool, len(environ))
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		if detachScrubbed(key) {
			continue
		}
		set[key] = true
		env = append(env, kv)
	}
	for _, key := range cmuxEnvOptions {
		if value, ok := recorded[key]; ok && !set[key] {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// detachEnvOptions returns the option variables env sets, the environment-provided options a
// descriptor records; nil when env sets none.
func detachEnvOptions(env []string) map[string]string {
	var options map[string]string
	for _, kv := range env {
		key, value, _ := strings.Cut(kv, "=")
		if !slices.Contains(cmuxEnvOptions, key) {
			continue
		}
		if options == nil {
			options = map[string]string{}
		}
		options[key] = value
	}
	return options
}

// detachChildArgv is the child's command line: the scope prefix, the executable, detachChildArgs,
// --detached-run=<id>, and the run's recorded arguments.
func detachChildArgv(prefix []string, exe, id string, argv []string) []string {
	cmd := slices.Clone(prefix)
	cmd = append(cmd, exe)
	cmd = append(cmd, detachChildArgs...)
	cmd = append(cmd, "--detached-run="+id)
	return append(cmd, argv...)
}

// detachIO is the launcher's surface: its own arguments, which the run records and relaunches, where
// it reports, and the colors of the repository's log lines.
type detachIO struct {
	args           []string // the launcher's arguments, os.Args[1:] in production
	stdout, stderr io.Writer
	colors         *progress.Colors
}

// detachLaunch is one --detach invocation: the run it decided on and the child it starts for it.
type detachLaunch struct {
	o       opts
	dio     detachIO
	svc     *git.Service // the invoking checkout's repository
	exe     string       // the binary started as the child
	root    string       // the invoking checkout
	planRel string       // the plan relative to root, as the descriptor records it
	branch  string       // the run's branch
	dir     string       // the run directory
	desc    runs.Descriptor
	env     []string // the child's environment
	token   string   // the T3 token a --t3-thread run gets over the secret pipe; never written anywhere
}

// launchDetached is the --detach launcher, which runPreConfigCommand has already refused inside an
// agent sandbox. It prepares the repository, decides the run under the launch lock of its plan and
// branch, starts the detached child, and returns once the child answers: nil after printing the run id
// and the follow command when the attempt started, an error otherwise. The child is never killed: a
// launcher that stops waiting leaves it to start on its own.
func launchDetached(ctx context.Context, o opts, cfg *config.Config, dio detachIO) error {
	l, err := prepareDetachLaunch(o, cfg, dio)
	if err != nil {
		return err
	}
	release, err := runs.AcquireLaunchLock(l.root, l.planRel, l.branch, detachLaunchLockTimeout)
	if err != nil {
		return fmt.Errorf("start a detached run: %w", err)
	}
	// released after the child answered: by then it holds its lifetime lock, so the run never reads as
	// crashed between the two
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			fmt.Fprintf(dio.stderr, "warning: release the launch lock: %v\n", releaseErr)
		}
	}()
	if decideErr := l.decide(time.Now()); decideErr != nil {
		return decideErr
	}
	return l.start(ctx)
}

// prepareDetachLaunch runs the checks that need no lock, in order: a plan the child can read, an
// executable it can run, the repository opened with vcs_command from its root, and the .loopai ignore
// rules written before anything is created under .loopai/progress. It resolves the run's branch the
// way worktree creation does: the --branch override, else the plan's derived name.
func prepareDetachLaunch(o opts, cfg *config.Config, dio detachIO) (*detachLaunch, error) {
	token, err := detachT3Token(o)
	if err != nil {
		return nil, err
	}
	if reason := planFileRefusal(o.PlanFile); reason != "" {
		return nil, errors.New(reason)
	}
	exe, exeErr := detachExecutable()
	if reason := executableHandOffRefusal(exe, exeErr); reason != "" {
		return nil, errors.New(reason)
	}
	svc, err := openDetachRepository(cfg, dio)
	if err != nil {
		return nil, err
	}
	if ignoreErr := svc.EnsureLocalGitignore(); ignoreErr != nil {
		return nil, fmt.Errorf("prepare .loopai: %w", ignoreErr)
	}
	root := svc.Root()
	return &detachLaunch{o: o, dio: dio, svc: svc, exe: exe, root: root, planRel: relativePlan(root, o.PlanFile),
		branch: svc.EffectiveBranchName(o.PlanFile, o.Branch), token: token}, nil
}

// detachT3Token returns the T3 token a --t3-thread launch hands its child, from LOOPAI_T3_TOKEN, which
// such a launch requires; any other launch hands over none.
func detachT3Token(o opts) (string, error) {
	if o.T3Thread == "" {
		return "", nil
	}
	token := strings.TrimSpace(os.Getenv("LOOPAI_T3_TOKEN"))
	if token == "" {
		return "", errors.New("--t3-thread needs LOOPAI_T3_TOKEN")
	}
	return token, nil
}

// openDetachRepository opens the repository in the working directory with vcs_command and requires
// the directory to be its root, where the child runs and finds its run directory.
func openDetachRepository(cfg *config.Config, dio detachIO) (*git.Service, error) {
	if err := requireRepoRoot(cfg); err != nil {
		return nil, err
	}
	svc, err := git.NewService(".", writerPrinter{color: dio.colors.Info(), w: dio.stderr}, cfg.VcsCommand)
	if err != nil {
		return nil, fmt.Errorf("open git repo: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve working directory: %w", err)
	}
	if !sameProgressRoot(cwd, svc.Root()) {
		return nil, fmt.Errorf("must run from repository root %s", svc.Root())
	}
	return svc, nil
}

// relativePlan returns planFile relative to the checkout root, the form a descriptor records and
// launch locks hash. A relative path already is, since the launcher runs from the root; an absolute
// one inside the checkout is made relative, so both spellings name one run, and one outside it is
// kept as it is.
func relativePlan(root, planFile string) string {
	if !filepath.IsAbs(planFile) {
		return filepath.Clean(planFile)
	}
	resolved := planFile
	if evaluated, err := filepath.EvalSymlinks(planFile); err == nil {
		resolved = evaluated
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.Clean(planFile)
	}
	return rel
}

// decide picks the run this launch starts and records it in its descriptor. It runs under the launch
// lock of the plan and branch, so it asks runs.LatestFor, which reads that lock as this launcher's own:
// a live latest run is refused, one that did not succeed is resumed, and otherwise a new run is
// created, unless a worktree of the succeeded run's branch is still in place.
func (l *detachLaunch) decide(now time.Time) error {
	latest, found, err := runs.LatestFor(l.root, l.planRel, l.branch)
	if err != nil {
		return fmt.Errorf("find the latest run of %s: %w", l.planRel, err)
	}
	if found {
		id := latest.Descriptor.ID
		switch {
		case latest.Status.Live():
			return fmt.Errorf("run %s of %s is already %s; follow it with loopai --attach %s",
				id, l.planRel, latest.Status, id)
		case latest.Status != runs.StatusSucceeded:
			return l.resume(latest)
		}
		if refusal := l.refuseLeftWorktree(latest); refusal != nil {
			return refusal
		}
	}
	return l.create(now)
}

// refuseLeftWorktree refuses a new run of a plan whose latest run succeeded while a worktree of its
// branch is still in place: the one the run recorded keeping, or any other registered worktree with
// the branch checked out, such as one whose removal the outcome missed. A new run would otherwise
// resume that worktree, whose plan already moved to completed/, or fail to check the branch out.
func (l *detachLaunch) refuseLeftWorktree(latest runs.Run) error {
	const advice = "inspect it, then commit or remove what it holds and remove the worktree, or merge the branch " +
		"with loopai --merge %s, before launching the plan again"
	id, branch := latest.Descriptor.ID, latest.Descriptor.Branch
	if kept := runs.KeptWorktree(latest); kept != "" {
		return fmt.Errorf("run %s of %s succeeded but kept its worktree %s; "+advice, id, l.planRel, kept, branch)
	}
	checkedOut, err := l.branchWorktree()
	if err != nil {
		return err
	}
	if checkedOut != "" {
		return fmt.Errorf("run %s of %s succeeded, but its branch %s is still checked out in worktree %s; "+advice,
			id, l.planRel, branch, checkedOut, branch)
	}
	return nil
}

// branchWorktree returns the registered worktree other than the invoking checkout that has the run's
// branch checked out, or "" when there is none or its directory is gone.
func (l *detachLaunch) branchWorktree() (string, error) {
	worktrees, err := l.svc.Worktrees()
	if err != nil {
		return "", fmt.Errorf("check the worktrees of %s: %w", l.branch, err)
	}
	for _, wt := range worktrees {
		if wt.Branch != l.branch || sameProgressRoot(wt.Path, l.root) {
			continue
		}
		if _, statErr := os.Stat(wt.Path); statErr == nil {
			return wt.Path, nil
		}
	}
	return "", nil
}

// resume prepares the next attempt of run, which did not succeed: its outcome is kept as attempt
// history, its stop marker cleared, and its descriptor gets the new attempt. The recorded origin stays,
// so a run started from a T3 thread remains that thread's run when a plain shell resumes it, unless
// this launch names a T3 thread with --t3-thread, --acp-thread, or --acp-session, which then owns the
// run. A loopai thread's relaunch that names only its session, because the provider could not look the
// thread up, keeps the thread id a loopai thread recorded: the session is that thread's, and dropping
// the id would leave the run unmatched by the thread's binding and adoption checks.
func (l *detachLaunch) resume(run runs.Run) error {
	l.dir = run.Dir
	desc := run.Descriptor
	if err := runs.ArchiveOutcome(l.dir, desc.Attempt); err != nil {
		return fmt.Errorf("resume run %s: %w", desc.ID, err)
	}
	if err := runs.ClearStop(l.dir); err != nil {
		return fmt.Errorf("resume run %s: %w", desc.ID, err)
	}
	desc.Attempt++
	desc.PID, desc.PIDStart = 0, "" // the new attempt's process records itself
	if origin := l.origin(); origin.Kind != runs.OriginCLI {
		if origin.Kind == runs.OriginT3Thread && origin.ThreadID == "" && desc.Origin.Kind == runs.OriginT3Thread {
			origin.ThreadID = desc.Origin.ThreadID
		}
		desc.Origin = origin
	}
	return l.record(desc)
}

// origin is the origin this launch gives a run: the loopai thread --acp-thread or --acp-session
// names, whose provider started it, a terminal in the T3 thread --t3-thread names, else the shell.
func (l *detachLaunch) origin() runs.Origin {
	switch {
	case l.o.ACPThread != "" || l.o.ACPSession != "":
		return runs.Origin{Kind: runs.OriginT3Thread, ThreadID: l.o.ACPThread, ACPSessionID: l.o.ACPSession}
	case l.o.T3Thread != "":
		return runs.Origin{Kind: runs.OriginT3Terminal, ThreadID: l.o.T3Thread}
	}
	return runs.Origin{Kind: runs.OriginCLI}
}

// create makes the directory of a new run, prunes old succeeded runs, and records the run with this
// launch's origin. The directory is created exclusively and never reused: when one exists already, as
// for a launch that drew the same id within the same second, another id is drawn.
func (l *detachLaunch) create(now time.Time) error {
	id, err := l.createDir(now)
	if err != nil {
		return err
	}
	// the new directory has no descriptor yet, so pruning skips it
	if pruneErr := runs.PruneSucceeded(l.root, detachKeepSucceeded); pruneErr != nil {
		fmt.Fprintf(l.dio.stderr, "warning: %v\n", pruneErr)
	}
	return l.record(runs.Descriptor{ID: id, Attempt: 1, Plan: l.planRel, Branch: l.branch, Origin: l.origin()})
}

// createDir creates the directory of a new run with a fresh id and returns the id.
func (l *detachLaunch) createDir(now time.Time) (string, error) {
	for range detachMaxNewIDs {
		id := newRunID(l.planRel, now)
		dir := runs.Dir(l.root, id)
		err := os.Mkdir(dir, 0o750)
		if err == nil {
			l.dir = dir
			return id, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("create run directory: %w", err)
		}
	}
	return "", fmt.Errorf("create run directory: %d run ids drawn for %s were all taken", detachMaxNewIDs, l.planRel)
}

// record completes desc with this launch's executable, arguments, environment options, checkout,
// mode, and console log, sets the child's environment, and saves the descriptor. Arguments and
// options are recorded on every launch, since a later relaunch starts from them.
func (l *detachLaunch) record(desc runs.Descriptor) error {
	l.env = detachChildEnv(os.Environ(), desc.EnvOptions)
	desc.Executable = l.exe
	desc.Argv = stripDetachArgs(l.dio.args)
	desc.EnvOptions = detachEnvOptions(l.env)
	desc.Cwd = l.root
	desc.Mode = string(determineMode(l.o))
	desc.ConsoleLog = filepath.Join(l.dir, runs.ConsoleLogFile)
	if err := runs.SaveDescriptor(l.dir, &desc); err != nil {
		return fmt.Errorf("record run %s: %w", desc.ID, err)
	}
	l.desc = desc
	return nil
}

// start starts the run's child and reports its answer. A child started through the systemd scope that
// ends before it answers is started once more without the scope, after one warning.
func (l *detachLaunch) start(ctx context.Context) error {
	console, from, err := l.openConsole()
	if err != nil {
		return err
	}
	defer console.Close()

	prefix := detachScopePrefix()
	res := l.spawn(ctx, prefix, console)
	if len(prefix) > 0 && res.noHandshake() {
		fmt.Fprintf(l.dio.stderr, "warning: the run did not start in a systemd user scope (%s); starting it without one\n",
			res.noHandshakeReason())
		res = l.spawn(ctx, nil, console)
	}
	return l.report(res, from)
}

// openConsole opens the run's console log for appending and writes the attempt's separator line. The
// file becomes the child's stdout and stderr; the offset returned is where this attempt's output
// starts.
func (l *detachLaunch) openConsole() (*os.File, int64, error) {
	f, err := os.OpenFile(l.desc.ConsoleLog, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, 0, fmt.Errorf("open console log: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("open console log: %w", err)
	}
	if _, err := fmt.Fprintf(f, "=== loopai run %s, attempt %d, started %s ===\n",
		l.desc.ID, l.desc.Attempt, time.Now().UTC().Format(time.RFC3339)); err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("write console log: %w", err)
	}
	return f, info.Size(), nil
}

// detachStart is what one start of the child came to: its answer, or why none arrived.
type detachStart struct {
	ack      detachAckMessage
	answered bool  // the child sent its answer line
	startErr error // the child could not be started
	readErr  error // the answer could not be read or decoded
	waitErr  error // the launcher stopped waiting: the timeout passed or its context ended
}

// noHandshake reports whether the child ended before any answer: it could not be started, or the ack
// pipe reached EOF without a line, which means every holder of its write end exited.
func (s detachStart) noHandshake() bool {
	return s.startErr != nil || (!s.answered && s.readErr == nil && s.waitErr == nil)
}

// noHandshakeReason describes a start that ended without an answer.
func (s detachStart) noHandshakeReason() string {
	if s.startErr != nil {
		return s.startErr.Error()
	}
	return "it exited before answering"
}

// spawn starts the child once, through prefix when it is not empty, and waits for its answer. The
// child gets two distinct pipes: the read end of the secret pipe as fd 3 and the write end of the ack
// pipe as fd 4, the inherited FIFOs it requires. The launcher closes its copies of those ends at once,
// so EOF on the ack pipe comes from the child alone, sends the secret line, which carries the T3 token
// of a --t3-thread run and is the only way the token reaches the child, and never waits for the
// child, which outlives it; the child's exit shows as that EOF instead.
func (l *detachLaunch) spawn(ctx context.Context, prefix []string, console *os.File) detachStart {
	secretR, secretW, err := os.Pipe()
	if err != nil {
		return detachStart{startErr: fmt.Errorf("create the handshake pipe: %w", err)}
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		_ = secretR.Close()
		_ = secretW.Close()
		return detachStart{startErr: fmt.Errorf("create the handshake pipe: %w", err)}
	}
	defer ackR.Close()

	argv := detachChildArgv(prefix, l.exe, l.desc.ID, l.desc.Argv)
	// the child outlives the launcher, so no context may kill it
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:noctx // a context would kill the detached run
	cmd.Dir, cmd.Env = l.root, l.env
	cmd.Stdout, cmd.Stderr = console, console
	cmd.ExtraFiles = []*os.File{secretR, ackW}
	startErr := startDetachedProcess(cmd)
	_ = secretR.Close()
	_ = ackW.Close()
	if startErr != nil {
		_ = secretW.Close()
		return detachStart{startErr: startErr}
	}
	defer func() { _ = cmd.Process.Release() }()

	sendDetachSecret(secretW, detachSecret{T3Token: l.token})
	return awaitDetachAck(ctx, ackR, detachAckTimeout)
}

// sendDetachSecret writes secret as the child's one handshake line and closes the pipe. A child that
// is already gone cannot read it; that shows as EOF on the ack pipe, so the write error is dropped.
func sendDetachSecret(w io.WriteCloser, secret detachSecret) {
	data, _ := json.Marshal(secret) // a struct of strings always encodes
	_, _ = w.Write(append(data, '\n'))
	_ = w.Close()
}

// awaitDetachAck waits for the child's answer on r, at most timeout and no longer than ctx lives.
func awaitDetachAck(ctx context.Context, r io.Reader, timeout time.Duration) detachStart {
	answer := make(chan detachStart, 1)
	go func() { answer <- readDetachAck(r) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-answer:
		return res
	case <-timer.C:
		return detachStart{waitErr: fmt.Errorf("no answer within %s", timeout)}
	case <-ctx.Done():
		return detachStart{waitErr: fmt.Errorf("stopped waiting for its answer: %w", ctx.Err())}
	}
}

// readDetachAck reads the child's one answer line from r. EOF before any line reports no answer.
func readDetachAck(r io.Reader) detachStart {
	line, err := bufio.NewReader(io.LimitReader(r, maxDetachAckLine)).ReadBytes('\n')
	if len(bytes.TrimSpace(line)) == 0 {
		if err != nil && !errors.Is(err, io.EOF) {
			return detachStart{readErr: fmt.Errorf("read its answer: %w", err)}
		}
		return detachStart{}
	}
	var ack detachAckMessage
	if json.Unmarshal(line, &ack) != nil {
		return detachStart{answered: true, readErr: fmt.Errorf("malformed answer %q", bytes.TrimSpace(line))}
	}
	return detachStart{ack: ack, answered: true}
}

// report turns the child's answer into the launcher's result: on ok it prints the run id and the
// follow command; otherwise it returns why the attempt did not start, showing the end of the console
// log from offset from when the child left nothing better to say.
func (l *detachLaunch) report(res detachStart, from int64) error {
	id, consoleLog := l.desc.ID, l.desc.ConsoleLog
	switch {
	case res.startErr != nil:
		return fmt.Errorf("detached run %s: %w", id, res.startErr)
	case res.waitErr != nil:
		return fmt.Errorf("detached run %s: %w; it may still be starting: follow it with loopai --attach %s "+
			"or read its console log %s", id, res.waitErr, id, consoleLog)
	case !res.answered:
		l.printConsoleTail(from)
		return fmt.Errorf("detached run %s exited before it started; console log: %s", id, consoleLog)
	case res.readErr != nil:
		l.printConsoleTail(from)
		return fmt.Errorf("detached run %s: %w; console log: %s", id, res.readErr, consoleLog)
	case !res.ack.OK:
		return fmt.Errorf("detached run %s did not start: %s", id, res.ack.Error)
	case res.ack.RunID != id:
		return fmt.Errorf("detached run %s: the child answered for run %q", id, res.ack.RunID)
	}
	fmt.Fprintf(l.dio.stdout, "run: %s\nfollow: loopai --attach %s\n", id, id)
	return nil
}

// printConsoleTail prints the last lines the attempt wrote to the console log from offset from.
func (l *detachLaunch) printConsoleTail(from int64) {
	if tail := consoleTail(l.desc.ConsoleLog, from, detachConsoleTailLines); tail != "" {
		fmt.Fprintf(l.dio.stderr, "the end of console log %s:\n%s\n", l.desc.ConsoleLog, tail)
	}
}

// consoleTail returns the last lines lines of the console log at path written from offset from on,
// each indented by two spaces; "" when there are none or the log cannot be read.
func consoleTail(path string, from int64, lines int) string {
	f, err := os.Open(path) //nolint:gosec // the console log of a run directory
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	from = max(from, info.Size()-maxDetachConsoleTail)
	if _, seekErr := f.Seek(from, io.SeekStart); seekErr != nil {
		return ""
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return ""
	}
	all := strings.Split(text, "\n")
	tail := all[max(0, len(all)-lines):]
	return "  " + strings.Join(tail, "\n  ")
}
