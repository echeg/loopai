# Skill contract check and one-command install

## Overview
- A completion report produced by a stale loopai binary (`master-75ed2fd`, built 2026-09-25) silently
  lacked the Evidence and Merge danger sections (with `Door`/`Blast radius`) that commit `36e57f8`
  (2026-10-05) added to the embedded `report.txt`. Nothing warned that the installed binary and the
  skills disagreed about the report contract.
- This plan adds an explicit **skill contract number**: the binary reports it through
  `loopai --skill-contract`, every Claude and Codex skill carries the number it was written for and
  warns when the two differ, and a test fingerprints the binary surface skills depend on so the number
  cannot be forgotten.
- It also adds `make install`, which updates every installed channel (binary, ACP launcher, Claude
  plugin, Codex skills) and verifies the result, and makes `/loopai:loopai-update` (`$loopai-update`
  in Codex) run it before its existing prompt merge, so one command updates everything.

## Decisions
- **Context**: five install channels age independently — binary `~/.local/bin/loopai` (+ `loopai-acp`),
  the Claude plugin `loopai@loopai`, standalone copies `~/.claude/skills/loopai*`, Codex skills
  `~/.codex/skills/loopai*`, and commented prompt copies in `~/.config/loopai/prompts/`. The binary
  exposes only a `revision` string, so no component can tell whether another one is compatible.
