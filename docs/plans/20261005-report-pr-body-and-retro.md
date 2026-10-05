# Report-backed PR bodies, Evidence and Merge danger sections, and a loopai-retro skill

## Overview

Three related improvements, inspired by the `/pr` and `/retro` skills in mattpocock/skills v1.3:

1. **PR bodies built from the completion report.** Today `buildPRTitleBody` (`cmd/loopai/main.go`)
   fills the body of every pull request loopai opens — `--pr` and `finalize = pr|merge` alike — with
   the plan's overview plus three diff numbers. The completion report (`.report.md`) with risk, plan
   deviation, reviewer findings, and validation never reaches the reviewer on GitHub. After this
   plan, a report is located (on the branch, in the working tree, or in memory under finalize) and a
   selection of its sections becomes the PR body, with the current body as the fallback whenever no
   report exists or the result would exceed GitHub's limit.
2. **Two new report sections.** `report.txt` and the facts-only fallback gain `## Evidence`
   (before/after proof: the test or output that failed and now passes) and `## Merge danger`
   (`Door: one-way | two-way` and `Blast radius: <one word>`), so the merge decision is made on
   reversibility, not only on a free-text risk level. `loopai-merge` narrates both before the
   confirmation gate.
3. **A `loopai-retro` skill.** A read-only, manually invoked retrospective over loopai's own durable
   artifacts (`.loopai/progress/`, `history/`, `.run.json`, `docs/plans/completed/*.report.md`,
   `docs/backlog/`) and the project's steering files, returning a ranked list of environment
   improvements. It never edits the repository except for backlog entries the user explicitly
   selects, because an auto-applying retro loops on its own false positives.

## Decisions

- **Context**: the PR body could carry the whole report, a selection of sections, or only the two
  new sections. The user chose a selection.
- **Chosen approach**: Summary, Evidence, Merge danger, Risk, Migrations and operational steps, and
  Plan deviation are placed in the body in that order; External review and Validation are wrapped in
  collapsed `<details>` blocks; the existing `## Changes` diff stats close the body. The report
  Summary replaces the plan overview. The PR title stays derived from the plan.
- **Rejected alternatives**: the full report verbatim (long reviewer blocks become unreadable and
  a 65 536-rune body needs truncation logic anyway); only Evidence and Merge danger appended to the
  plan overview (leaves risk and deviation in the repository only).
- **Report source**: `closeoutTarget` gains a `report` field. Under finalize the call site passes
  `Runner.Report()` in memory, because a single-plan `--worktree` run archives the sidecar through
  `MainGitSvc` in the main checkout, so it is never on the pushed branch. `--pr` (and finalize when
  the in-memory text is empty) locates the sidecar the same way `--report` does: `ShowFile` on
  `refs/heads/<branch>` for each `completionReportPaths` candidate, then the working tree. No
  report means the legacy body; a report is never required.
- **Size handling**: the body is built, then checked against `maxPRBodyRunes`. If it exceeds the
  limit, the two `<details>` blocks are dropped; if it still exceeds, the legacy body is used. The
  PR is never refused because the report is long.
- **Section placement**: `## Evidence` follows `## Change scope`, `## Merge danger` follows
  `## Risk`. The report contract becomes eleven sections. `extractReport` keeps validating nothing
  beyond the `# Report:` heading; the PR body builder tolerates a missing section by omitting it.
- **loopai-retro Codex counterpart**: written by hand like the other Codex skills (the user chose
  this over `exempt_skills`). The skill reads files through shell commands, so the port is small.
- **loopai-retro scope**: read-only analysis by default; the one write it may perform is filing
  user-selected candidates as `docs/backlog/` entries with the pathspec commit the other skills use.
  No prompt, agent, config, or `CLAUDE.md` edits, ever.
- **Verified facts**: `extractReport` (`pkg/processor/completion_report.go:16`) does no section
  validation; `factsOnlyReport` (line 39) hard-codes the sections and
  `pkg/processor/completion_report_test.go:185-191` pins the heading list and `8` `## ` headings;
  `check-symlinks.sh` does not restrict frontmatter keys, and no existing skill uses
  `disable-model-invocation`; `.run.json` is removed after successful archival
  (`removeRunRecordAfterArchival`, `cmd/loopai/main.go:1899`), so the retro's structured input is the
  report sidecar plus progress logs, with `.run.json` present only for unarchived runs; no script or
  test pins the topic list in either `loopai-merge` skill body.

