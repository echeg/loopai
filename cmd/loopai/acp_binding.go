package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/runs"
)

// acpBindingDir holds the Mode 2 bindings, relative to the session cwd. It is a loopai runtime path,
// which the dirty checks skip. The generated .loopai/.gitignore cannot be what keeps it out of Git
// status: that file ignores itself and is never committed, so a T3 thread's worktree, created from
// committed files, never has it, and neither does a checkout loopai has not run in or one whose file
// predates acp/. Every binding write therefore installs the directory's own ignore file first.
const acpBindingDir = ".loopai/acp"

// acpSessionIDPattern accepts a session id that is one safe path segment: loopai's own
// "loopai-<hex>" ids and whatever id a client passes to session/load.
var acpSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// acpBinding is one binding file. A thread binding, binding-thread-<runs.ThreadKey(thread id)>.json,
// binds the run a T3 thread started; a session binding, binding-session-<session id>.json, records
// which thread a session belongs to and binds the run itself when the thread id is unknown. The
// thread id is the binding key wherever it is known, because T3 Code replaces the session id when a
// session/load falls back to a new session; the session id is only the fallback. Thread ids can carry
// ':' and '%' and exceed 300 characters, so the raw id is kept inside the file and only its hash in
// the name.
type acpBinding struct {
	ThreadID  string    `json:"thread_id"`
	SessionID string    `json:"session_id"`
	RunID     string    `json:"run_id"`
	Cwd       string    `json:"cwd"`
	UpdatedAt time.Time `json:"updated_at"`
}

// acpThreadBindingPath returns the binding file of a thread.
func acpThreadBindingPath(cwd, threadID string) string {
	return filepath.Join(cwd, filepath.FromSlash(acpBindingDir), "binding-thread-"+runs.ThreadKey(threadID)+".json")
}

// acpSessionBindingPath returns the binding file of a session, refusing an id that is not one safe
// path segment: the id comes from the client, and session/load accepts any.
func acpSessionBindingPath(cwd, sessionID string) (string, error) {
	if !acpSessionIDPattern.MatchString(sessionID) {
		return "", fmt.Errorf("unsafe ACP session id %q", sessionID)
	}
	return filepath.Join(cwd, filepath.FromSlash(acpBindingDir), "binding-session-"+sessionID+".json"), nil
}

// requireBindingStore refuses a session cwd that cannot hold bindings, before any binding is read or
// written: a relative one, whose files would land wherever the process happens to run; one that is not
// an existing directory, such as a thread worktree removed since, whose tree a write would recreate
// for later runs to misread as loopai's own; and one whose .loopai or .loopai/acp is a symbolic link
// or not a directory, which a write would follow outside the checkout. A missing .loopai or
// .loopai/acp is fine: the first write creates it inside the existing cwd.
func requireBindingStore(cwd string) error {
	if !filepath.IsAbs(cwd) {
		return fmt.Errorf("ACP session cwd %q is not absolute", cwd)
	}
	info, err := os.Lstat(cwd)
	switch {
	case err != nil:
		return fmt.Errorf("ACP session cwd: %w", err)
	case !info.IsDir():
		return fmt.Errorf("ACP session cwd %s is not a directory", cwd)
	}
	path := cwd
	for component := range strings.SplitSeq(acpBindingDir, "/") {
		path = filepath.Join(path, component)
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil
		case err != nil:
			return fmt.Errorf("inspect %s: %w", path, err)
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("%s is a symbolic link", path)
		case !info.IsDir():
			return fmt.Errorf("%s is not a directory", path)
		}
	}
	return nil
}

// saveACPBinding writes binding b to path, a binding file of the session cwd, with mode 0600 and
// atomically. The binding directory's own ignore file is installed first, and its failure refuses the
// write: a binding Git could see shows up in T3 Code's changes panel and gets committed by the first
// agent of the thread that stages everything.
func saveACPBinding(cwd, path, label string, b acpBinding) error {
	if err := git.EnsureRuntimeDirectoryIgnored(filepath.Join(cwd, filepath.FromSlash(acpBindingDir))); err != nil {
		return fmt.Errorf("ignore the ACP bindings: %w", err)
	}
	return saveJSONState(path, label, b)
}

// readACPBinding reads a binding file and reports whether belongs accepts it as the key's own. A
// missing, unreadable, or undecodable file reads as none, and so does one written for another key or
// naming an invalid run id, as a stray file of another schema, such as the removed ACP chat's
// <session-id>.json state, would.
func readACPBinding(path string, belongs func(acpBinding) bool) (acpBinding, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // the name is a hash or a validated single segment
	if err != nil {
		return acpBinding{}, false
	}
	var b acpBinding
	if json.Unmarshal(data, &b) != nil || !belongs(b) || (b.RunID != "" && !runs.ValidID(b.RunID)) {
		return acpBinding{}, false
	}
	return b, true
}