- **Chosen approach**:
  - Detect *incompatibility* only, through a manually bumped integer `skillContract` in `cmd/loopai`.
    All-old-but-consistent installs stay silent.
  - Binary side: standalone flag `--skill-contract` prints the integer alone on stdout and exits 0.
    An old binary answers `unknown flag` with exit 1, which the skill reads as contract 0 — the
    `75ed2fd` binary would have been caught immediately. `--version` and the banner stay unchanged
    (the user's `/build_and_isntall` compares those strings).
  - Fingerprint guard: `TestSkillContract` renders a readable canonical surface — long flag names from
    `opts` (hidden removed flags excluded), keys of embedded `pkg/config/defaults/config`, and `##`
    headings of embedded `report.txt` — and compares it with the golden
    `cmd/loopai/testdata/skill-contract.golden` (`contract: N` plus the surface text). A changed surface
    fails `make test` with the instruction to bump `skillContract` if skills are affected, then rerun
    with `-update`. `-update` may accept a surface change without a bump; that is a deliberate,
    diff-visible decision.
  - Skill side: one byte-identical snippet as the first step of all 10 Claude skills and 9 Codex skills.
    Mismatch warns and continues, never stops. `loopai-update` reports the line in its summary instead.
    The two existing checkers enforce snippet presence and `expected=` equal to `skillContract`.
  - Install: `scripts/install.sh` behind `make install` — build, install binaries, update the plugin via
    `claude plugin marketplace update loopai` / `claude plugin update`, reinstall Codex skills, verify
    the contract, warn about standalone copies. `loopai-update` locates the checkout and runs it, never
    pulling without consent.
- **Rejected alternatives**:
  - lag-behind-repo detection: warns after every commit and works only where a checkout exists;
  - binary scanning installed skill directories at startup: couples the binary to foreign layouts;
  - doctor-only command: depends on the user remembering to run it;
  - install steps written as model instructions inside `loopai-update`: nondeterministic, duplicated in
    the Codex copy, untestable;
  - a separate `loopai-upgrade` skill: two commands instead of one;
  - auto-refreshing or deleting standalone `~/.claude/skills/loopai*` copies: foreign directories with
    no baseline that may hold user edits;
  - plugin version embedded at build as the contract: every skill typo fix warns until a rebuild,
    which is lag detection again;
  - discipline-only bumps: would have missed the Merge danger change;
  - the contract number inside `--version` output: needs parsing and changes the compared line;
  - a hash-only golden: unreadable diffs.
- **Verified facts**:
  - `resolveVersion` and `revision` live in `cmd/loopai/main.go:220-242`; `main` prints the banner
    `loopai <rev>` to `versionBannerWriter(os.Args[1:])` (`cmd/loopai/acp.go:28`) *before* flag
    parsing, on stdout except under `--acp`. `--skill-contract` must therefore suppress that banner (or
    move it to stderr) so stdout carries only the integer.
  - `isStandaloneCommand` is at `cmd/loopai/main.go:5193`; removed flags are declared with
    `hidden:"true"` (`--codex`, `--codex-only`, `--external-review-tool`, `--external-review-model`).
  - Marketplace `loopai` is a `directory` source pointing at this checkout
    (`~/.claude/plugins/known_marketplaces.json`), so plugin skills are read live from the repo;
    `installed_plugins.json` still says 0.5.5 while `claude plugin list` reports 0.7.1.
  - Non-interactive plugin commands exist: `claude plugin marketplace update <name>`,
    `claude plugin update <plugin>`, `claude plugin list`.
  - Claude skill inventory (`scripts/check-symlinks.sh:9`): loopai, loopai-adopt, loopai-brainstorm,
    loopai-grill, loopai-merge, loopai-orca, loopai-plan, loopai-retro, loopai-t3, loopai-update.
    Codex inventory: the same minus loopai-grill.
  - Report headings are also hard-coded in `pkg/processor/completion_report.go` (fallback) and
    `cmd/loopai/pr_body.go:111`; the fingerprint reads the embedded prompt, which is what the model is
    asked to produce.
  - `make test` needs closed stdin (`</dev/null`); stub subprocess spawns are slow under the full suite.
- **Non-goals**: the binary does not inspect skills; skills never stop on mismatch; no lag detection; no
  automatic `git pull`; no automatic deletion of standalone copies; `<<<RALPHEX:...>>>` signals and the
  launch-history format stay out of the fingerprint (launch-history has its own executable contract
  test); `~/.claude/commands/build_and_isntall.md` is the user's file and stays untouched.
- **Success criteria**: a binary without `--skill-contract` triggers the warning in every skill; after
  `make install` on this machine no skill warns; editing a `##` heading in `report.txt` without bumping
  fails `make test`; `make test` and `make lint` pass.

## Context (from discovery)
- Files/components involved:
  - `cmd/loopai/main.go` (opts, banner, standalone routing), `cmd/loopai/acp.go` (`versionBannerWriter`)
  - new `cmd/loopai/skill_contract.go`, `cmd/loopai/skill_contract_test.go`,
    `cmd/loopai/testdata/skill-contract.golden`
  - `assets/claude/skills/*/SKILL.md` (10), `assets/codex/skills/*/SKILL.md` (9)
  - `scripts/check-symlinks.sh`, `scripts/check-symlinks_test.sh`, `scripts/check-codex-skills.sh`,
    `scripts/check-codex-skills_test.sh`, new `scripts/check-skill-contract-snippet_test.sh`
  - new `scripts/install.sh`, `scripts/install_test.sh`; `Makefile`
  - `.claude-plugin/plugin.json`, `.claude-plugin/marketplace.json`
  - `CLAUDE.md`, `README.md` (`## Installation` at line 55), `llms.txt`
- Related patterns found:
  - `scripts/check-launch-history-skills_test.sh` already enforces a byte-identical shared snippet across
    six skills and executes it against fixtures — the model for the new snippet test.
  - `scripts/install-codex-skills.sh` + `_test.sh` already install into a redirected `CODEX_HOME`.
  - Standalone flags such as `--version`, `--init`, `--dump-defaults` route before config loading.
- Dependencies identified: none new; shell scripts need Bash and `jq` like the existing checkers.

## Development Approach
- **Testing approach**: Regular (code first, then tests in the same task)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional - they are a required part of the checklist
  - write unit tests for new functions/methods
  - write unit tests for modified functions/methods
  - add new test cases for new code paths
  - update existing test cases if behavior changes
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- Run tests after each change (`make test </dev/null`)
- Maintain backward compatibility
- Tests must redirect `HOME` and `USERPROFILE` (and `CODEX_HOME`, `LOOPAI_BIN_DIR`) to temp dirs and never
  touch the real `~/.config/loopai/`, `~/.claude/`, `~/.codex/`, or `~/.local/bin/`.

## Testing Strategy
- **Unit tests**: Go tests in `cmd/loopai` for the flag, banner suppression, and fingerprint golden.
- **Shell suites**: snippet execution against stub `loopai` binaries; checker fixtures; `install.sh`
  against stub `make`/`claude`/`codex` in a temp `HOME`. All wired into `make test`.
- **E2E tests**: not affected (no dashboard change).

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## What Goes Where
- **Implementation Steps** (`[ ]` checkboxes): tasks achievable within this codebase - code changes, tests, documentation updates
- **Post-Completion** (no checkboxes): items requiring external action - manual testing, changes in consuming projects, deployment configs, third-party verifications
- **Checkbox placement**: Checkboxes belong only in Task sections (`### Task N:` or `### Iteration N:`). Do not put checkboxes in Success criteria, Overview, Context, Decisions, or Decision Log — they cause extra loop iterations.

## Decision Log
- 2026-10-09 loopai-brainstorm: **accepted** - incompatibility-only detection through a manual contract number
- 2026-10-09 loopai-brainstorm: **rejected** - "warn when any copy lags the repo checkout" - noisy after every commit, needs a local checkout
- 2026-10-09 loopai-brainstorm: **accepted** - the check runs in skills only
- 2026-10-09 loopai-brainstorm: **rejected** - "binary scans installed skills at startup" - couples the binary to foreign directory layouts
- 2026-10-09 loopai-brainstorm: **rejected** - "doctor-only command" - relies on the user remembering it
- 2026-10-09 loopai-brainstorm: **accepted** - `make install` does the work, `loopai-update` runs it
- 2026-10-09 loopai-brainstorm: **rejected** - "all install steps as skill instructions" - nondeterministic, duplicated in Codex, untestable
- 2026-10-09 loopai-brainstorm: **rejected** - "separate loopai-upgrade skill" - two commands instead of one
- 2026-10-09 loopai-brainstorm: **accepted** - standalone `~/.claude/skills/loopai*` copies are warned about, never refreshed or deleted
- 2026-10-09 loopai-brainstorm: **accepted** - surface fingerprint guard on the contract number
- 2026-10-09 loopai-brainstorm: **rejected** - "discipline-only bumps" and "plugin version as contract" - miss the Merge danger case or degrade into lag detection
- 2026-10-09 loopai-brainstorm: **accepted** - separate `--skill-contract` flag; fingerprint = flags + config keys + report headings, signals excluded

## Implementation Steps

### Task 1: Add skillContract and the --skill-contract flag
- [x] create `cmd/loopai/skill_contract.go` with `const skillContract = 1` and a short comment saying what the number guards and when to bump it
- [x] add `SkillContract bool \`long:"skill-contract" description:"print the skill contract number and exit"\`` to `opts` in `cmd/loopai/main.go`; handle it right after parsing, beside `o.Version`, printing exactly `<N>\n` to stdout and exiting 0 before config loading, repository checks, cmux/orca/T3 setup, or keep-awake
- [x] suppress the pre-parse version banner when `--skill-contract` is present in raw args (extend `versionBannerWriter` or add a sibling raw-args check honoring `--` like `acpRequested`), so stdout carries only the integer
- [x] include `o.SkillContract` in `isStandaloneCommand` and reject it combined with plan arguments or other modes the same way `--version` combinations are handled (verify existing behavior first and match it)
- [x] write tests: output is exactly the integer with no banner; exit path skips config loading (temp `HOME`, no config present); `--` before the flag disables the banner suppression check; combination with a plan argument behaves as decided (like `--version`: ignored, prints the integer and exits 0)
- [x] ➕ fix pre-existing macOS failures surfaced by `make test` (temp dir `/var` vs resolved `/private/var`): `pkg/t3` `planRelPath` now resolves symlinks on both sides (a real bug for checkouts entered through a symlink), `TestLocateCompletionReport`/`TestRunFinalizeCloseout` expectations use the resolved root, and `codex_image_test.py` compares against the resolved `CODEX_HOME`
- [x] ➕ fix pre-existing `unparam` lint in `pkg/git/external.go` `classifyNoCommitMerge` (`abandon` returns only the error)
- [x] run `make test </dev/null` and `make lint` - must pass before task 2

### Task 2: Add the contract surface fingerprint test
- [x] in `cmd/loopai/skill_contract.go` add `skillContractSurface() (string, error)` rendering three sorted sections: `flags:` (long names from the go-flags parser over `opts`, skipping hidden options), `config keys:` (keys from embedded `pkg/config/defaults/config`, commented `# key = value` lines included, since the defaults file documents keys commented out), `report headings:` (`## ` lines of the embedded `report.txt` with its `# ` comment prefix stripped, in file order)
- [x] create `cmd/loopai/testdata/skill-contract.golden` with `contract: 1` on the first line followed by the rendered surface
- [x] write `TestSkillContract` in `cmd/loopai/skill_contract_test.go`: fails when the golden `contract:` differs from `skillContract`; fails when the surface differs, printing a unified diff plus "contract surface changed; bump skillContract if skills depend on the change, then run go test ./cmd/loopai -run TestSkillContract -update"; a package-level `-update` flag rewrites the golden with the current constant and surface
- [x] write tests for the surface renderer: hidden removed flags (`--codex`, `--codex-only`) absent; a known key (`task_model`) and heading (`Merge danger`) present; output deterministic across calls
- [x] run `make test </dev/null` and `make lint` - must pass before task 3

### Task 3: Add the shared contract snippet to every skill
- [x] define the snippet once (in this plan's Technical Details) and insert it as the first step of all 10 `assets/claude/skills/*/SKILL.md`, byte-identical, inside a fenced bash block followed by the identical instruction paragraph; for `loopai-update` add its variant sentence that it reports the line in its summary instead of warning
- [x] insert the same snippet and instruction (with `$loopai-update` wording) into all 9 `assets/codex/skills/*/SKILL.md`; keep the Codex wording free of Claude-only constructs so `check-codex-skills.sh` passes
- [x] verify `loopai-grill` still satisfies `scripts/check-grill-skill_test.sh` (it pre-approves no tools; the snippet needs Bash, which the skill already uses for its helpers) and update that test only if a contract it checks legitimately moved
- [x] bump `.claude-plugin/plugin.json` and the `loopai` entry in `.claude-plugin/marketplace.json` to the same new version (0.8.0)
- [x] run the existing skill suites (`make test-symlinks test-codex-skills test-merge-skill test-retro-skill test-launch-history-skills test-report-docs test-grill-skill </dev/null`) - must pass before task 4

### Task 4: Enforce the snippet in the checkers
- [x] in `scripts/check-symlinks.sh` read `skillContract` from `cmd/loopai/skill_contract.go` (`grep`/`sed`, fail loudly when not found) and require every Claude skill to contain the canonical snippet with `expected=<N>`; the canonical text lives in one place the checker reads (a shared `scripts/lib/skill-contract-snippet.txt` or a heredoc in one script sourced by both checkers)
- [x] apply the same check in `scripts/check-codex-skills.sh` for every Codex skill
- [x] extend `scripts/check-symlinks_test.sh` and `scripts/check-codex-skills_test.sh` fixtures: valid fixture carries the snippet; new failing fixtures for a missing snippet, a modified snippet, and a stale `expected=` value
- [x] create `scripts/check-skill-contract-snippet_test.sh` that extracts the snippet from one skill, executes it with `PATH` pointing at stub `loopai` scripts, and asserts the printed line for: loopai absent (`missing`), flag unknown (exit 1 → `0`), non-numeric output (→ `0`), older, equal, and newer contracts; add a `test-skill-contract` Makefile target and include it in `make test`
- [x] ➕ the shared canonical snippet lives in `scripts/lib/skill-contract.sh` (sourced by both checkers and both fixture suites); `test-skill-contract` also added to the CI asset-check step
- [x] run `make test </dev/null` - must pass before task 5

### Task 5: Add scripts/install.sh and make install
- [x] create `scripts/install.sh` (Bash, `set -euo pipefail`, repo root resolved from the script path) implementing the seven steps from Technical Details with one `✓`/`!` line per step and a final summary
- [x] add `install:` target to `Makefile` calling it, and add it to `.PHONY`
- [x] make step 5 compare `<bin_dir>/loopai --skill-contract` with the constant read the same way as the checkers, compare `claude plugin list` output for `loopai@loopai` with `plugin.json` version, and warn when `command -v loopai` resolves to a different path than `<bin_dir>/loopai`
- [x] create `scripts/install_test.sh` with temp `HOME`, `CODEX_HOME`, `LOOPAI_BIN_DIR`, and stub `make`, `claude`, `codex`, `go` in `PATH` (keep the real `PATH` appended); cases: success path, build failure (nothing installed, non-zero exit), no `claude` on PATH (`!`, exit 0), plugin not installed (prints install command, does not call `install`), contract mismatch (non-zero exit), shadowing `loopai` earlier in `PATH` (warning), standalone `~/.claude/skills/loopai-merge` present (warning with `rm -rf` line, directory still exists), missing Codex home (skipped)
- [x] wire `scripts/install_test.sh` into `make test`; ensure it runs with closed stdin and generous timeouts for stub spawns (the suite is synchronous with no waits; `test-install` closes stdin and is also in the CI asset-check step)
- [x] ➕ deviation: `install_test.sh` builds PATH from stubs plus `/usr/bin:/bin` instead of appending the real PATH, because a real `claude` or installed `~/.local/bin/loopai` would leak into the cases the installer inspects; it runs the installer from a copied repo tree so the stub `make` never writes the real `.bin/`, and stubs `install-codex-skills.sh` instead of `codex`/`go`, which the installer never calls; plugin versions come from `claude plugin list --json` (`folderVersion` for directory-sourced plugins, which is what Claude Code reads); extra cases cover the default `~/.local/bin`, a failed plugin update, a plugin version mismatch, an old binary without the flag, and a bin dir missing from PATH
- [x] run `make test </dev/null` and `make lint` - must pass before task 6

### Task 6: Teach loopai-update to run the installer
- [ ] in `assets/claude/skills/loopai-update/SKILL.md` replace Step 0's clone-only guidance with an "update installation" step: locate the checkout (`$LOOPAI_SRC`, else the `loopai` marketplace `path` in `~/.claude/plugins/known_marketplaces.json` when its source type is `directory`, else AskUserQuestion), show `git -C <checkout> status -sb`, offer `git pull --ff-only` through AskUserQuestion only when upstream is ahead, run `make -C <checkout> install`, summarize its `✓`/`!` lines, then continue with the existing prompt-merge steps unchanged; keep the clone instructions as the fallback when no checkout exists
- [ ] mirror the step in `assets/codex/skills/loopai-update/SKILL.md` with a plain-text question instead of AskUserQuestion and Codex-safe wording
- [ ] keep the manifest version from task 3 (bump again only if task 3's version was already released)
- [ ] extend an existing skill-contract shell suite (or add assertions to `check-skill-contract-snippet_test.sh`) so both `loopai-update` copies mention `make -C` and `known_marketplaces.json`, guarding the step against accidental removal
- [ ] run `make test </dev/null` - must pass before task 7

### Task 7: Verify acceptance criteria
- [ ] verify all requirements from Overview are implemented
- [ ] verify a stub binary without `--skill-contract` yields `binary=0` through the snippet and that every skill contains the warning instruction
- [ ] verify editing a `##` heading in embedded `report.txt` fails `TestSkillContract` (then revert)
- [ ] run full test suite (`make test </dev/null`)
- [ ] run linter (`make lint`) - all issues must be fixed
- [ ] cross-compile `GOOS=windows GOARCH=amd64 go build ./...`

### Task 8: [Final] Update documentation
- [ ] `CLAUDE.md`: add `make install` to Build commands; add a paragraph on `skillContract`, `--skill-contract`, the fingerprint contents, when to bump, the `-update` command, and the byte-identical snippet in all 19 skills; add the PR checklist item "contract surface changed → `skillContract` bumped or the change deliberately accepted via `-update`"
- [ ] `README.md` `## Installation`: rewrite around `make install` and `/loopai:loopai-update`, mention `LOOPAI_BIN_DIR` and the standalone-copy warning
- [ ] `llms.txt`: one line on `--skill-contract`
- [ ] confirm `make test-report-docs` and other doc-contract suites still pass

## Technical Details

**Snippet** (byte-identical in all 19 skills; only `expected=` changes on a bump):

```bash
expected=1
if ! command -v loopai >/dev/null 2>&1; then
  actual=missing
else
  actual=$(loopai --skill-contract 2>/dev/null) || actual=0
  case $actual in ''|*[!0-9]*) actual=0 ;; esac
fi
echo "skill contract: skill=$expected binary=$actual"
```

Instruction paragraph after it (Claude wording; Codex replaces the command with `$loopai-update`):
- `binary=missing`: continue with this skill's existing handling of a missing CLI.
- `binary` lower than `skill`: tell the user "the installed loopai binary is older than these skills; run `/loopai:loopai-update`", then continue.
- `binary` higher than `skill`: tell the user "these skills are older than the installed loopai binary; run `/loopai:loopai-update`, then `/reload-plugins` or restart the session", then continue.
- equal: continue silently.

**Golden file** `cmd/loopai/testdata/skill-contract.golden`:

```text
contract: 1

flags:
--acp
--base-ref
...

config keys:
backlog_dir
...

report headings:
Summary
Change scope
Evidence
Risk
Merge danger
...
```

**install.sh steps** (exit non-zero only for 1, 2, and a contract mismatch in 5):
1. `make build` in the repo root; on failure stop before installing anything.
2. `install -d "$BIN_DIR"`; `install -m 0755 .bin/loopai .bin/loopai-acp "$BIN_DIR"/` with `BIN_DIR=${LOOPAI_BIN_DIR:-$HOME/.local/bin}`.
3. If `claude` is on PATH: `claude plugin marketplace update loopai`, then `claude plugin update <name>@loopai` for each plugin from that marketplace present in `claude plugin list`; if `loopai@loopai` is absent print `claude plugin install loopai@loopai` as a hint. Failures print `!`.
4. If `${CODEX_HOME:-$HOME/.codex}` exists run `scripts/install-codex-skills.sh`; otherwise print a skip line.
5. Verify `"$BIN_DIR"/loopai --skill-contract` equals `skillContract`; compare plugin versions; warn when `command -v loopai` is not `"$BIN_DIR"/loopai`.
6. If `$HOME/.claude/skills/loopai*` exists, warn that these standalone copies duplicate the plugin and print `rm -rf` with the exact paths; never delete.
7. Print: open Claude Code sessions need `/reload-plugins` or a restart; open Codex sessions need a restart.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- On this machine run `make install`, then `/reload-plugins`; confirm no skill prints a contract warning and `loopai --version` shows the new revision.
- Decide whether to delete the standalone `~/.claude/skills/loopai*` copies using the printed command.
- Optionally retire `~/.claude/commands/build_and_isntall.md` in favor of `make install`.
- Run `/loopai:loopai-update` once to refresh the commented prompt copies in `~/.config/loopai/prompts/`.
