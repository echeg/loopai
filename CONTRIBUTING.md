# Contributing to loopai

loopai is a personal fork maintained for source builds and practical upstream synchronization. Keep contributions focused on the fork's current CLI and avoid reintroducing removed distribution or documentation infrastructure.

## Development setup

1. Clone the repository.
2. Install Go 1.26 or newer.
3. Install `golangci-lint`.
4. Run `make test` and `make lint`.
5. Build `.bin/loopai` with `make build`.

Optional dashboard tests require Playwright browsers:

```bash
make e2e-setup
make e2e
```

## Compatibility boundary

User-visible names, commands, configuration, and runtime directories use `loopai`.

Do not rename:

- the `github.com/umputun/ralphex` Go module or its import paths
- internal package identifiers solely for branding
- `<<<RALPHEX:...>>>` protocol signals

These names are intentionally retained to reduce upstream merge conflicts and preserve historical prompt/progress compatibility.

Do not add reads or automatic migration from legacy `~/.config/ralphex/` or `.ralphex/`; the fork intentionally uses only `~/.config/loopai/` and `.loopai/`.

## Code style

- Follow standard Go conventions.
- Keep comments lowercase except exported godoc.
- Wrap errors with context using `fmt.Errorf("context: %w", err)`.
- Define interfaces at the consumer side.
- Prefer existing dependencies.
- Use `filepath` and keep behavior cross-platform.
- Keep packages and changes small and cohesive.

## Tests

- Add or update tests with every behavior change.
- Prefer table-driven tests with `testify`.
- Cover successful and error paths.
- Use `t.TempDir()` for filesystem tests.
- Keep one corresponding `_test.go` file per source file.
- Never access real `~/.config/loopai/` or `~/.config/ralphex/` from tests.
- Do not reduce coverage or weaken assertions to make a change pass.

Before submitting:

```bash
make test
make lint
```

Run dashboard e2e and Windows cross-compilation when the affected code warrants them:

```bash
make e2e
GOOS=windows GOARCH=amd64 go build ./...
```

## Changes and pull requests

1. Branch from the appropriate base.
2. Make one focused change with tests.
3. Update README, CLAUDE.md, llms.txt, embedded config comments, or focused docs when behavior changes.
4. Run the full validation suite.
5. Write a meaningful commit message and pull-request description.

Keep pull requests reviewable. Split unrelated changes and large refactors into separate submissions. Do not edit `CHANGELOG.md` during normal development; it belongs to the release process.

New dependencies and broad architectural changes should be discussed before implementation.

## AI-assisted contributions

AI assistance is welcome, but contributors remain responsible for every submitted line.

- Review generated code and tests yourself.
- Check security, concurrency, error handling, and platform behavior.
- Ensure the change follows existing package and test patterns.
- Do not submit unreviewed bulk output for maintainers to repair.
- Be prepared to explain the implementation and its tradeoffs.

## Reporting issues

Include:

- loopai revision from `loopai --version`
- Go version
- operating system and architecture
- executor and relevant flags
- steps to reproduce
- expected and actual behavior
- relevant `.loopai/progress/` excerpts with secrets removed