## Context (from discovery)

- PR creation: `createPullRequest` (`cmd/loopai/main.go:5831`), `buildPRTitleBody` (6000),
  `findPRPlan` (6119), `parsePRPlan`, `readPRPlan` (6041, bounded no-symlink reader, `maxPRPlanSize`
  1 MiB), `validatePRMetadata` (6029, `maxPRBodyRunes` 65 536), `closeoutTarget` struct.
- Finalize close-out: `runFinalizeCloseout` (7005) → `openFinalizePR` (7027) builds
  `closeoutTarget{plansDir, statsBase}` at 7048. Call site at `executePlan` line 1902, where
  `r.Report()` is in scope; `archiveAfterFinalize` (1891) runs first.
- Report lookup for `--report`: `runReportCommand` (5267) with `findReportPlanForBranch` (5337),
  `completionReportPaths` (5364), the `ShowFile` loop (5296-5306) and the working-tree fallback
  (5308-5320).
- Report contract: `pkg/config/defaults/prompts/report.txt`, `factsOnlyReport`, docs at
  `README.md:549-553`, `llms.txt:165`, `CLAUDE.md:651` ("nine-section").
- Tests: `TestBuildPRTitleBody` (`main_test.go:6764`), `TestRunPRCommand` (7066, `gh` stub logs
  args to `$GH_ARGS_LOG` and stdin body to `$GH_BODY_LOG`), `TestRunPRCommandExplicitFeature`
  (12056), `TestRunReportCommand` (12188, committed-sidecar fixture around 12249-12288),
  `TestRunFinalizeCloseout` (14562) with `finalizeGHStub` (14415) and `setupFinalizePRFixture`
  (14490), `TestOpenFinalizePRStops` (14849).
- Skill inventory: `scripts/check-symlinks.sh:9` `expected_skills`;
  `scripts/check-symlinks_test.sh:30-38`; `scripts/check-codex-skills.sh:35` `exempt_skills`, forbidden
  Claude-only tokens at 41-52, required `agents/openai.yaml` keys at 120-130;
  `scripts/check-codex-skills_test.sh:47-55`; manifests `.claude-plugin/plugin.json:3` and
  `.claude-plugin/marketplace.json:12`, both `0.5.12`.
- Skill lists in docs: `CLAUDE.md:93-96`, `llms.txt:39-41` and `76-77`, `README.md:112-128`
  ("eight skills" while listing nine) and `README.md:233-234` (Codex, "Eight skills").
- Structural models: `assets/claude/skills/loopai-merge/SKILL.md` (frontmatter `name`,
  `description`, `argument-hint`, `allowed-tools`), `assets/codex/skills/loopai-merge/` with
  `agents/openai.yaml` (`display_name`, `short_description`, `default_prompt`).
- Retro inputs: progress log header (`Plan:`, `Worktree plan:`, `Branch:`, `Mode:`, model lines,
  `Started:`), section headers `--- <label> ---` without timestamp (`PrintSection`,
  `pkg/progress/progress.go:393`), timestamped body lines `[YY-MM-DD HH:MM:SS] <msg>`,
  `validation: <label> took <d>` and `validation: <total> (<n> runs)` lines
  (`pkg/progress/validation_timer.go`), `QUESTION:`/`OPTIONS:`/`DRAFT REVIEW:` lines, footer
  `Completed:`/`Failed:`; archives in `.loopai/progress/history/<stem>/archive-*.txt`;
  `processor.RunRecord` (`pkg/processor/run_record.go:39-58`); report sections including
  per-reviewer `###` blocks with `finding -> fixed | dismissed (reason) | filed to backlog`.

## Development Approach

- **Testing approach**: Regular (code first, then tests)
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
- Run tests after each change
- Maintain backward compatibility: a repository with no report, an old report without the new
  sections, and a customized `report.txt` must all keep producing a PR body

## Testing Strategy

- **Unit tests**: required for every task; Go tests use table-driven `testify` cases and
  `t.TempDir()` fixtures with `PATH`-injected `gh` stubs as the existing PR tests do
- **Shell regression suites**: skill inventory changes are covered by
  `scripts/check-symlinks_test.sh` and `scripts/check-codex-skills_test.sh`
- **E2E tests**: none; the dashboard is untouched

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, prompt, skill, test, and documentation changes
  in this repository
- **Post-Completion** (no checkboxes): a live `gh` smoke test and a retro run over real logs
- **Checkbox placement**: checkboxes belong only in Task sections

