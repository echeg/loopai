package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/runs"
)

// openJournalDirRepo opens the repository holding the working directory for prepareJournalDir. It
// runs before config loading, so it uses plain git: a repository a custom vcs_command reaches but
// git does not has no git work tree for the journal to pollute.
var openJournalDirRepo = func() (*git.Service, error) {
	return git.NewService(".", nil)
}

// prepareJournalDir checks and creates the --journal-dir directory before anything else runs and
// returns its absolute path, so the journal and outcome land where the host expects them whatever
// the run does to its working directory later. It refuses a path that exists and is not a directory,
// a directory already holding events.jsonl or outcome.json (the files belong to one run, and a
// foreground run holds no lock that would keep a second one out), and a directory inside the
// repository work tree whose journal files git does not ignore: review stalemate detection
// fingerprints untracked file content, so journal writes there would read as code changes. A
// missing directory is created with mode 0700 once every check has passed.
func prepareJournalDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve --journal-dir %s: %w", dir, err)
	}
	info, err := os.Stat(abs)
	switch {
	case err == nil && !info.IsDir():
		return "", fmt.Errorf("--journal-dir %s is not a directory", abs)
	case err == nil:
		for _, name := range []string{runs.JournalFile, runs.OutcomeFile} {
			_, statErr := os.Lstat(filepath.Join(abs, name))
			if statErr == nil {
				return "", fmt.Errorf("--journal-dir %s already holds %s; use an empty directory for each run", abs, name)
			}
			if !errors.Is(statErr, fs.ErrNotExist) {
				return "", fmt.Errorf("inspect --journal-dir %s: %w", abs, statErr)
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("inspect --journal-dir %s: %w", abs, err)
	}
	if err := refuseUnignoredJournalDir(abs); err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", fmt.Errorf("create --journal-dir %s: %w", abs, err)
	}
	return abs, nil
}

// refuseUnignoredJournalDir refuses an absolute journal directory inside the work tree of the
// repository holding the working directory unless git ignores both files the run writes there.
// The files are checked rather than the directory because a directory-only ignore rule matches a
// missing directory only once it exists, while a file below an ignored directory always matches.
// Outside a repository there is no work tree to protect.
func refuseUnignoredJournalDir(abs string) error {
	svc, err := openJournalDirRepo()
	if err != nil || !svc.WorkTreeContains(abs) {
		return nil //nolint:nilerr // no git work tree around the working directory, nothing to pollute
	}
	for _, name := range []string{runs.JournalFile, runs.OutcomeFile} {
		ignored, ignoreErr := svc.PathIgnored(filepath.Join(abs, name))
		if ignoreErr != nil {
			return fmt.Errorf("check --journal-dir %s: %w", abs, ignoreErr)
		}
		if !ignored {
			return fmt.Errorf("--journal-dir %s is inside the repository work tree and git does not ignore %s; "+
				"use a directory outside the checkout or one git ignores", abs, name)
		}
	}
	return nil
}
