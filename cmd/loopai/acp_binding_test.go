package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/runs"
)

// acpLongThreadID is a ~330-character T3 thread id of the kind subagent and provider-native threads
// get: it carries ':' and '%' and could never be a file name.
var acpLongThreadID = "thread:provider:claudeAgent:native-thread:" +
	strings.Repeat("thread%3Aprovider%3AclaudeAgent%3Anative-thread%3A", 5) + "thread%3Aprovider%3Aabc%3Aagent%3A3"

// readBindingFile decodes a binding file into a generic map, so the test sees the exact JSON keys.
func readBindingFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test temp file
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(data, &m))
	return m
}

// bindingDirNames returns the sorted names in the session cwd's binding directory.
func bindingDirNames(t *testing.T, cwd string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(cwd, ".loopai", "acp"))
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

func TestSaveSessionThreadSchema(t *testing.T) {
	cwd := t.TempDir()
	before := time.Now().UTC()
	require.NoError(t, saveSessionThread(cwd, "loopai-abc", "thread-1"))

	path := filepath.Join(cwd, ".loopai", "acp", "binding-session-loopai-abc.json")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "a binding is private to the user")
	m := readBindingFile(t, path)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	assert.Equal(t, []string{"cwd", "run_id", "session_id", "thread_id", "updated_at"}, keys)
	assert.Equal(t, "thread-1", m["thread_id"])
	assert.Equal(t, "loopai-abc", m["session_id"])
	assert.Empty(t, m["run_id"])
	assert.Equal(t, cwd, m["cwd"])
	updated, err := time.Parse(time.RFC3339Nano, m["updated_at"].(string))
	require.NoError(t, err)
	assert.WithinDuration(t, before, updated, time.Minute)

	assert.Equal(t, []string{".gitignore", "binding-session-loopai-abc.json"}, bindingDirNames(t, cwd),
		"the write leaves no temp file behind, only the binding and the directory's ignore file")
	data, err := os.ReadFile(filepath.Join(cwd, ".loopai", "acp", ".gitignore")) //nolint:gosec // test temp file
	require.NoError(t, err)
	assert.Equal(t, "*\n", string(data))
}

func TestBindRunHashesALongThreadID(t *testing.T) {
	require.Greater(t, len(acpLongThreadID), 300)
	cwd := t.TempDir()
	require.NoError(t, bindRun(cwd, acpLongThreadID, "loopai-abc", "feature-20261022-120000-abcd"))

	dir := filepath.Join(cwd, ".loopai", "acp")
	names := bindingDirNames(t, cwd)
	require.Equal(t, []string{".gitignore", "binding-thread-" + runs.ThreadKey(acpLongThreadID) + ".json"}, names)
	name := names[1]
	assert.Regexp(t, `^binding-thread-[0-9a-f]{32}\.json$`, name, "only the hash is in the name")
	assert.Equal(t, acpLongThreadID, readBindingFile(t, filepath.Join(dir, name))["thread_id"], "the raw id is in the file")

	b, ok := loadBinding(cwd, acpLongThreadID, "another-session")
	require.True(t, ok, "the thread key finds the binding from any session")
	assert.Equal(t, acpBinding{ThreadID: acpLongThreadID, SessionID: "loopai-abc", RunID: "feature-20261022-120000-abcd",
		Cwd: cwd, UpdatedAt: b.UpdatedAt}, b)
	assert.False(t, b.UpdatedAt.IsZero())
}

func TestLoadBindingRestoresTheThreadFromTheSession(t *testing.T) {
	cwd := t.TempDir()
	require.NoError(t, saveSessionThread(cwd, "s-1", "thread-1"))

	b, ok := loadBinding(cwd, "", "s-1")
	require.True(t, ok)
	assert.Equal(t, "thread-1", b.ThreadID, "a session/load without a lookup still knows its thread")
	assert.Empty(t, b.RunID)

	require.NoError(t, bindRun(cwd, "thread-1", "s-1", "run-a"))
	b, ok = loadBinding(cwd, "", "s-1")
	require.True(t, ok)
	assert.Equal(t, "run-a", b.RunID, "the session's thread leads to the thread's run")
	assert.Equal(t, "thread-1", b.ThreadID)

	_, ok = loadBinding(cwd, "", "s-2")
	assert.False(t, ok, "an unknown session with no thread has no binding")
	b, ok = loadBinding(cwd, "thread-1", "s-2")
	require.True(t, ok, "a session T3 Code replaced finds the run through the thread")
	assert.Equal(t, "run-a", b.RunID)
}

