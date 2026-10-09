# Path containment checks reject plans in a checkout reached through a symlink

- found: 2026-10-09, plan: 20261009-acp-thread-chat, phase: task
- severity: minor
- area: pkg/t3/launch.go

`planRelPath` (`pkg/t3/launch.go:217`) makes the plan absolute with `filepath.Abs`, which takes the
shell's `$PWD` spelling, and compares it lexically with the repository root Git reports, which is
symlink-resolved. Running `loopai --t3-launch docs/plans/x.md` from a checkout reached through a
symlink (any path under macOS `/var` or `/tmp`, or a user symlink such as `~/src -> /Volumes/work`)
fails with `plan ... is outside the repository ...`.

`externalBackend` path relativization (`pkg/git/external.go:1754`) resolves only the parent
directory and falls back to the unresolved spelling when that directory does not exist, so a
`ShowFile` lookup for a path absent from the working tree (a report sidecar archived on the branch
only) reports "outside repository root" instead of "path not found" under the same conditions.

The `cmd/loopai` tests exposed both on macOS and now run with a symlink-resolved `TMPDIR`, matching
Linux CI, so they no longer cover this case.

Suggested fix: resolve the longest existing prefix of the path with `filepath.EvalSymlinks` (and the
root) before `filepath.Rel` in both places, and add a test that runs from a symlinked checkout path.
