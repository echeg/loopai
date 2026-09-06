# Plan completion report and loopai-merge skill

## Overview

After a loopai run finishes, the user merges the feature branch into the branch it was cut from and
asks the same questions every time: what changed, how wide and how risky the change is, whether a
database, config, or data migration is needed, how far the work drifted from the plan, what was filed
to the backlog while working, and what each external-review model found. Today the answers are spread
across the progress log (reviewer findings as free text), `docs/backlog/` files, `➕`/`⚠️` lines inside
the plan, and the diff itself. Nothing summarizes them, and `--merge` prints one line.

This plan adds a completion report, `docs/plans/completed/<stem>.report.md`, written in the same commit
that archives the plan, and a `loopai-merge` skill that takes a plan name, narrates that report in the
conversation language, asks for confirmation, and only then runs `loopai --merge <plan>`. Facts are
collected deterministically by Go in a persisted run record; the assessments (risk, migrations, plan
deviation, per-finding outcomes) are written by the model in a new best-effort `report` phase that runs
after finalize. A new `loopai --report <plan>` close-out command prints the report from the feature
branch before the merge, so the skill and a human use one mechanism.

It integrates with the existing phase engine (`pkg/processor/phase`, modelled on finalize), the
existing plan archival path (`MovePlanToCompleted`, `MainGitSvc` in worktree mode), the existing
close-out routing (`--merge`/`--pr`), and the review checkpoint store introduced by
`docs/plans/20260906-review-resume.md`.

## Decisions

- **Context**: the user merges after every run and re-derives the same facts by hand. A durable,
  per-plan report next to the archived plan answers them once; a skill turns it into a merge gate.
- **Chosen approach (approved in brainstorm)**: hybrid authoring. Go collects deterministic facts in a
  `RunRecord` (persisted to `.loopai/progress/<progress-log-stem>.run.json`); a new
  `phase.ReportPhase`, modelled on `FinalizePhase` (review executor, best-effort, `status.PhaseReport`,
  section `report step`), runs the embedded prompt `report.txt` with a new `{{RUN_FACTS}}` placeholder.
  The model reads the plan, the diff, and the facts and **returns** the markdown report in its response;
  it must not write repository files. `cmd/loopai` obtains it via `Runner.Report()` and writes
  `docs/plans/completed/<stem>.report.md` with the same git service that moves the plan (`MainGitSvc`
  in worktree mode), in the same commit, message `move completed plan: <name> (+ report)`. Phase
  order: post-review → finalize → report → plan archival. Config key `report_enabled`, default `true`.
  A model failure yields a facts-only report marked `assessment unavailable`; a report write error is a
  warning and never blocks archival.
- **Rejected: report as checkboxes or a section inside the plan file**. `task.txt` treats any `[ ]`
  anywhere in the file as pending work and withholds `ALL_TASKS_DONE`; an appended section risks that,
  and it bloats the plan the model keeps editing.
- **Rejected: the model writes the report file itself**. In `--worktree` mode the plan is archived in
  the main checkout by `MainGitSvc` while the model works inside the worktree, so the file would land in
  the wrong checkout and miss the archival commit.
- **Rejected: everything in `cmd/loopai` after the runner returns**. Reviewer findings exist only inside
  `pkg/processor`, and a phase outside the processor loses retry/limit policy, sections, and cmux/Orca
  titles.
- **Rejected: reconstructing findings from the progress log**. Human format, changes freely, no
  machine structure.
- **Rejected: a separate `docs/reports/` directory**. Needs a new key and weakens the link to the plan.
- **Rejected: a Go-only deterministic report**. Cannot assess risk, migrations, or plan deviation.
- **Persistence (approved)**: the run record is written to disk after every recorded event, next to the
  progress log and the review checkpoint, with the same anchor (`filepath.Dir(log.Path())`) and the
  same atomic write pattern (temp file, `0600`, rename). It is loaded at startup when the branch matches,
  reset whenever the review checkpoint is cleared (task phase committed new work, branch no longer
  contains the recorded commit), and removed after successful plan archival.