func TestBindingSessionFallback(t *testing.T) {
	cwd := t.TempDir()
	require.NoError(t, saveSessionThread(cwd, "s-1", ""))
	require.NoError(t, bindRun(cwd, "", "s-1", "run-a"))

	assert.Equal(t, []string{".gitignore", "binding-session-s-1.json"}, bindingDirNames(t, cwd),
		"without a thread id the session file is the key")

	b, ok := loadBinding(cwd, "", "s-1")
	require.True(t, ok)
	assert.Equal(t, acpBinding{SessionID: "s-1", RunID: "run-a", Cwd: cwd, UpdatedAt: b.UpdatedAt}, b)
	b, ok = loadBinding(cwd, "thread-1", "s-1")
	require.True(t, ok, "a thread found later still sees the run bound by session")
	assert.Equal(t, "run-a", b.RunID)

	require.NoError(t, saveSessionThread(cwd, "s-1", "thread-1"))
	b, ok = loadBinding(cwd, "", "s-1")
	require.True(t, ok)
	assert.Equal(t, "run-a", b.RunID, "recording the thread keeps the run")
	assert.Equal(t, "thread-1", b.ThreadID)
	require.NoError(t, saveSessionThread(cwd, "s-1", ""))
	b, _ = loadBinding(cwd, "", "s-1")
	assert.Equal(t, "thread-1", b.ThreadID, "a failed lookup later never forgets the thread")

	_, ok = loadBinding(cwd, "thread-2", "s-1")
	assert.False(t, ok, "a session of another thread is not this thread's binding")
}

func TestUnbindRun(t *testing.T) {
	cwd := t.TempDir()
	require.NoError(t, saveSessionThread(cwd, "s-1", ""))
	require.NoError(t, bindRun(cwd, "", "s-1", "run-a"))
	require.NoError(t, saveSessionThread(cwd, "s-1", "thread-1"))
	require.NoError(t, bindRun(cwd, "thread-1", "s-1", "run-a"))

	require.NoError(t, unbindRun(cwd, "thread-1", "s-1", "run-b"))
	b, _ := loadBinding(cwd, "thread-1", "s-1")
	assert.Equal(t, "run-a", b.RunID, "another run's unbind leaves the binding alone")

	require.NoError(t, unbindRun(cwd, "thread-1", "s-1", "run-a"))
	b, ok := loadBinding(cwd, "thread-1", "s-1")
	require.True(t, ok)
	assert.Empty(t, b.RunID, "neither key finds the run again")
	b, ok = loadBinding(cwd, "", "s-1")
	require.True(t, ok)
	assert.Empty(t, b.RunID)
	assert.Equal(t, "thread-1", b.ThreadID, "the session keeps its thread")

	require.NoError(t, unbindRun(cwd, "thread-1", "s-1", ""), "an empty run id unbinds nothing")
	require.NoError(t, unbindRun(cwd, "thread-9", "s-9", "run-a"), "a key with no binding is no error")
}

