package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/umputun/ralphex/pkg/config"
)

// launcher tags recorded in the launch history; the skills read only these three.
const (
	launcherOrca = "orca"
	launcherT3   = "t3"
	launcherCLI  = "cli"
)

const (
	launchHistoryLimit = 10
	launchHistoryFile  = "launch-history"
)

// launchEntry is one plan-executing launch: the effective model flags and the launcher.
type launchEntry struct {
	When     time.Time
	Launcher string
	Flags    string
}

// launchHistoryPath returns the history file in the global config directory, honoring
// the --config-dir/LOOPAI_CONFIG_DIR override the same way config.Load does.
func launchHistoryPath(configDir string) string {
	dir := configDir
	if dir == "" {
		dir = config.DefaultConfigDir()
	}
	return filepath.Join(dir, launchHistoryFile)
}

func knownLauncher(name string) bool {
	switch name {
	case launcherOrca, launcherT3, launcherCLI:
		return true
	}
	return false
}

// readLaunchHistory returns the recorded entries newest first. A missing or unreadable
// file is an empty history, and blank, malformed, or unknown-launcher lines are skipped.
func readLaunchHistory(path string) []launchEntry {
	f, err := os.Open(path) //nolint:gosec // path is derived from the loopai config directory
	if err != nil {
		return nil
	}
	defer f.Close()

	var entries []launchEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 || !knownLauncher(fields[1]) {
			continue
		}
		when, err := time.Parse(time.RFC3339, fields[0])
		if err != nil {
			continue
		}
		entries = append(entries, launchEntry{When: when.UTC(), Launcher: fields[1], Flags: fields[2]})
	}
	return entries
}

// recordLaunch prepends entry to the history at path, dropping any older entry with the
// same flags and keeping at most launchHistoryLimit entries. The file is replaced whole.
func recordLaunch(path string, entry launchEntry) error {
	entries := []launchEntry{entry}
	for _, e := range readLaunchHistory(path) {
		if e.Flags == entry.Flags {
			continue
		}
		entries = append(entries, e)
	}
	if len(entries) > launchHistoryLimit {
		entries = entries[:launchHistoryLimit]
	}
	return writeFileAtomic(path, ".launch-history-*.tmp", "launch history", func(w io.Writer) error {
		bw := bufio.NewWriter(w)
		for _, e := range entries {
			if _, err := fmt.Fprintf(bw, "%s\t%s\t%s\n", e.When.UTC().Format(time.RFC3339), e.Launcher, e.Flags); err != nil {
				return err //nolint:wrapcheck // wrapped by writeFileAtomic
			}
		}
		return bw.Flush()
	})
}

// launchFlagValue is the charset the loopai-plan, loopai-orca, and loopai-t3 skills
// accept for a pass-through flag value; a value outside it is never recorded.
var launchFlagValue = regexp.MustCompile(`^[A-Za-z0-9._:,+-]+$`)

// launchFlags renders the effective model choice as the three pass-through flags. The
// review spec is rendered only when set, since an inherited one equals the task spec, and
// the reviewer chain only when explicit, since an automatic reviewer follows from the
// task provider and is selected again the same way on the next launch.
func launchFlags(o opts, cfg *config.Config, sel externalReviewSelection) string {
	var parts []string
	add := func(flag, value string) {
		if value != "" && launchFlagValue.MatchString(value) {
			parts = append(parts, flag+" "+value)
		}
	}
	add("--task-model", resolveSpec(o.TaskModel, cfg.TaskModel))
	add("--review-model", resolveSpec(o.ReviewModel, cfg.ReviewModel))
	if sel.Explicit {
		add("--external-reviewers", sel.flagValue())
	}
	return strings.Join(parts, " ")
}

// launcherFor names the launcher that started the run. T3 wins over Orca because
// --t3-launch terminal mode runs `loopai --t3`; a config enabling both is a reporting
// choice, and the Orca skill passes --orca alone.
func launcherFor(cfg *config.Config) string {
	switch {
	case cfg.T3:
		return launcherT3
	case cfg.Orca:
		return launcherOrca
	default:
		return launcherCLI
	}
}

// recordLaunchHistory records one plan-executing launch in the global history. It is
// best-effort like the cmux, Orca, and T3 reporters: a failure is one warning on stderr
// and never fails, delays, or changes the run. stderr must not be the process stdout,
// which the ACP protocol owns.
func recordLaunchHistory(o opts, cfg *config.Config, sel externalReviewSelection, launcher string, stderr io.Writer) {
	entry := launchEntry{When: time.Now().UTC(), Launcher: launcher, Flags: launchFlags(o, cfg, sel)}
	if err := recordLaunch(launchHistoryPath(o.ConfigDir), entry); err != nil {
		fmt.Fprintf(stderr, "warning: launch history not recorded: %v\n", err)
	}
}