## Validation Commands

- `make test`
- `make lint`

## Implementation Steps

### Task 1: Add Evidence and Merge danger to the report contract

- [x] in `pkg/config/defaults/prompts/report.txt`, insert `## Evidence` after `## Change scope`:
      show before/after proof of the delivered change — the specific test, command output, or
      behavior that failed before and passes now, drawn from the supplied validation facts and the
      diff; write the literal `none` when the facts carry no such evidence; never invent a test run
- [x] in the same file, insert `## Merge danger` after `## Risk` with the fixed shape
      `**Door:** one-way | two-way` plus one line of reasoning (a change is two-way when a plain
      revert restores the previous state; destructive data changes, published APIs, released
      config formats, and anything consumers already depend on are one-way) and
      `**Blast radius:** <one word>` plus an optional line of ramifications; update the "exactly
      these sections" wording and the header comment
- [x] in `factsOnlyReport` (`pkg/processor/completion_report.go`), write `## Evidence` and
      `## Merge danger` as `_assessment unavailable_` at the same positions, keeping the order
      identical to the prompt
- [x] update `pkg/processor/completion_report_test.go`: add both headings to the pinned list, raise
      the `## ` count from 8 to 10, and assert the order of all eleven sections
- [x] add a test that `extractReport` returns a report containing the new sections unchanged
- [x] run `go test ./pkg/processor/...` - must pass before task 2

Validation adjustment for Task 1: the full suite exposed pre-existing portability
assumptions. The rollout directory-cache test now sets a distinct initial directory timestamp,
and the branch-conflict test accepts both supported Git error messages while checking the
conflicting worktree path. The Copilot wrapper timestamp pattern now works with awk versions
without interval expressions; its existing accepted-draft regression cases cover the fix.
The Pi wrapper EOF assertion now checks all JSON events, avoiding jq 1.6 exit-status
sensitivity to the trailing result event. Full Linux validation runs as a non-root user
for permission tests.

Validation passed: go test ./pkg/processor/... and make test in a non-root Linux Docker
checkout of HEAD plus the task changes; make lint on Windows reported zero issues.

### Task 2: Parse report sections and render the PR body

- [x] create `cmd/loopai/pr_body.go` with `splitReportSections(report string) []reportSection`
      (`heading`, `body`), splitting on `## ` lines only, treating `### ` lines as body, and
      ignoring everything before the first `## ` heading
- [x] add `reportPRBody(report string, stats git.DiffStats) (full, trimmed string, ok bool)`: returns `false` when
      the report has no `## Summary` section; otherwise emits Summary body first (no heading), then
      `## Evidence`, `## Merge danger`, `## Risk`, `## Migrations and operational steps`,
      `## Plan deviation` in that order, omitting absent sections; then `External review` and
      `Validation` each as `<details><summary>…</summary>` blocks with a blank line after the
      opening tag so Markdown renders inside; then the existing `## Changes` stats block
- [x] add `fitPRBody(full, trimmed, legacy string) string`: returns the first candidate within
      `maxPRBodyRunes`; `reportPRBody` exposes both the full body and the body without the
      `<details>` blocks so the caller can degrade in that order
- [x] create `cmd/loopai/pr_body_test.go` with table-driven tests: full eleven-section report,
      report predating the new sections, report missing `## Summary`, `### reviewer` subsections
      staying inside External review, an oversized External review degrading to the trimmed body,
      and an oversized Summary degrading to the legacy body
- [x] run `go test ./cmd/loopai/ -run 'PRBody|ReportSections'` - must pass before task 3

Implementation adjustment for Task 2: reportPRBody returns both full and trimmed bodies
plus its success flag. This reconciles the originally listed two-value signature with
the requirement to expose both candidates to fitPRBody and the Task 3 caller.

Validation passed: go test ./cmd/loopai/ -run 'PRBody|ReportSections', make test
in a non-root Linux Docker checkout of HEAD plus the Task 2 files, and make lint
on Windows (zero issues).

### Task 3: Locate the report and feed it into PR creation

- [x] extract the lookup from `runReportCommand` into
      `locateCompletionReport(gitSvc, plansDir, planFile, branch string) (body []byte, source string, err error)`
      that tries `ShowFile("refs/heads/"+branch, path)` for each `completionReportPaths` candidate,
      then the working tree through `readPRPlan`, and returns a sentinel not-found error; make
      `runReportCommand` call it so `--report` output is unchanged