// TestBindingWritesStayOutOfGitStatus writes every kind of binding into checkouts with no ignore
// rule for .loopai/acp/ of their own: none of them shows up in Git status, so neither T3 Code's
// changes panel nor an agent of the thread that stages everything ever sees one.
func TestBindingWritesStayOutOfGitStatus(t *testing.T) {
	// a developer's global excludes could ignore .loopai/ and hide what the test checks
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	writeLoopaiIgnore := func(t *testing.T, repo, content string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(filepath.Join(repo, ".loopai"), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(repo, ".loopai", ".gitignore"), []byte(content), 0o600))
	}
	tests := []struct {
		name  string
		setup func(t *testing.T) string // returns the session cwd
	}{
		{name: "a fresh repository", setup: setupTestRepo},
		{name: "a thread's linked worktree of a checkout with the generated ignore file", setup: func(t *testing.T) string {
			repo := setupTestRepo(t)
			writeLoopaiIgnore(t, repo, ".gitignore\nprogress/\nworktrees/\nacp/\n") // ignores itself, never committed
			wt := filepath.Join(t.TempDir(), "thread")
			runGit(t, repo, "worktree", "add", "-b", "t3code/thread", wt)
			return wt
		}},
		{name: "a generated ignore file that predates acp/", setup: func(t *testing.T) string {
			repo := setupTestRepo(t)
			writeLoopaiIgnore(t, repo, ".gitignore\nprogress/\nworktrees/\n")
			return repo
		}},
		{name: "a committed custom ignore file", setup: func(t *testing.T) string {
			repo := setupTestRepo(t)
			writeLoopaiIgnore(t, repo, "custom-rule\n")
			runGit(t, repo, "add", ".loopai/.gitignore")
			runGit(t, repo, "commit", "-m", "add a custom loopai ignore file")
			return repo
		}},
	}
	const runID = "feature-20261022-120000-abcd"
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cwd := tc.setup(t)
			require.NoError(t, saveSessionThread(cwd, "s-1", "thread-1"))
			require.NoError(t, bindRun(cwd, "thread-1", "s-1", runID))
			require.NoError(t, bindRun(cwd, "", "s-2", runID))
			require.NoError(t, unbindRun(cwd, "thread-1", "s-1", runID))
			assert.Equal(t, []string{".gitignore", "binding-session-s-1.json", "binding-session-s-2.json",
				"binding-thread-" + runs.ThreadKey("thread-1") + ".json"}, bindingDirNames(t, cwd))

			assert.Empty(t, gitOutput(t, cwd, "status", "--porcelain"))
			assert.Empty(t, gitOutput(t, cwd, "status", "--porcelain", "--untracked-files=all"))
			assert.Empty(t, gitOutput(t, cwd, "add", "--all", "--dry-run"), "staging everything picks up no binding")

			require.NoError(t, os.Remove(filepath.Join(cwd, ".loopai", "acp", ".gitignore")))
			assert.NotEmpty(t, gitOutput(t, cwd, "status", "--porcelain"),
				"the checkout ignores no binding by itself: the directory's own ignore file is what hides them")
		})
	}
}

// TestBindingWriteNeedsItsIgnoreFile refuses a binding whose directory cannot get its ignore file,
// since Git would show the binding.
func TestBindingWriteNeedsItsIgnoreFile(t *testing.T) {
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".loopai", "acp")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(target, filepath.Join(dir, ".gitignore")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	require.ErrorContains(t, saveSessionThread(cwd, "s-1", "thread-1"), "symbolic link")
	require.ErrorContains(t, bindRun(cwd, "thread-1", "s-1", "run-a"), "symbolic link")
	require.ErrorContains(t, bindRun(cwd, "", "s-1", "run-a"), "symbolic link")
	assert.Equal(t, []string{".gitignore"}, bindingDirNames(t, cwd), "no binding is written")
	assert.NoFileExists(t, target)
}

