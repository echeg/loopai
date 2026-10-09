# Report: Skill contract check and one-command install
plan: docs/plans/20261009-skill-contract-install.md · branch: echeg/skill-contract-install · base: master · mode: full · executor: claude · task model: claude:opus:high · review model: claude:opus:high · started: 2026-10-09T08:57:01Z · finished: 2026-10-09T10:30:49Z

## Summary
This branch gives each installed loopai part a way to detect when it doesn't match the others, and adds one command that updates all of them.

- **Contract number.** `cmd/loopai/skill_contract.go` adds a constant, `skillContract = 1`. The new flag `loopai --skill-contract` prints that number alone on stdout and exits 0 before config loading.
  - `versionBannerWriter` drops the pre-parse version banner when the flag appears before `--`. It uses a new `rawFlagRequested` helper, which `acpRequested` now uses too.
  - The flag is listed in `isStandaloneCommand`.
- **Fingerprint test.** `TestSkillContract` compares the surface that skills depend on against `cmd/loopai/testdata/skill-contract.golden`. That surface has three parts:
  - visible long flags, with their sorted `choice:` values;
  - keys of the embedded config, including commented `# key =` lines;
  - the `##` headings of the embedded `report.txt`, in file order.

  `config.EmbeddedDefault` is a new exported helper that reads embedded defaults. The test fails on any surface change unless `skillContract` is bumped or `-update` is run on purpose.
- **Skill snippet.** All 19 skills (10 Claude, 9 Codex) now open with the same snippet. It prints `skill contract: skill=<N> binary=<M>`, followed by an instruction to warn and continue, never stop.
  - The canonical text lives in `scripts/lib/skill-contract.sh`. `check-symlinks.sh` and `check-codex-skills.sh` both enforce it, including the `expected=` value.
  - `scripts/check-skill-contract-snippet_test.sh` runs the snippet against stub binaries.
- **`make install`.** It runs `scripts/install.sh`, which:
  - builds;
  - installs `loopai` and `loopai-acp` into `${LOOPAI_BIN_DIR:-~/.local/bin}`;
  - updates the `loopai` marketplace and each installed `@loopai` plugin;
  - reinstalls the Codex skills;
  - checks the installed binary's contract number and the plugin version;
  - warns about a shadowing `loopai` on `PATH` and about standalone `~/.claude/skills/loopai*` copies.
- **`loopai-update`.** Both copies now start with an installation step:
  1. find the checkout;
  2. fetch, and offer `git pull --ff-only` only when upstream is ahead;
  3. run `make -C <checkout> install`;
  4. merge prompts using the binary that was just installed.
- **Documentation.** `CLAUDE.md`, `README.md`, and `llms.txt` are updated.

Phase durations, up to the end of finalize (before report assessment and archival):

| phase | duration_ms | approx. |
|---|---:|---|
| tasks | 3781343 | 63m01s |
| internal review | 1290292 | 21m30s |
| evaluation | 420084 | 7m00s |
| external review | 136266 | 2m16s |
| other | 0 | 0s |

Run shape:
- 8 task iterations with 0 failed retries.
- Internal review: 1 loop iteration, ended by `review_done`.
- Post-review: 1 iteration.

## Change scope
- **Diff:** 48 files, +2067 / −139.
- **Added (7):**
  - `cmd/loopai/skill_contract.go`
  - `cmd/loopai/skill_contract_test.go`
  - `cmd/loopai/testdata/skill-contract.golden`
  - `scripts/check-skill-contract-snippet_test.sh`
  - `scripts/install.sh`
  - `scripts/install_test.sh`
  - `scripts/lib/skill-contract.sh`
- **Modified (41):**
  - **Go:** `cmd/loopai/acp.go`, `cmd/loopai/main.go`, `cmd/loopai/main_test.go`, `cmd/loopai/pr_report_test.go`, `pkg/config/defaults.go`, `pkg/config/defaults_test.go`, `pkg/git/external.go`, `pkg/t3/launch.go`, `pkg/t3/launch_test.go`
  - **Skills:** all 10 `assets/claude/skills/*/SKILL.md` and all 9 `assets/codex/skills/*/SKILL.md`
  - **Checkers:** `scripts/check-symlinks.sh`, `scripts/check-symlinks_test.sh`, `scripts/check-codex-skills.sh`, `scripts/check-codex-skills_test.sh`
  - **Build and CI:** `Makefile` (new targets `test-skill-contract`, `install`, `test-install`, wired into `test` and `.PHONY`), `.github/workflows/ci.yml` (asset-check step gains `test-skill-contract test-install`)
  - **Manifests:** `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json`, both 0.7.1 → 0.8.1
  - **Docs:** `CLAUDE.md`, `README.md`, `llms.txt`, and the plan file
  - **Other test:** `plugins/codex-imagegen/.../codex_image_test.py`
- **Commits (10, oldest first):**
  1. `abe4a94` feat: add skillContract and the --skill-contract flag
  2. `b3f8588` feat: add the contract surface fingerprint test
  3. `2170fce` feat: add the shared skill contract snippet to every skill
  4. `1d47c80` feat: enforce the skill contract snippet in the checkers
  5. `8c0ff9a` feat: add scripts/install.sh and make install
  6. `370c856` feat: teach loopai-update to run the installer
  7. `098afef` feat: verify skill contract acceptance criteria
  8. `afa6f59` feat: document the skill contract and make install
  9. `f0032fb` fix: address code review findings
  10. `d89b734` fix: address codex review findings

## Risk
**low**

