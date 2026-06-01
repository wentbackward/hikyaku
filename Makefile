.PHONY: build build-hardened test test-short lint lint-hardened vet check check-hardened run docker-build install-hooks

VERSION ?= $(shell git describe --tags --dirty 2>/dev/null || echo dev)
LDFLAGS = -ldflags '-X main.Version=$(VERSION)'

build:
	go build $(LDFLAGS) -o bin/hikyaku ./cmd/hikyaku

# Hardened build strips the SIGUSR1 capture feature, log levels 3-4, and
# the prompt text from journal entries. See docs/security.md.
build-hardened:
	go build -tags hardened $(LDFLAGS) -o bin/hikyaku-hardened ./cmd/hikyaku

test:
	go test ./... -v -race -count=1

# Run tests multiple times with the race detector to catch intermittent races.
# Single runs can miss timing-dependent data races.
test-race-stress:
	@pass=0; fail=0; for i in $$(seq 1 20); do \
		echo "--- run $$i/20 ---"; \
		if go test ./... -race -count=1 2>&1 | tee /tmp/test-run-$$i.log | grep -q "FAIL"; then \
			echo "FAILED on run $$i — see /tmp/test-run-$$i.log"; \
			exit 1; \
		fi; \
	done; \
	echo "All 20 race-detector runs passed"

test-short:
	go test ./... -short

# Full lint. Falls back to `go vet` if golangci-lint isn't installed so CI and
# local still get basic checks. To match CI exactly, install golangci-lint:
#   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed — falling back to go vet"; \
		go vet ./...; \
	fi

lint-hardened:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run --build-tags hardened ./...; \
	else \
		echo "golangci-lint not installed — falling back to go vet"; \
		go vet -tags hardened ./...; \
	fi

vet:
	go vet ./...

# Run everything CI runs, in order.
check: lint test build

# Hardened-specific unit tests. We do NOT run the full suite under -tags
# hardened: the capture/journal packages (and the proxy capture tests) assert
# inspect-only features that hardened deliberately compiles out, so they would
# fail by design. These two packages hold the hardened enforcement assertions
# (TLS policy, https-only openai backends, keyed affinity hash, log gating).
test-hardened:
	go test -tags hardened -count=1 ./internal/config/ ./internal/balancer/

# Same as `check` but for the hardened build variant.
check-hardened: lint-hardened test-hardened
	go build -tags hardened ./cmd/hikyaku

# One-time setup for contributors: point git at the version-controlled
# .githooks/ directory so pre-commit runs golangci-lint before each commit.
install-hooks:
	git config core.hooksPath .githooks
	@echo "pre-commit hook installed (.githooks/pre-commit)"

run:
	CONFIG_PATH=config.example.yaml go run ./cmd/hikyaku

docker-build:
	docker build -t hikyaku .