func TestBindingIgnoresFilesThatDoNotDecode(t *testing.T) {
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".loopai", "acp")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	write := func(name, content string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	// the removed ACP chat's state of the same session, and binding files that are not this key's
	write("s-1.json", `{"provider":"claude","provider_session_id":"p-1","last_run":{"plan":"docs/plans/a.md"}}`)
	write("binding-session-s-1.json", `{"session_id":`)
	write("binding-session-s-2.json", `{"provider":"claude","last_run":{"plan":"docs/plans/a.md"}}`)
	write("binding-session-s-3.json", `{"session_id":"s-other","run_id":"run-a"}`)
	write("binding-session-s-4.json", `{"session_id":"s-4","run_id":"../../escape"}`)
	write("binding-thread-"+runs.ThreadKey("thread-1")+".json", `{"thread_id":"thread-2","run_id":"run-a"}`)

	for _, sid := range []string{"s-1", "s-2", "s-3", "s-4"} {
		_, ok := loadBinding(cwd, "", sid)
		assert.False(t, ok, sid)
	}
	_, ok := loadBinding(cwd, "thread-1", "")
	assert.False(t, ok, "a thread file holding another thread is ignored")

	require.NoError(t, saveSessionThread(cwd, "s-1", "thread-1"), "a corrupt binding is replaced, not an error")
	b, ok := loadBinding(cwd, "", "s-1")
	require.True(t, ok)
	assert.Equal(t, "thread-1", b.ThreadID)
	assert.Empty(t, b.RunID)
	data, err := os.ReadFile(filepath.Join(dir, "s-1.json")) //nolint:gosec // test temp file
	require.NoError(t, err)
	assert.Contains(t, string(data), "provider_session_id", "the old state file is never touched")
}

func TestBindingRefusesUnsafeKeys(t *testing.T) {
	cwd := t.TempDir()
	for _, sid := range []string{"", "../escape", "a/b", ".hidden", strings.Repeat("x", 129)} {
		require.Error(t, saveSessionThread(cwd, sid, "thread-1"), "session id %q", sid)
		require.Error(t, bindRun(cwd, "", sid, "run-a"), "session id %q", sid)
		_, ok := loadBinding(cwd, "", sid)
		assert.False(t, ok, "session id %q", sid)
	}
	require.NoError(t, bindRun(cwd, "thread-1", "../unsafe-but-unused", "run-a"),
		"a thread binding keeps the session id inside the file only")

	for _, runID := range []string{"", "../escape", "Upper"} {
		require.Error(t, bindRun(cwd, "thread-1", "s-1", runID), "run id %q", runID)
	}

	rel := "relative/checkout"
	require.ErrorContains(t, saveSessionThread(rel, "s-1", "thread-1"), "not absolute")
	require.ErrorContains(t, bindRun(rel, "thread-1", "s-1", "run-a"), "not absolute")
	require.ErrorContains(t, unbindRun(rel, "thread-1", "s-1", "run-a"), "not absolute")
	_, ok := loadBinding(rel, "thread-1", "s-1")
	assert.False(t, ok)
	_, err := os.Stat(filepath.Join(cwd, "relative"))
	assert.True(t, os.IsNotExist(err))
}

// bindingTreeFiles maps every file under root to its content.
func bindingTreeFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // test temp tree
		require.NoError(t, err)
		files[path] = string(data)
		return nil
	}))
	return files
}