- **Ordering constraint**: this plan builds on `docs/plans/completed/20260906-review-resume.md`,
  merged into `master` on 2026-09-06 (merge commit `c9eb994`). It reuses that plan's
  `phase.ReviewerCompletion` and `OnReviewerDone` hook (`pkg/processor/phase/external_review.go:33,70`),
  `Runner.SetReviewCheckpoints` and `ReviewCheckpointStore` (`pkg/processor/review_checkpoint.go:48`),
  `newReviewCheckpointStore(progressLogPath)` (`cmd/loopai/review_checkpoint_state.go:17`), and
  `Runner.clearReviewCheckpoint(reason)` (`pkg/processor/review_resume.go:304`). That clear has two
  callers: invalidation clears with a non-empty reason during the task phase, and the success clear
  `clearReviewCheckpoint("")` right after `finalize.Run` on all three paths of
  `runExternalAndPostReview` (`pkg/processor/runner.go:429,447,475`). The run record is reset only on
  invalidation clears; the success clear leaves it intact because the report phase and archival still
  need it, and the record is removed after archival instead.
- **Report language**: English, like every other repository document. The skill narrates in the
  conversation language.
- **Verified facts**: `runExternalAndPostReview` order at `pkg/processor/runner.go:423-476` and the
  finalize phase construction at `runner.go:253`; `GitChecker` already carries `HeadHash`,
  `DiffFingerprint`, `IsDirtyAll`, `ContainsRevisionContext`, and `CurrentBranch`
  (`runner.go:106-112`); `FinalizePhaseOpts{Cfg, Log, Exec, Policy, Prompts, PhaseHolder}` at
  `pkg/processor/phase/finalize.go:22`; `finalize_enabled` parsed in `pkg/config/values.go:348` with a
  `FinalizeEnabledSet` tracker and loaded from `finalize.txt` in `pkg/config/prompts.go:75`;
  `ExternalReviewPhase.runIteration` has `reviewResult.Output` and `evalResult.Output` per iteration
  (`external_review.go:229-263`); `moveCompletedPlan` (`cmd/loopai/main.go:1523`) uses `MainGitSvc` and
  `MainPlanFile` in worktree mode and skips chains; `git.Service.MovePlanToCompleted`
  (`pkg/git/service.go:1951`) does `git mv` plus commit and resolves alternate basenames through
  `resolvePlanMoveTargets` (`service.go:2057`); `shouldMovePlan` is true only for `ModeFull` and
  `ModeTasksOnly`, so `--review`/`--external-only` never produce a report; `git.Service` has
  `DiffStats`, `BranchDiffStats` (`service.go:2137,2145`), `HeadHash`, `CurrentBranch`,
  `ContainsRevisionContext`, and no commit-log helper; `progress.SectionTimer.FinishRun` prints phase
  durations and `progress.ValidationTimer` aggregates validation timings (`validation_timer.go:10-27`);
  close-out flags are `Merge`/`PR` with `optional-value:""` (`main.go:85-86`), validated by
  `validateCloseoutFlags` (`main.go:2848`), routed by `isStandaloneCommand` (`main.go:3998`) and
  `runCloseoutCommand` (`main.go:4677`), with `resolveFeatureBranch` (`main.go:4046`) resolving a plan to
  its branch through progress records; `status.PhaseFinalize` is mapped in `pkg/cmux/cmux.go:851,875`
  and `pkg/orca/orca.go:379`; `plan.Selector` globs `docs/plans/*.md` non-recursively, so
  `completed/*.report.md` is never offered as a plan; backlog entries are files in `backlog_dir` with a
  `# title` heading and a `- found: <date>, plan: <name>, phase: ...` line; the skill inventory is seven
  skills at manifest version `0.5.1`.

## Context (from discovery)

