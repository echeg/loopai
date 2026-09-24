# cmux hand-off spawns a workspace for a config the child rejects

- found: 2026-09-24, plan: 20260924-explicit-provider-specs, phase: internal review
- severity: minor
- area: cmd/loopai/main.go

`handOffToCmuxWorkspace` (`cmd/loopai/main.go:4252`) refuses to spawn for an unusable
executable, a non-root working directory, and an unreadable plan file, because each would
otherwise fail only in the child after the workspace was created and focused, while the
originating terminal prints `handed off to cmux workspace` and exits 0. Config is not
checked: wherever `.git` exists the hand-off stays config-independent (`:4286`), so a config
the child cannot load reaches `cmux.SpawnWorkspace` (`:4309`) and leaves an orphan card.

The explicit-provider-specs change made that case common rather than rare: every copied
config that still sets a removed key (`checkRemovedKeys`, `pkg/config/values.go:889`) or an
unprefixed `task_model`/`review_model`/`plan_model` (`validateModelSpecs`,
`cmd/loopai/main.go:3036`) now fails at startup, so the first `--cmux-workspace` run of an
unmigrated user creates a card that dies immediately.

Suggested direction: before spawning, parse config with `config.LoadReadOnly` (already used
by `handOffAllowedOutsideRepo` at `:4462` and `preserveAPIKeyRequested`) and refuse the
hand-off with the loader's error when it fails, the same way `planFileRefusal` refuses an
unreadable plan. Optionally also run the spec-grammar part of `validateModelSpecs` on the
loaded values. This only reads config; it does not move executor resolution ahead of the
hand-off, so the documented "hand-off before config loading and executor resolution" contract
still holds for everything that can differ between the two terminals.
