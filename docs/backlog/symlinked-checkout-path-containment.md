# Path containment checks reject paths in a checkout reached through a symlink

- found: 2026-10-09, plan: 20261009-acp-thread-chat, phase: task
- severity: minor
- area: pkg/git/external.go

`externalBackend.toRelative` (`pkg/git/external.go`) resolves only the parent directory and falls
back to the unresolved spelling when that directory does not exist, so a `ShowFile` lookup for a
path absent from the working tree (a report sidecar archived on the branch only) reports "outside
repository root" instead of "path not found" when the checkout is reached through a symlink (any
path under macOS `/var` or `/tmp`, or a user symlink such as `~/src -> /Volumes/work`).

The `cmd/loopai` tests exposed it on macOS and now run with a symlink-resolved `TMPDIR`, matching
Linux CI, so they no longer cover this case.

Suggested fix: resolve the longest existing prefix of the path with `filepath.EvalSymlinks` (and the
root) before `filepath.Rel`, and add a test that runs from a symlinked checkout path.