- Files/components involved:
  - `pkg/processor/runner.go`, `pkg/processor/phase/{finalize,external_review,review,task}.go`,
    `pkg/processor/phase/phase.go` (`Deps`), `pkg/processor/prompts.go` (`replaceBaseVariables`).
  - `pkg/config/{config,values,prompts}.go`, `pkg/config/defaults/config`,
    `pkg/config/defaults/prompts/` (new `report.txt`).
  - `pkg/status/status.go` (new `PhaseReport`), `pkg/cmux/cmux.go`, `pkg/orca/orca.go`,
    `pkg/web/session_progress.go` (phase mappings).
  - `pkg/git/service.go` (commit list, name-status, show-file helpers, sidecar archival).
  - `pkg/plan` (read-only drift extraction).
  - `cmd/loopai/main.go` (`moveCompletedPlan`, close-out routing, `--report`), new
    `cmd/loopai/run_record_state.go`.
  - `assets/claude/skills/loopai-merge/SKILL.md`, `assets/claude/loopai-merge.md`,
    `scripts/check-symlinks.sh`, `scripts/check-symlinks_test.sh`, `.claude-plugin/*.json`.
- Related patterns found: `FinalizePhase` as the best-effort single-session phase template;
  `savePlanChainCheckpoint` and the review checkpoint store for atomic JSON in `.loopai/progress/`;
  `stalemateState` consuming `HeadHash`/`DiffFingerprint`; runner tests asserting `[]status.Phase`
  sequences (`TestRunner_CodexAndPostReview_PipelineOrder`); close-out tests using local bare remotes
  and `PATH`-injected stubs.
- Dependencies identified: none new.

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
- Run focused tests while iterating; `make test` and `make lint` at the end of every task
- Maintain backward compatibility: with `report_enabled = false` a run behaves exactly as today, and a
  run without a recorder or store produces the same phase sequence and log lines as before
- Tests must use `t.TempDir()` and never touch `~/.config/loopai/` or `~/.config/ralphex/`
- Generated mocks come from `go generate` (`moq`); never hand-edit them

## Testing Strategy

- **Unit tests**: required for every task (see Development Approach above)
- **Shell suites**: `scripts/check-symlinks_test.sh` must cover the new skill; `make check-plugin`
  must accept the bumped manifests
- **E2E tests**: not applicable; the dashboard is untouched beyond a phase label mapping. Cross-compile
  with `GOOS=windows GOARCH=amd64 go build ./...` in the verification task because the store and the
  sidecar write files

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, prompt, skill, and documentation changes
  in this repository
- **Post-Completion** (no checkboxes): manual smoke test on a toy repository, plugin reinstall
- **Checkbox placement**: checkboxes belong only in Task sections

## Implementation Steps

### Task 1: RunRecord model, recorder interface, and store contract
- [x] create `pkg/processor/run_record.go` with `RunRecord{Version, Plan, Branch, BaseRef, Mode, Executor, TaskModel, ReviewModel, StartedAt, FinishedAt, PhaseDurations map[string]Duration, Tasks{Iterations, FailedRetries}, InternalReview{FirstRan, LoopIterations, EndedBy}, External []ExternalReviewerRecord, PostReview{Ran, Iterations}, Report string}` and `ExternalReviewerRecord{Key, Label, Iterations []ExternalIterationRecord, Duration, EndedBy, HadFindings}` with `ExternalIterationRecord{Index, ReviewerOutput, EvaluatorResponse, Truncated bool}`; `const runRecordVersion = 1`, `const runRecordTextCap = 16 * 1024`
- [x] add `truncateForRecord(s string) (string, bool)` that cuts at the cap on a rune boundary and appends `\n[truncated]`
- [x] define `RunRecordStore` interface at the consumer in `pkg/processor`: `Load() (RunRecord, bool, error)`, `Save(RunRecord) error`, `Remove() error`; add the `//go:generate moq` line and generate `run_record_store_mock_test.go` in the external `processor_test` package (`mocks/run_record_store.go` would create an import cycle with existing internal tests)
- [x] define `phase.RunRecorder` in `pkg/processor/phase/phase.go` with `TaskIteration(failed bool)`, `InternalReviewDone(loopIterations int, endedBy string)`, `ExternalIteration(index int, key, label, reviewerOutput, evaluatorResponse string)`, `ExternalDone(done ReviewerCompletion)`, `PostReviewDone(iterations int)`; add `Recorder RunRecorder` to `phase.Deps` (nil = no recording)
- [x] write tests: JSON round-trip of `RunRecord`, `truncateForRecord` below/at/above the cap and on multi-byte text, zero-value record marshals without panics
- [x] run `go test ./pkg/processor/...` - must pass before task 2

