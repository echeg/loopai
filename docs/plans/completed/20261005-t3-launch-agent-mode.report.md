# Report: --t3-launch starts a T3 Code provider session by default
Plan: `C:\Users\necro\.t3\worktrees\loopai\t3-launch-agent-mode\docs\plans\20261005-t3-launch-agent-mode.md` | Branch: `t3-launch-agent-mode` | Base: `master` | Mode: `full` | Executor: `codex` | Task model: `codex:gpt-6.1-sol:high` | Review model: `codex:gpt-6-astra:high` | Started: `2026-10-05T20:11:18Z` | Finished: `2026-10-05T21:59:35Z`

## Summary

Delivered `--t3-launch[=auto|agent|terminal]`. Bare/auto launches a T3 Code provider session when an enabled loopai provider instance is configured; otherwise it preserves terminal launching. Explicit agent mode requires an instance, while terminal mode bypasses provider discovery.

Agent launches carry the plan and local overrides into the worktree and dispatch `thread.turn.start`, enabling provider-session progress, reasoning, completion messages, and cancellation. They open no terminal and place no bearer token in the thread. Settings discovery, CLI reporting, both skills, documentation, and regression coverage were updated.

| phase | duration_ms |
|---|---:|
| evaluation | 1648900 |
| external review | 415482 |
| internal review | 653874 |
| other | 10 |
| tasks | 3778706 |

Task iterations: 7; task failed retries: 0. Internal review first ran: true; internal review loop iterations: 1; internal review ended by: `review_done`. Post-review ran: true; post-review iterations: 1.

The finish timestamp and timing snapshot precede report assessment and archival. Finalize was not run; no finalize base sync occurred. Scope was assessed using `git diff master...HEAD`.

## Change scope

30 files changed: 1274 additions and 218 deletions, comprising 27 modified files and 3 added files.

- T3 integration: modified `pkg/t3/client.go`, `client_test.go`, `launch.go`, `launch_test.go`, `reporter_test.go`, and `runtime.go`; added `settings.go` and `settings_test.go`.
- CLI and ACP: modified `cmd/loopai/main.go`, `main_test.go`, `acp.go`, and `acp_test.go`, plus `cmd/loopai-acp/main.go` and `main_test.go`.
- Skills and packaging: modified both `assets/{claude,codex}/skills/loopai-t3/SKILL.md` files, `.claude-plugin/marketplace.json`, `.claude-plugin/plugin.json`, and `Makefile`; added `scripts/check-loopai-t3-skill_test.sh`. Plugin version advanced to `0.5.13`.
- Documentation: modified `CLAUDE.md`, `README.md`, `docs/t3-code.md`, `llms.txt`, and the implementation plan.
- Validation portability: modified `pkg/executor/codex_test.go`, `pkg/git/service_test.go`, `scripts/check-symlinks.sh`, `scripts/check-symlinks_test.sh`, and `scripts/pi-as-claude/pi-as-claude_test.sh`.

Nine commits:

| Commit | Description |
|---|---|
| `5327c1dd14d0d708870ce271cb3d4ef358cc87d1` | fix: address external review findings |
| `c58ffc6483dfc42e962ec2a306c6741727738901` | fix: address code review findings |
| `436e193ffad7c31a6173b8a0d794d3c46a30fe6c` | feat: document automatic T3 provider-session launches |
| `d312910684e81b0e8cbd7741160797a52eac8f2a` | feat: verify T3 launch mode acceptance criteria |
| `304218a873e6d5139cab6996a919d7cd18c20467` | feat: document T3 launch modes in skills |
| `4652d6a6d5aacd1f0d10cb27729910d4239b59a1` | feat: wire T3 launch modes through the CLI |
| `f91d6fff7ec7886ecc470c13f03457395dab684b` | feat: launch T3 provider sessions in agent mode |
| `bb20a7b900eea9c70a253efaea3e1caf33f1ad33` | feat: add T3 provider turn start command |
| `8e633af66878901b3b1cacb01d219eb1899f3d7f` | feat: discover loopai provider instances in T3 settings |