// readThreadBinding reads the binding of a thread; an empty id has none.
func readThreadBinding(cwd, threadID string) (acpBinding, bool) {
	if threadID == "" {
		return acpBinding{}, false
	}
	return readACPBinding(acpThreadBindingPath(cwd, threadID), func(b acpBinding) bool { return b.ThreadID == threadID })
}

// readSessionBinding reads the binding of a session; an unsafe id has none.
func readSessionBinding(cwd, sessionID string) (acpBinding, bool) {
	path, err := acpSessionBindingPath(cwd, sessionID)
	if err != nil {
		return acpBinding{}, false
	}
	return readACPBinding(path, func(b acpBinding) bool { return b.SessionID == sessionID })
}

// loadBinding returns the binding of a session's thread, read from disk alone. A known thread id
// finds the thread's binding. Without one, the session's binding names the thread, which is how a
// session/load in a new process restores it, and that thread's binding wins when it exists. Otherwise
// the session's own binding is returned, unless it belongs to another thread than threadID. A cwd
// requireBindingStore refuses has no binding.
func loadBinding(cwd, threadID, sessionID string) (acpBinding, bool) {
	if requireBindingStore(cwd) != nil {
		return acpBinding{}, false
	}
	if b, ok := readThreadBinding(cwd, threadID); ok {
		return b, true
	}
	s, ok := readSessionBinding(cwd, sessionID)
	if !ok {
		return acpBinding{}, false
	}
	if threadID == "" {
		if b, found := readThreadBinding(cwd, s.ThreadID); found {
			return b, true
		}
		return s, true
	}
	if s.ThreadID != "" && s.ThreadID != threadID {
		return acpBinding{}, false
	}
	return s, true
}

// saveSessionThread records in the session's binding which thread the session belongs to, keeping
// the run it binds. An empty threadID keeps a thread recorded earlier, so a failed lookup never
// forgets the thread a previous one found.
func saveSessionThread(cwd, sessionID, threadID string) error {
	if err := requireBindingStore(cwd); err != nil {
		return err
	}
	path, err := acpSessionBindingPath(cwd, sessionID)
	if err != nil {
		return err
	}
	b, _ := readSessionBinding(cwd, sessionID)
	if threadID != "" {
		b.ThreadID = threadID
	}
	b.SessionID, b.Cwd, b.UpdatedAt = sessionID, cwd, time.Now().UTC()
	return saveACPBinding(cwd, path, "ACP session binding", b)
}

// bindRun binds run runID to the thread, or to the session when the thread id is unknown, replacing
// a run bound before.
func bindRun(cwd, threadID, sessionID, runID string) error {
	if !runs.ValidID(runID) {
		return fmt.Errorf("invalid run id %q", runID)
	}
	if err := requireBindingStore(cwd); err != nil {
		return err
	}
	if threadID != "" {
		b := acpBinding{ThreadID: threadID, SessionID: sessionID, RunID: runID, Cwd: cwd, UpdatedAt: time.Now().UTC()}
		return saveACPBinding(cwd, acpThreadBindingPath(cwd, threadID), "ACP thread binding", b)
	}
	path, err := acpSessionBindingPath(cwd, sessionID)
	if err != nil {
		return err
	}
	b, _ := readSessionBinding(cwd, sessionID)
	b.SessionID, b.RunID, b.Cwd, b.UpdatedAt = sessionID, runID, cwd, time.Now().UTC()
	return saveACPBinding(cwd, path, "ACP session binding", b)
}

// unbindRun removes run runID from the thread's binding and from the session's, wherever it is
// bound, so neither key finds it again. A binding naming another run is left alone, and so is the
// session's record of its thread.
func unbindRun(cwd, threadID, sessionID, runID string) error {
	if runID == "" {
		return nil
	}
	if err := requireBindingStore(cwd); err != nil {
		return err
	}
	var errs []error
	if b, ok := readThreadBinding(cwd, threadID); ok && b.RunID == runID {
		b.RunID, b.UpdatedAt = "", time.Now().UTC()
		errs = append(errs, saveACPBinding(cwd, acpThreadBindingPath(cwd, threadID), "ACP thread binding", b))
	}
	if b, ok := readSessionBinding(cwd, sessionID); ok && b.RunID == runID {
		path, err := acpSessionBindingPath(cwd, sessionID)
		if err == nil {
			b.RunID, b.UpdatedAt = "", time.Now().UTC()
			err = saveACPBinding(cwd, path, "ACP session binding", b)
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