- [x] add `report string` to `closeoutTarget`; in `buildPRTitleBody` (or a wrapper it calls),
      prefer `target.report`, else call `locateCompletionReport` with the plan `findPRPlan`
      resolved and the branch; on not-found keep the legacy body; on any other lookup error warn
      on stderr and keep the legacy body rather than failing the PR
- [x] cap a report read from `ShowFile` at `maxPRPlanSize` like the working-tree path
- [x] thread the report through finalize: add a `report string` parameter to `runFinalizeCloseout`,
      pass `r.Report()` at the `executePlan` call site, and set `closeoutTarget.report` in
      `openFinalizePR`
- [x] update `TestBuildPRTitleBody` for the report-backed body and add cases: committed sidecar on
      the branch only (not in the working tree), sidecar only in the working tree, no sidecar, and
      an in-memory report winning over an on-disk one
- [x] extend `TestRunPRCommand`/`TestRunPRCommandExplicitFeature` with a committed sidecar fixture
      and assert `$GH_BODY_LOG` contains `## Merge danger` and the `<details>` blocks; extend
      `TestRunFinalizeCloseout` with a report passed in memory and no sidecar on the branch,
      asserting the body
- [x] add a `TestRunReportCommand` case proving the extracted helper preserves the `(merged)`
      fallback
- [x] run `go test ./cmd/loopai/...` - must pass before task 4

Validation passed: go test ./cmd/loopai/... and full make test in a non-root
Linux Docker checkout of HEAD plus the Task 3 changes; make lint on Windows
reported zero issues. The report-aware wrapper retains the legacy title/body builder
and applies report selection and size fallbacks before PR metadata validation.

### Task 4: Narrate the new sections in loopai-merge

- [x] in `assets/claude/skills/loopai-merge/SKILL.md`, extend the fixed topic list to Summary,
      Change scope, Evidence, Risk, Merge danger, Migrations and operational steps, Plan deviation,
      Backlog, External review; state that an older report lacks Evidence and Merge danger and
      that they are then reported as absent, not inferred
- [x] in the confirmation gate, restate `Door` and `Blast radius` in one line above the
      `Merge into <base>?` question when present; add to the `Open PR` option that the pull request
      body is built from the report
- [x] mirror both changes in `assets/codex/skills/loopai-merge/SKILL.md` (prose, no
      `AskUserQuestion`)
- [x] run `make check-symlinks check-codex-skills test-symlinks test-codex-skills` - must pass
      before task 5

Added scripts/check-merge-skill_test.sh and the make test-merge-skill target,
also included in make test, to pin the narration order, older-report absence rule,
confirmation reminder, partial/missing danger handling, and both PR choices.

Validation passed: make check-symlinks check-codex-skills test-symlinks
test-codex-skills test-merge-skill and full make test in a non-root Linux Docker
checkout of HEAD plus the Task 4 changes; make lint on Windows reported zero issues.

### Task 5: Create the loopai-retro Claude skill

- [x] create `assets/claude/skills/loopai-retro/SKILL.md` with frontmatter `name: loopai-retro`,
      a description with triggers (`loopai-retro`, `retro`, `retrospective`, `ретро`),
      `argument-hint: '[plan stem | progress log path | --last N]'`,
      `allowed-tools: [Bash, Read, Glob, Grep, AskUserQuestion]`, and
      `disable-model-invocation: true`
- [x] write the input-selection section: with a plan stem, use its progress log, `history/`
      archives, `.run.json` if present, and `docs/plans/completed/<stem>.report.md`; with a log
      path, that log alone; with `--last N` or no argument, the N (default 5) newest top-level
      `progress-*.txt` logs by mtime; always add `docs/backlog/*.md` and the steering files
      `CLAUDE.md`, `AGENTS.md`, `.loopai/config`, `.loopai/prompts/`, `.loopai/agents/`, plus the
      repository's check commands (`Makefile`, CI workflows, pre-commit config)
- [x] write the bounded-reading rule: never read a progress log whole; first `grep -n` the
      structural lines (`--- … ---` section headers, `validation:`, `Completed:`, `Failed:`,
      `QUESTION:`, `DRAFT REVIEW:`, `TASK_FAILED`, `stalemate`, `limit`, `retry`, `warning:`),
      then read bounded windows around the hits; report sidecars and backlog entries may be read
      whole
