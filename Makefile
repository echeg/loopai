# Get the latest commit branch, hash, and date
TAG=$(shell git describe --tags --abbrev=0 --exact-match 2>/dev/null)
BRANCH=$(if $(TAG),$(TAG),$(shell git rev-parse --abbrev-ref HEAD 2>/dev/null))
HASH=$(shell git rev-parse --short=7 HEAD 2>/dev/null)
TIMESTAMP=$(shell git log -1 --format=%ct HEAD 2>/dev/null | xargs -I{} date -u -r {} +%Y%m%dT%H%M%S)
GIT_REV=$(shell printf "%s-%s-%s" "$(BRANCH)" "$(HASH)" "$(TIMESTAMP)")
REV=$(if $(filter --,$(GIT_REV)),latest,$(GIT_REV))

WRAPPER_TESTS := \
	scripts/agy-as-claude/agy-as-claude_test.sh \
	scripts/codex-as-claude/codex-as-claude_test.sh \
	scripts/copilot-as-claude/copilot-as-claude_docs_test.sh \
	scripts/copilot-as-claude/copilot-as-claude_test.sh \
	scripts/gemini-as-claude/gemini-as-claude_test.sh \
	scripts/opencode/opencode-as-claude_test.sh \
	scripts/opencode/opencode-review_test.sh \
	scripts/pi-as-claude/pi-as-claude_docs_test.sh \
	scripts/pi-as-claude/pi-as-claude_test.sh

all: test build

build:
	cd cmd/loopai && go build -ldflags "-X main.revision=$(REV) -s -w" -o ../../.bin/loopai.$(BRANCH)
	cp .bin/loopai.$(BRANCH) .bin/loopai

check-symlinks:
	@./scripts/check-symlinks.sh

test-symlinks:
	@./scripts/check-symlinks_test.sh

check-plugin:
	@./scripts/check-plugin.sh

test-plugin:
	@./scripts/check-plugin_test.sh

test-wrappers:
	@set -e; for test_script in $(WRAPPER_TESTS); do \
		bash "$$test_script"; \
	done

test-completions:
	bash -n completions/loopai.bash
	@grep -Fqx 'complete -o default -F _loopai loopai' completions/loopai.bash
	@grep -Fqx '#compdef loopai' completions/loopai.zsh
	@grep -Fq 'complete -c loopai ' completions/loopai.fish
	@if grep -n 'ralphex' completions/loopai.*; then \
		echo "legacy command name found in loopai completions"; \
		exit 1; \
	fi
	@if command -v zsh >/dev/null 2>&1; then zsh -n completions/loopai.zsh; fi
	@if command -v fish >/dev/null 2>&1; then fish -n completions/loopai.fish; fi

test: check-symlinks test-symlinks check-plugin test-plugin test-completions
	go clean -testcache
	go test -race -coverprofile=coverage.out ./...
	grep -v "_mock.go" coverage.out | grep -v mocks > coverage_no_mocks.out
	go tool cover -func=coverage_no_mocks.out
	rm coverage.out coverage_no_mocks.out
	$(MAKE) test-wrappers

lint:
	golangci-lint run --max-issues-per-linter=0 --max-same-issues=0

fmt:
	gofmt -s -w $$(find . -type f -name "*.go" -not -path "./vendor/*" -not -path "./mocks/*" -not -path "**/mocks/*")
	goimports -w $$(find . -type f -name "*.go" -not -path "./vendor/*" -not -path "./mocks/*" -not -path "**/mocks/*")

race:
	go test -race -timeout=60s ./...

version:
	@echo "branch: $(BRANCH), hash: $(HASH), timestamp: $(TIMESTAMP)"
	@echo "revision: $(REV)"

e2e-setup:
	go run github.com/playwright-community/playwright-go/cmd/playwright@latest install --with-deps chromium

e2e:
	go test -v -failfast -count=1 -timeout=5m -tags=e2e ./e2e/...

e2e-ui:
	E2E_HEADLESS=false go test -v -failfast -count=1 -timeout=10m -tags=e2e ./e2e/...

e2e-prep: build
	@./scripts/internal/prep-toy-test.sh
	@cp .bin/loopai /tmp/loopai-test/.bin/loopai
	@echo ""
	@echo "=== E2E Full Test Ready ==="
	@echo "cd /tmp/loopai-test"
	@echo ".bin/loopai docs/plans/fix-issues.md"
	@echo ""
	@echo "Monitor: tail -f /tmp/loopai-test/.loopai/progress/progress-fix-issues.txt"

e2e-review: build
	@./scripts/internal/prep-review-test.sh
	@cp .bin/loopai /tmp/loopai-review-test/.bin/loopai
	@echo ""
	@echo "=== E2E Review Test Ready ==="
	@echo "cd /tmp/loopai-review-test"
	@echo ".bin/loopai --review"
	@echo ""
	@echo "Monitor: tail -f /tmp/loopai-review-test/.loopai/progress/progress-review.txt"

e2e-codex: build
	@./scripts/internal/prep-review-test.sh
	@cp .bin/loopai /tmp/loopai-review-test/.bin/loopai
	@echo ""
	@echo "=== E2E Codex-Only Test Ready ==="
	@echo "cd /tmp/loopai-review-test"
	@echo ".bin/loopai --codex --external-only"
	@echo ""
	@echo "Monitor: tail -f /tmp/loopai-review-test/.loopai/progress/progress-codex.txt"

.PHONY: all build check-symlinks test-symlinks check-plugin test-plugin test-wrappers test lint fmt race version e2e-setup e2e e2e-ui e2e-prep e2e-review e2e-codex