- **Public APIs.** The CLI change is additive: one new standalone flag, `--skill-contract`.
  - `--version` output and the normal banner are unchanged. The banner is dropped only when `--skill-contract` appears before `--`.
  - One exported Go function was added, `config.EmbeddedDefault`.
  - The `classifyNoCommitMerge` change in `pkg/git/external.go` is limited to an internal closure, which now returns only the error. Every caller still gets the same `(MergeResult{}, err)` pair.
- **Data schemas.** None changed. No run record, checkpoint, progress-log, or launch-history format was touched. Protocol signals are deliberately kept out of the fingerprint.
- **Configuration.** No config keys were added, removed, or renamed. The golden file only reads the existing keys. Removed hidden spellings stay excluded.
- **Concurrency.** No goroutine, lock, or process-lifecycle code changed. `install.sh` runs sequentially, and its tests use a copied repo tree with a stubbed `PATH`.
- **Migrations.** None in code.
- **Behavior changes worth watching:**
  - `pkg/t3` `planRelPath` now retries with symlinks resolved, but only when the plain relative check fails. This fixes `--t3-launch` for a checkout entered through a symlink and doesn't affect paths that already worked.
  - Each skill's first step now runs a shell snippet. A mismatch only warns, so skill flows aren't blocked.
  - `make install` writes outside the repository: `~/.local/bin`, Claude plugin state, and `~/.codex/skills`. It is an explicit, opt-in command, never deletes standalone skill copies, and exits non-zero only on a failed build, a failed binary install, or a contract mismatch.

## Migrations and operational steps
- Run `make install` (or `/loopai:loopai-update`) so the installed binary answers `--skill-contract`. Until then, an older binary reports as contract 0 and every skill prints the "binary is older" warning, though it continues.
- After installing, run `/reload-plugins` or restart open Claude Code sessions, and restart open Codex sessions, to pick up plugin 0.8.1 and the updated Codex skills.
- Optional: remove standalone `~/.claude/skills/loopai*` copies using the `rm -rf` line the installer prints. The installer never deletes them.

## Plan deviation
All eight tasks are checked off.

**Added items:**
- **macOS temp-path failures (pre-existing).** `make test` surfaced failures caused by the `/var` vs `/private/var` mismatch. They were fixed in `pkg/t3` `planRelPath` (a real bug for symlinked checkouts), in the `TestLocateCompletionReport` and `TestRunFinalizeCloseout` expectations, and in `codex_image_test.py`.
- **`unparam` lint (pre-existing).** Fixed in `pkg/git/external.go` `classifyNoCommitMerge`.
- **Shared snippet location.** The canonical snippet lives in `scripts/lib/skill-contract.sh`, sourced by both checkers and both fixture suites. The plan had left the choice open. `test-skill-contract` was also added to the CI asset-check step.
- **`install_test.sh` design:**
  - `PATH` is built from stubs plus `/usr/bin:/bin` and does not include the real `PATH`.
  - The installer runs from a copied repo tree.
  - It stubs `install-codex-skills.sh` rather than `codex` and `go`.
  - Plugin versions are read from `claude plugin list --json`, preferring `folderVersion`.
  - Extra cases cover the default bin dir, a failed plugin update, a plugin version mismatch, an old binary without the flag, and a bin dir missing from `PATH`.
- **`loopai-update` extras.** The step also fetches before comparing with upstream, refuses any pull other than `--ff-only`, and continues to the prompt merge after a failed install only while a binary is still installed. Both summaries gain Installation and Skill contract lines, and Constraints/SCOPE now allow Step 0's writes outside the config directory.

**Blocked:** none.

**Skipped:** the original Task 5 checkbox for `scripts/install_test.sh` — stub `make`, `claude`, `codex`, `go` with the real `PATH` appended. It is checked in the plan, but it was implemented in the modified form described by the "`install_test.sh` design" item above, not as written.
- Every case it listed exists:
  - `success`
  - `build-failure`
  - `no-claude`
  - `not-installed`
  - `contract-mismatch`
  - `shadowed`
  - `standalone`
  - `no-codex`
- Further cases were added, for 19 in total. The intent is met; only the stubbing and `PATH` approach differ.

**Other differences:**
- The plan targeted manifest version 0.8.0. The branch ends at 0.8.1, because the review-fix commit `f0032fb` changed skills and bumped both manifests again. They stay equal to each other.
- The golden's flag section also records sorted `choice:` values for flags that have them, such as `--finalize [merge|none|pr|sync]`. The plan listed only long names. `CLAUDE.md` describes this extended form.

## Backlog
None. No backlog entries were filed during this run.

## External review
### codex:gpt-6-astra:high
- label: codex
- iterations: 2
- duration_ms: 556319
- ended by: done
- had findings: true

Findings:
- **[P2] Use the newly installed binary for prompt merging -> fixed.**
  - The finding: Step 1 of both `loopai-update` skills ran a bare `loopai --dump-defaults`. An older shadowing binary, or an install directory missing from `PATH`, would break or skew the prompt merge.
  - The fix: Step 0 now takes the binary path from the installer's `installed loopai and loopai-acp into <dir>` line. Step 1 runs `"$LOOPAI_BIN" --dump-defaults`. `check-skill-contract-snippet_test.sh` now requires that form in both copies. Committed in `d89b734`.
  - Iteration 2 reported `NO ISSUES FOUND` and confirmed the fix.

## Validation
- Commands: none recorded. The plan has no `## Validation Commands` section.
- Timings: duration_ms 0, runs 0.