- [x] write the candidate categories with their "use when" trigger: navigation pointers;
      automated checks (an unwired or absent guardrail is itself a finding); coding standards
      (a mechanical violation becomes a lint rule or check, a judgement call becomes a dynamic
      review agent in `.loopai/agents/` or a `CLAUDE.md` line); bloated steering files; no-op
      instructions; tool economy; information access; and the loopai-specific ones — a finding
      the same reviewer raises across runs, a review loop ended by its iteration cap or stalemate
      rather than clean, task iterations that failed and retried, validation commands whose
      measured time dominates the run or that ran many times, and human waits (`QUESTION:` lines)
      a config key or plan detail would have avoided
- [x] write the output section: candidates ranked by severity, each with the category, the
      evidence as `path:line` into the log or report, the proposed change, and where it belongs
      (lint rule, Makefile/CI, `.loopai/agents/<name>.txt`, `CLAUDE.md` pointer, `.loopai/config`
      key, plan template); then `AskUserQuestion` with `multiSelect` offering to file selected
      candidates as `docs/backlog/<kebab-slug>.md` entries in the format `loopai-plan` uses, each
      committed through `git add <entry>` and `git commit -m "docs: add backlog entry" -- <entry>`
- [x] write the constraints: read-only except the selected backlog entries; never edit prompts,
      agents, config, steering files, plans, or reports; never run loopai; never present a
      candidate without evidence; a round where nothing is selected writes nothing
- [x] add the symlink `assets/claude/loopai-retro.md -> ./skills/loopai-retro/SKILL.md`, add
      `loopai-retro` to `expected_skills` in `scripts/check-symlinks.sh`, and add
      `add_skill loopai-retro` to the valid fixture in `scripts/check-symlinks_test.sh`
- [x] add `scripts/check-retro-skill_test.sh` and the `make test-retro-skill` target,
      included in `make test`, covering input boundaries, manual invocation, evidence-backed
      candidates, selection-only writes, and executing the bounded log-reading examples
- [x] run `make check-symlinks test-symlinks` - must pass before task 6

Validation adjustment for Task 5: adding the Claude skill makes the existing Codex
inventory check require its counterpart, which is explicitly assigned to Task 6.
Run `make test` to confirm that this is its only failure, then
`make -o check-codex-skills -o test-codex-skills test` plus the standalone
`scripts/check-codex-skills_test.sh` for the remaining full suite. The installer
regression tests also run the inventory check and share this dependency. Do not exempt
`loopai-retro` or implement Task 6 here; Task 6 must pass the unchanged inventory
check, and Task 7 runs the unmodified full suite.

Validation passed: make check-symlinks test-symlinks test-retro-skill, the
standalone Codex inventory regression tests, and the remaining make test targets
(including Go race/coverage and wrapper suites) in a non-root Linux Docker
checkout of HEAD plus the Task 5 changes. make lint on Windows reported zero
issues. The full suite confirms only the scheduled Codex counterpart dependency;
its inventory and installer targets remain for Task 6.

### Task 6: Create the loopai-retro Codex skill and bump the manifests

- [x] create `assets/codex/skills/loopai-retro/SKILL.md` as a hand-written port: same inputs,
      bounded reading, categories, output, and constraints, with the selection asked in prose and
      no Claude-only tokens (`AskUserQuestion`, `allowed-tools`, `/loopai:`, `Task tool`); say the
      skill runs only on an explicit `$loopai-retro` request
- [x] create `assets/codex/skills/loopai-retro/agents/openai.yaml` with `display_name`,
      `short_description`, and `default_prompt`
- [x] add `add_pair loopai-retro` to the fixture in `scripts/check-codex-skills_test.sh`
- [x] bump `.claude-plugin/plugin.json` and the loopai entry in `.claude-plugin/marketplace.json`
      from `0.5.12` to `0.6.0`
- [x] extend `scripts/check-retro-skill_test.sh` to validate both ports, execute both bounded
      log-reading examples, and check Codex explicit invocation and selection-only writes
- [x] run `make check-codex-skills test-codex-skills check-plugin test-plugin test-wrappers` -
      must pass before task 7

Validation passed: Codex inventory and installer checks, plugin manifest checks,
the shared retro regression suite, and full make test (including Go race/coverage
and all wrapper suites) in a non-root Linux Docker checkout with init enabled.
make lint on Windows reported zero issues. Codex invocation policy also disables
implicit invocation, matching the skill's explicit-request rule.

### Task 7: Verify acceptance criteria