// TestBindingStoreRefusesUnsafeCwds checks a session cwd that cannot hold bindings: nothing is written,
// no directory is created, and every read finds no binding, which the routing treats as having none.
func TestBindingStoreRefusesUnsafeCwds(t *testing.T) {
	const runID = "feature-20261022-120000-abcd"
	tests := []struct {
		name    string
		setup   func(t *testing.T) (cwd, outside string) // outside is a directory a write must not reach
		wantErr string
	}{
		{name: "a removed thread worktree", wantErr: "no such file or directory", setup: func(t *testing.T) (string, string) {
			parent := t.TempDir()
			return filepath.Join(parent, "worktree"), parent
		}},
		{name: "a symlink to a removed thread worktree", wantErr: "no such file or directory",
			setup: func(t *testing.T) (string, string) {
				parent := t.TempDir()
				link := filepath.Join(t.TempDir(), "project")
				require.NoError(t, os.Symlink(filepath.Join(parent, "worktree"), link))
				return link, parent
			}},
		{name: "a file", wantErr: "is not a directory", setup: func(t *testing.T) (string, string) {
			path := filepath.Join(t.TempDir(), "file")
			require.NoError(t, os.WriteFile(path, nil, 0o600))
			return path, filepath.Dir(path)
		}},
		{name: "a symlinked .loopai", wantErr: "is a symbolic link", setup: func(t *testing.T) (string, string) {
			cwd, outside := t.TempDir(), t.TempDir()
			require.NoError(t, os.Symlink(outside, filepath.Join(cwd, ".loopai")))
			return cwd, outside
		}},
		{name: "a symlinked .loopai/acp", wantErr: "is a symbolic link", setup: func(t *testing.T) (string, string) {
			cwd, outside := t.TempDir(), t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(cwd, ".loopai"), 0o750))
			require.NoError(t, os.Symlink(outside, filepath.Join(cwd, ".loopai", "acp")))
			return cwd, outside
		}},
		{name: "a .loopai that is a file", wantErr: "is not a directory", setup: func(t *testing.T) (string, string) {
			cwd := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(cwd, ".loopai"), nil, 0o600))
			return cwd, t.TempDir()
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cwd, outside := tc.setup(t)
			// a binding planted where a followed link leads must not be read either
			if info, err := os.Stat(filepath.Join(cwd, ".loopai")); err == nil && info.IsDir() {
				require.NoError(t, os.MkdirAll(filepath.Join(outside, "acp"), 0o750))
				planted := `{"session_id":"s-1","run_id":"` + runID + `"}`
				for _, dir := range []string{outside, filepath.Join(outside, "acp")} {
					require.NoError(t, os.WriteFile(filepath.Join(dir, "binding-session-s-1.json"), []byte(planted), 0o600))
				}
			}
			before := bindingTreeFiles(t, outside)

			require.ErrorContains(t, saveSessionThread(cwd, "s-1", "thread-1"), tc.wantErr)
			require.ErrorContains(t, bindRun(cwd, "thread-1", "s-1", runID), tc.wantErr)
			require.ErrorContains(t, bindRun(cwd, "", "s-1", runID), tc.wantErr)
			require.ErrorContains(t, unbindRun(cwd, "thread-1", "s-1", runID), tc.wantErr)
			_, ok := loadBinding(cwd, "thread-1", "s-1")
			assert.False(t, ok)
			_, ok = loadBinding(cwd, "", "s-1")
			assert.False(t, ok, "a binding behind a link is not read")
			assert.Equal(t, before, bindingTreeFiles(t, outside), "nothing is written outside the cwd")
		})
	}

	t.Run("a removed cwd is never recreated", func(t *testing.T) {
		cwd := filepath.Join(t.TempDir(), "worktree")
		_ = saveSessionThread(cwd, "s-1", "thread-1")
		_ = bindRun(cwd, "thread-1", "s-1", runID)
		assert.NoDirExists(t, cwd)
	})
}

// TestBindingStoreAcceptsASymlinkedCwd checks a project T3 Code reaches through a symbolic link to its
// checkout: the cwd is that link, and its bindings are written inside the checkout and read back
// through the same path, so a restarted provider still finds the thread's run.
func TestBindingStoreAcceptsASymlinkedCwd(t *testing.T) {
	const runID = "feature-20261022-120000-abcd"
	checkout := t.TempDir()
	cwd := filepath.Join(t.TempDir(), "project")
	require.NoError(t, os.Symlink(checkout, cwd))

	require.NoError(t, requireBindingStore(cwd))
	require.NoError(t, saveSessionThread(cwd, "s-1", "thread-1"))
	require.NoError(t, bindRun(cwd, "thread-1", "s-1", runID))

	b, ok := loadBinding(cwd, "thread-1", "s-2")
	require.True(t, ok, "a replaced session finds the run through the thread")
	assert.Equal(t, runID, b.RunID)
	b, ok = loadBinding(cwd, "", "s-1")
	require.True(t, ok, "a session/load without a lookup finds the run through the session")
	assert.Equal(t, runID, b.RunID)
	assert.Equal(t, "thread-1", b.ThreadID)
	assert.Contains(t, bindingDirNames(t, checkout), "binding-session-s-1.json",
		"the binding lives in the checkout the link names")

	require.NoError(t, unbindRun(cwd, "thread-1", "s-1", runID))
	b, ok = loadBinding(cwd, "thread-1", "s-1")
	require.True(t, ok)
	assert.Empty(t, b.RunID)
}