### Task 2: Record events from the phases and persist after each one
- [x] `pkg/processor/phase/task.go`: call `Recorder.TaskIteration(result.Signal == SignalFailed)` once per executor iteration when `Deps.Recorder != nil`
- [x] `pkg/processor/phase/review.go`: call `Recorder.InternalReviewDone(i, endedBy)` when `Loop` returns nil, with `endedBy` one of `review_done`, `no_changes`, `max_iterations`; the post-review `Loop(ctx, prefix)` with a non-empty prefix reports through `PostReviewDone(i)` instead, so distinguish the two by the prefix argument
- [x] `pkg/processor/phase/external_review.go`: after each `runIteration`, call `Recorder.ExternalIteration(index, key, label, reviewResult.Output, evalResult.Output)`; on reviewer completion call `Recorder.ExternalDone(done)` from the same place the `OnReviewerDone` hook fires; the reviewer key is `Tool + ":" + model spec` from the phase's reviewer list
- [x] runner: implement `RunRecorder` on a `runRecorder` type owned by `Runner` that mutates `r.record` and, when a `RunRecordStore` is set through `SetRunRecordStore`, saves after every event; a save error is logged once as `run record: save failed: %v` and never fails the run
- [x] runner startup: in `Run`, load the record when a store is set; keep it only when `Branch` equals the current branch (via the runner's `GitChecker.CurrentBranch`) and the review checkpoint was honored; otherwise start a fresh record; whenever `clearReviewCheckpoint(reason)` runs with a non-empty reason (invalidation during the task phase), also reset the record and `Remove` it; the success clear `clearReviewCheckpoint("")` after finalize must leave the record untouched
- [x] fill `Run`-level fields from `Config` at startup (`Plan`, `BaseRef`, `Mode`, executor and model specs) and `FinishedAt` when `Run` returns
- [x] create `cmd/loopai/run_record_state.go` with `runRecordStore{path}` where path is `<progress dir>/<progress stem>.run.json`, atomic write (temp + `Chmod(0o600)` + rename), `Load` returning `found=false` on not-exist and a wrapped error on corrupt JSON, `Remove` idempotent; wire `r.SetRunRecordStore(newRunRecordStore(runnerLog.Path()))` next to the review checkpoint store in `executePlan`
- [x] write tests: each phase calls the recorder with the right arguments (mock executors), no calls when `Recorder` is nil, runner saves after each event and logs a save failure once, startup keeps a matching record and discards a mismatched branch, checkpoint clear resets and removes the record, file store round-trip / corrupt / missing / mode `0600` in `t.TempDir()`
- [x] run `go test -race ./pkg/processor/... ./cmd/...` - must pass before task 3

### Task 3: Git facts and plan drift collection
- [x] `pkg/git/service.go`: add `CommitsBetween(base, head string) ([]Commit, error)` returning `Commit{Hash, Subject}` from `git log --format=%H%x00%s base..head`, and `DiffNameStatus(base string) ([]FileChange, error)` returning `FileChange{Status, Path}` from `git diff --name-status base...HEAD`; both honor `vcs_command` through the existing backend
- [x] add `processor.RunFactsSource` interface at the consumer with `CommitsBetween`, `DiffNameStatus`, `DiffStats`; `git.Service` satisfies it; runner gets `SetRunFactsSource`
- [x] create `pkg/plan/drift.go` with `ExtractDrift(content string) Drift{Added, Blocked, Skipped []string}` that collects lines starting with `➕`, lines starting with `⚠️`, and checkboxes whose text contains `(skipped` (case-insensitive); fence-aware like `FileHasUncompletedCheckbox`
- [x] add `collectRunFacts(ctx) RunFacts` to the runner: commits and name-status against `cfg.DefaultBranch`, diff stats, backlog files as name-status entries with status `A` under `AppConfig.BacklogDir` (fallback `docs/backlog`) with the `# title` read from the file when present, validation commands from the parsed plan, and `plan.ExtractDrift` of the current plan file; any single failure leaves that field empty and logs `run facts: <field>: %v`
- [x] write tests: `CommitsBetween` and `DiffNameStatus` on a temp repository with two commits and added/modified/deleted files; `ExtractDrift` table cases including fenced code and no drift; `collectRunFacts` with a mock source covering partial failures
- [x] run `go test ./pkg/git/... ./pkg/plan/... ./pkg/processor/...` - must pass before task 4

### Task 4: Report phase, prompt, config key, and facts rendering
- [x] `pkg/status/status.go`: add `PhaseReport Phase = "report"`; map it in `pkg/cmux/cmux.go` (same group as finalize, text `report`, icon `doc.text`), `pkg/orca/orca.go` (working title like finalize), and `pkg/web/session_progress.go` (`phaseFromSection` recognizes `report step`)
- [x] `pkg/config`: add `ReportEnabled bool` with `ReportEnabledSet` mirroring `FinalizeEnabled` in `config.go`, `values.go`, and the embedded `pkg/config/defaults/config` with a commented `report_enabled = true` block under a new `# completion report` heading; add `reportPromptFile = "report.txt"` and load `prompts.Report` in `pkg/config/prompts.go` with the local/global/embedded fallback
- [x] create `pkg/config/defaults/prompts/report.txt`: header lists `{{PLAN_FILE}}`, `{{DEFAULT_BRANCH}}`, `{{BACKLOG_DIR}}`, `{{RUN_FACTS}}`; body instructs the model to read the plan and `git diff {{DEFAULT_BRANCH}}...HEAD`, use the facts as ground truth, write NOTHING to the repository, and return only a markdown document with exactly these sections in order: `# Report: <plan title>` plus a metadata line; `## Summary`; `## Change scope`; `## Risk` (`low`/`medium`/`high` with reasons: public APIs, data schemas, configs, concurrency, migrations); `## Migrations and operational steps` (DB, config, data, deploy, or the literal `none`); `## Plan deviation`; `## Backlog`; `## External review` (one `### <reviewer key>` per reviewer with the Go numbers copied verbatim, then a list `finding -> fixed | dismissed (reason) | filed to backlog`); `## Validation`; no completion signal
- [x] create `pkg/processor/run_facts.go` with `renderRunFacts(record RunRecord, facts RunFacts) string` producing compact markdown: metadata, phase durations, a files table (`status | path`), diff totals, commit list, backlog files with titles, drift lines, validation commands and timings, and per reviewer the numbers plus each iteration's reviewer output and evaluator response in fenced blocks; expand `{{RUN_FACTS}}` in `pkg/processor/prompts.go` through a new `ReportPrompt(facts string)` builder that goes through `replaceBaseVariables`
- [x] create `pkg/processor/phase/report.go` with `ReportPhase` modelled on `FinalizePhase`: `ReportPhaseOpts{Cfg, Log, Exec, Policy, Prompts, PhaseHolder}`, `Run(ctx, facts string) (string, error)` that sets `PhaseReport`, prints `status.NewGenericSection("report step")`, runs the executor, returns the response text; timeout, `SignalFailed`, and executor errors other than context cancellation are logged and return `"", nil` so the caller falls back; `Cfg.ReportEnabled` false is a no-op
- [x] add `extractReport(output string) (string, bool)`: takes the text from the first line starting with `# Report:` to the end, strips trailing signal markers, and reports false when the heading is missing; add `factsOnlyReport(record, facts) string` that renders the nine headings with the Go facts and `_assessment unavailable_` under the model-owned sections
- [x] write tests: config parsing of `report_enabled` (set/unset/invalid), prompt loading fallback, `renderRunFacts` golden-style table cases (empty record, two reviewers with truncation, no backlog), `ReportPhase` success / FAILED / timeout / disabled with mock executor and phase-holder transitions, `extractReport` with preamble text, missing heading, trailing signal, `factsOnlyReport` contains all nine headings
- [x] run `go test ./pkg/...` - must pass before task 5

### Task 5: Runner order and Report() result
- [x] `pkg/processor/runner.go`: construct `ReportPhase` in `NewWithExecutors` with the review executor (falling back to task) and `prompts`; add `report reportPhaseRunner` to `runnerPhases` with interface `Run(ctx, facts string) (string, error)`
- [x] in `runExternalAndPostReview`, on all three paths (external disabled, no findings, findings), call a new `r.runReport(ctx)` between `finalize.Run` and the success `clearReviewCheckpoint("")` (`pkg/processor/runner.go:426-429`, `444-447`, `472-475`), guarded by `cfg.ReportEnabled` only (`ModeTasksOnly` never reaches this function); `runReport` collects facts, renders them, runs the phase, and stores `extractReport(output)` or `factsOnlyReport(...)` into `r.record.Report`, then saves the record
- [x] add `func (r *Runner) Report() string` returning `r.record.Report` (empty when disabled or never run)
- [x] `runTasksOnly`: no report (no reviews to describe); document this in the config comment
- [x] update `TestRunner_CodexAndPostReview_PipelineOrder` and the full-mode tests for the new trailing `PhaseReport` when enabled; add tests: report disabled keeps the old sequence byte-for-byte, model failure yields a facts-only report, `--review` and `--external-only` modes still produce `Report()` text (archival is what skips the sidecar there, via `shouldMovePlan`), findings and no-findings paths both run report after finalize
- [x] run `go test -race ./pkg/processor/...` - must pass before task 6

### Task 6: Write the report sidecar in the archival commit
- [x] `pkg/git/service.go`: add `MovePlanToCompletedWithReport(planFile string, report []byte) error` that reuses `resolvePlanMoveTargets`, writes `<completedDir>/<stem>.report.md` (stem = dest basename without `.md`) with `0o644` via `os.WriteFile`, stages it, and commits with `move completed plan: <base> (+ report)`; empty `report` delegates to `MovePlanToCompleted` unchanged; when `resolvePlanMoveTargets` reports `done` (already archived), write and commit the report alone as `add completion report: <base>` only when the sidecar does not exist yet
- [x] `cmd/loopai/main.go`: `moveCompletedPlan(req, report string)` passes the runner's `Report()` through; a sidecar write failure after a successful plan move is printed as `warning: failed to write completion report: %v` and does not fail the run; after successful archival remove the run record file (`store.Remove()`), logging a failure as a warning
- [x] ensure chain runs (`len(ChainPlanFiles) > 1`) pass the report through the same call so each member archives its own sidecar
- [x] write tests: sidecar and plan land in one commit with the expected message, worktree mode archives through `MainGitSvc` in the main checkout, empty report keeps the legacy commit message, already-archived plan gets a report-only commit once and not twice, write failure surfaces as a warning while the plan move succeeds, run record removed after archival
- [x] run `go test ./pkg/git/... ./cmd/...` - must pass before task 7

### Task 7: `loopai --report <plan>` close-out command
- [x] add `Report bool \`long:"report" description:"print the completion report for a plan or branch; positional argument names the feature (branch or plan), default current branch"\`` to `opts`; extend `mergeRequested`/`prRequested`-style predicates with `reportRequested`, `validateCloseoutFlags` (mutually exclusive with `--merge`, `--pr`, `--clear`, execution modes; rejects surplus positionals), `isStandaloneCommand`, `hasExecutionMode` exclusions, and the early-routing branch that sends `--merge`/`--pr` to `runCloseoutCommand`
- [x] `pkg/git/service.go`: add `ShowFile(ref, path string) ([]byte, error)` wrapping `git show <ref>:<path>` and `git.ErrPathNotFound` for a missing path
- [x] implement `runReportCommand(ctx, gitSvc, target, stdout)`: resolve the feature with `resolveFeatureBranch` (progress-record association first, derivation second); locate the plan's completed stem (`<plansDir>/completed/<stem>.report.md`, honoring `plan.AltDateBasename` variants); try `ShowFile(branch, path)`, then the working tree file; print `branch: <name>` and the report body; when the branch is gone but the file exists on disk print `branch: (merged)`; when neither exists return `no completion report for <plan>; the run predates report_enabled or archived without one`
- [x] write tests with a temp repository: report on the feature branch found by progress record with a `--branch` override, fallback to disk after merge, missing report error, `--report` rejected with `--merge` and with a surplus positional, `--report` routes before executor/notification setup (no `claude` binary needed)
- [x] run `go test ./cmd/...` - must pass before task 8

### Task 8: `loopai-merge` skill and plugin inventory
- [x] create `assets/claude/skills/loopai-merge/SKILL.md` with frontmatter (`name`, `description` with triggers `loopai-merge`, `merge plan`, `смержи план`, `argument-hint: '<plan name or path>'`, `allowed-tools: [Bash, Read, Glob, AskUserQuestion]`) and steps: preflight (`which loopai`, repository root, clean tree via `git status --porcelain`), normalize the argument (`20260906-foo`, `foo`, `docs/plans/foo.md`, `docs/plans/completed/foo.md` all resolve to the plan stem), run `loopai --report <plan>`, narrate in the conversation language by fixed sections (summary, scope, risk, migrations, deviation, backlog, per-reviewer findings) without inventing facts absent from the report, then `AskUserQuestion` "Merge into <base>?" with options merge / open PR via `loopai --pr <plan>` / cancel; on merge run `loopai --merge <plan>` and show its output verbatim; on a conflict report it and stop; when `--report` finds no report, say so, show `git log <base>..<branch> --stat`, and still offer the merge; the skill never edits code, never runs `git merge` by hand, never deletes branches
- [x] add the symlink `assets/claude/loopai-merge.md -> skills/loopai-merge/SKILL.md`
- [x] add `loopai-merge` to `expected_skills` in `scripts/check-symlinks.sh` and to the valid fixture inventory in `scripts/check-symlinks_test.sh`
- [x] bump `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json` to the same new version
- [x] mention the skill in the `loopai-orca` close-out section and in `loopai` skill's completion text where `--merge` is suggested
- [x] run `make check-symlinks`, `make test-symlinks`, `make check-plugin`, `make test-plugin` - must pass before task 9

### Task 9: Verify acceptance criteria
- [x] verify a full-mode run with `report_enabled = true` archives plan and `<stem>.report.md` in one commit and the report contains all nine sections with a `### <reviewer key>` per reviewer
- [x] verify a model failure in the report phase still archives a facts-only report
- [x] verify `report_enabled = false` produces the pre-change phase sequence, log lines, and commit message
- [x] verify `loopai --report <plan>` prints the report from the feature branch before merge and from disk after
- [x] verify the run record is removed after archival and reset when the review checkpoint is cleared
- [x] run `make test`
- [x] run `make lint` - all issues fixed
- [x] run `GOOS=windows GOARCH=amd64 go build ./...`
- [x] verify coverage for the new files in `pkg/processor`, `pkg/git`, `pkg/plan`, and `cmd/loopai` meets the project standard (80%+)

### Task 10: [Final] Update documentation
- [ ] `README.md`: document the completion report (location, sections, `report_enabled`, facts-only fallback, worktree archival path), the `--report` command in the close-out section, and the `loopai-merge` skill in the skills list
- [ ] `llms.txt`: one paragraph on the report, `--report`, and the run record file
- [ ] `CLAUDE.md`: architecture notes on `RunRecord`/`RunRecorder`, the `report` phase position after finalize, why the model returns the report instead of writing it, the sidecar archival commit, the `.run.json` clear-together rule with the review checkpoint, and the eight-skill inventory
- [ ] `pkg/config/defaults/config`: comments for `report_enabled` match behavior, including that tasks-only and review-only modes produce no sidecar
- [ ] `docs/custom-providers.md`: note that wrappers must return the report text as ordinary assistant output for the report phase to capture it

## Technical Details

### Run record file

`<main checkout>/.loopai/progress/<progress stem>.run.json`, `0600`, rewritten atomically after each
event. Cleared together with the review checkpoint and removed after archival.

```json
{
  "version": 1,
  "plan": "docs/plans/20260906-foo.md",
  "branch": "foo",
  "base_ref": "master",
  "mode": "full",
  "executor": "codex",
  "task_model": "gpt-5.6-sol:medium",
  "review_model": "gpt-5.6-sol:high",
  "started_at": "2026-09-06T01:41:47Z",
  "tasks": {"iterations": 6, "failed_retries": 0},
  "internal_review": {"first_ran": true, "loop_iterations": 2, "ended_by": "review_done"},
  "external": [
    {"key": "claude:opus:high", "label": "claude (opus:high)", "ended_by": "done", "had_findings": true,
     "duration_ms": 1830000,
     "iterations": [{"index": 1, "reviewer_output": "...", "evaluator_response": "...", "truncated": false}]}
  ],
  "post_review": {"ran": true, "iterations": 1},
  "report": ""
}
```

### Report phase flow

```text
post-review → finalize → report:
  facts  = collectRunFacts(git: commits, name-status, stats, backlog adds, drift, validation)
  prompt = ReportPrompt(renderRunFacts(record, facts))      # {{RUN_FACTS}}
  out    = ReportPhase.Run(ctx, prompt)                       # best-effort
  record.Report = extractReport(out) || factsOnlyReport(record, facts)
archival (cmd/loopai):
  MovePlanToCompletedWithReport(plan, record.Report)          # one commit
  runRecordStore.Remove()
```

### Report sections (fixed order)

1. `# Report: <plan title>` and a metadata line: plan, branch, base, mode, models, duration
2. `## Summary`
3. `## Change scope`: Go files table and commit list, model adds breadth by package/subsystem
4. `## Risk`: `low` / `medium` / `high` with reasons
5. `## Migrations and operational steps`: DB, config, data, deploy, or `none`
6. `## Plan deviation`: `➕` / `⚠️` / skipped lines plus drift the model notices between plan and diff
7. `## Backlog`: files added under `backlog_dir` on this branch, with titles
8. `## External review`: `### <reviewer key>` with Go numbers, then `finding -> fixed | dismissed (reason) | filed to backlog`
9. `## Validation`: plan validation commands and timing aggregates

### `--report` resolution

`resolveFeatureBranch(plan)` → `git show <branch>:<plans_dir>/completed/<stem>.report.md` →
fallback to the working-tree file → error naming the plan. Output starts with `branch: <name>`.

## Post-Completion

**Manual verification**:
- On the `make e2e-prep` toy repository, run a plan with a two-reviewer chain and confirm the archival
  commit carries both files, the report has per-reviewer findings, and `loopai --report` prints it.
- Rerun with `report_enabled = false` and confirm the commit message and log are unchanged.
- Reinstall the plugin so the new `loopai-merge` skill is available, then run `/loopai-merge <plan>`
  on the toy repository and confirm the narration precedes the confirmation prompt.

**Follow-ups**:
- Consider a report for `--review` / `--external-only` runs stored under `.loopai/progress/` since
  those modes never archive a plan.
- Consider surfacing the report link in the completion notification and the dashboard.

## Validation Commands

- `make test`
- `make lint`
- `GOOS=windows GOARCH=amd64 go build ./...`