## Risk

**medium** — The public CLI changes its default behavior when a usable provider instance exists, and the integration now depends on T3 Code’s public `thread.turn.start` API and provider-instance contract. Go API additions expose launch modes, provider discovery, and turn construction.

No persisted data schema changes or migrations are introduced. Configuration is read-only, size-limited, and selected deterministically; malformed settings cause an auto-mode warning and terminal fallback, or an explicit-agent error. Agent and terminal execution are mutually exclusive, avoiding concurrent runs in one worktree. Reporter synchronization changes affect tests only.

Remaining operational risks include T3 contract changes, agent-mode rejection of whitespace-containing plan paths, and partial launches leaving a worktree and thread for recovery. Live end-to-end verification is not recorded.

## Migrations and operational steps

- Database and data migrations: none.
- Reinstall the plugin and run `make install-codex-skills` to refresh installed skills.
- To enable provider sessions where absent, configure an enabled `grok` provider instance pointing to `loopai-acp` or `loopai-acp.exe` in `<T3 home>/userdata/settings.json`. Existing terminal users require no configuration change.
- Perform the plan’s live verification of progress, reasoning, completion messages, input carryover, and title generation.
- Reverify the `thread.turn.start` contract after the next T3 Code update.

## Plan deviation

All seven implementation tasks are marked complete, and the implementation follows the planned discovery, dispatch, launch-mode, CLI, skill, acceptance, and documentation work.

- Added: “➕ synchronize `TestReporterExistingThread` with the completed pin operation before” — completed by waiting for the pin operation before stopping the reporter, eliminating the acceptance-test race.
- Additional validation repairs addressed Git worktree error wording and cancellation synchronization, filesystem timestamp resolution, jq stream handling, and Windows symlink targets without a leading `./`.
- Review corrections aligned the initial agent title with `titleSeed`, made launcher-name matching case-insensitive, and protected hyphen-prefixed plan paths in ACP prompts.
- Blocked items: none.
- Skipped items: none.

The plan’s manual post-completion checks remain unverified in the supplied record. Finalize was not run.

## Backlog

There were no backlog entries or backlog files.

## External review

### claude:opus:high

- label: claude
- iterations: 3
- duration_ms: 2064281
- ended by: done
- had findings: true

Iteration 1 — truncated: false

- Agent-mode thread title remains `<plan> · starting` because it does not match `titleSeed` (medium) -> fixed.
- `binaryPath` launcher-name matching is case-sensitive, missing valid Windows names (low) -> fixed.

Iteration 2 — truncated: false

The reviewer reported **NO ISSUES FOUND** and confirmed both fixes.

- Evaluator-discovered Windows symlink target mismatch caused by the optional leading `./` -> fixed.

Iteration 3 — truncated: false

The reviewer reported **NO ISSUES FOUND**, confirming the title, case-insensitive discovery, and symlink fixes. Evaluation ended with the external-review completion marker.

No findings were dismissed or filed to backlog. The reviewer’s `duration_ms: 2064281` and the phase timing `external review: 415482` are separate supplied measurements.

## Validation

Supplied validation commands:

- `make test`
- `make lint`

Supplied validation timings:

- duration_ms: 0
- runs: 0

These counters record no timed validation runs. Review narratives separately report successful targeted T3 and CLI tests, race-enabled T3 tests, Linux T3/CLI package tests, repository-wide Go lint, symlink checks and regression coverage, shell syntax checks, and `git diff --check`.

Explicit reviewer commands reported passing:

- `go test ./pkg/t3/ -run 'TestFindLoopaiInstance|TestLaunch' -count=1`
- `bash scripts/check-symlinks.sh`

Validation limitations: the native Windows CLI suite encountered Unix-dependent fixture failures and timed out; a Linux rerun passed. A later full `make test` stopped at the symlink failure; the corrected focused suite passed. WSL subsequently could not start, and no successful full-suite rerun after that correction is recorded.