- [x] verify a `--pr` from a checkout whose branch carries the committed sidecar produces the
      report-backed body, and a repository with no report produces the legacy body
- [x] verify a finalize PR under `--worktree` carries the in-memory report
- [x] verify a report produced from an unmodified pre-change `report.txt` copy still yields a body
- [x] run `make test`
- [x] run `make lint` - all issues must be fixed
- [x] run `GOOS=windows GOARCH=amd64 go build ./...`

Acceptance coverage added in cmd/loopai/pr_acceptance_test.go: committed-sidecar
and no-report PR creation, plus full single-plan worktree execution with report generation,
archival in the source checkout, and an in-memory PR body while the pushed branch has no
sidecar. The compatibility case uses testdata/report-pre-evidence.txt, a byte-for-byte
copy of the report prompt at 36e57f8^, and a deterministic executor response in the old
nine-section format; it verifies the actual rendered prompt and resulting PR body.

Validation passed: focused acceptance and existing finalize tests, full make test
(including Go race/coverage and all wrapper suites) in a non-root Linux Docker
checkout with init enabled, make lint on Windows (zero issues), and
GOOS=windows GOARCH=amd64 go build ./....

### Task 8: [Final] Update documentation

- [ ] `README.md`: list `loopai:loopai-retro` in the plugin skills and fix the "eight skills"
      count, add `loopai-retro` to the Codex install list and its count, describe the report-backed
      PR body under `--pr` and finalize, and replace the nine-section report description with the
      eleven sections
- [ ] `llms.txt`: add `loopai:loopai-retro` and `$loopai-retro` to the skill lists and update the
      report section summary and the PR body note
- [ ] `CLAUDE.md`: add `loopai-retro` to the current skill set, describe the skill in one short
      paragraph (read-only, manual invocation, backlog entries are its only write), replace
      "nine-section" with "eleven-section", and document `closeoutTarget.report`, the in-memory
      report under finalize, and the `<details>` → legacy degradation order
- [ ] `docs/t3-code.md` or `docs/notifications.md` only if they mention the PR body (grep first;
      otherwise no change)

## Technical Details

- **Report sections (final order)**: `# Report:` title line, metadata line, `Summary`,
  `Change scope`, `Evidence`, `Risk`, `Merge danger`, `Migrations and operational steps`,
  `Plan deviation`, `Backlog`, `External review`, `Validation`.
- **Merge danger shape** (model-authored, parsed by nobody — the skill reads it as text):

  ```markdown
  ## Merge danger
  **Door:** two-way
  A revert of the branch restores the previous behavior; no data or published interface changes.

  **Blast radius:** small
  Only the PR body builder and the report prompt change.
  ```

- **PR body layout** when a report exists:

  ```markdown
  <Summary body>

  ## Evidence
  …
  ## Merge danger
  …
  ## Risk
  …
  ## Migrations and operational steps
  …
  ## Plan deviation
  …
  <details><summary>External review</summary>

  …
  </details>

  <details><summary>Validation</summary>

  …
  </details>

  ## Changes

  - Files changed: N
  - Additions: N
  - Deletions: N
  ```

- **Lookup order for `--pr`**: `target.report` (set only by finalize) → `refs/heads/<branch>` sidecar
  via `ShowFile` → working-tree sidecar via `readPRPlan` → no report. The not-found sentinel keeps
  `createPullRequest` on the legacy path; other errors are warnings, never PR failures.
- **Data flow under finalize**: `executePlan` → `runFinalizeCloseout(ctx, req, synced, report, log)`
  → `openFinalizePR` → `closeoutTarget{plansDir, statsBase, report}` → `createPullRequest`.
- **Retro ranking**: severity is major when the evidence shows a failed run, a stalemate, an
  iteration cap, or a finding repeated across runs; minor otherwise. Evidence is mandatory: a
  candidate without a `path:line` is dropped.

## Post-Completion

**Manual verification**:

- Open a real pull request with `loopai --pr` on a branch that has a completion report and check
  the rendering of the `<details>` blocks on GitHub.
- Run `/loopai:loopai-retro --last 3` on this repository and judge whether the ranked candidates are
  actionable; tune the category wording from that output.

**External system updates**:

- Users with a customized `report.txt` in `.loopai/prompts/` or `~/.config/loopai/prompts/` need
  `loopai-update` to receive the two new sections; until then their PR bodies simply omit them.
- Reinstall the plugin (`claude plugin update loopai`) and rerun `make install-codex-skills` to pick
  up `loopai-retro`.
