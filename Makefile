SHELL := /usr/bin/env bash
.DEFAULT_GOAL := check

PROTO_FILES := $(shell find proto -name '*.proto' -type f | sort)
GO_PACKAGES := ./...

.PHONY: build tools generate generate-check no-fake-provider-adapters format-check lint type-check test check integration-test runtime-e2e beta-preflight beta-live-e2e provider-smoke clean

build:
	cd go && go build ./...
	uv run --frozen python -c "import harness_agents"

tools:
	mkdir -p .tools/bin
	@if command -v protoc-gen-go >/dev/null 2>&1 && \
		test "$$(protoc-gen-go --version)" = "protoc-gen-go v1.36.10"; then \
		cp "$$(command -v protoc-gen-go)" .tools/bin/protoc-gen-go; \
	else \
		cd go && GOBIN="$(CURDIR)/.tools/bin" \
			go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.10; \
	fi

generate: tools
	uv run --frozen python scripts/generate_proto.py

generate-check:
	./scripts/generate-check.sh

no-fake-provider-adapters:
	./scripts/no-fake-provider-adapters.sh
	@status=0; \
	./scripts/no-fake-provider-adapters.sh \
		scripts/testdata/no-fake-provider-adapters/split-provider-mode.go \
		>/dev/null 2>&1 || status=$$?; \
	if ((status != 1)); then \
		echo "multiline alternate-provider regression fixture was not rejected" >&2; \
		exit 1; \
	fi

format-check:
	test -z "$$(gofmt -l go)"
	uv run --frozen ruff format --check python scripts

lint: format-check
	cd go && go vet $(GO_PACKAGES)
	uv run --frozen ruff check python scripts

type-check:
	uv run --frozen ty check python/src/harness_agents \
		--exclude 'python/src/harness_agents/_generated/**'

test:
	cd go && go test $(GO_PACKAGES)
	uv run --frozen pytest

check: generate-check no-fake-provider-adapters lint type-check test build

integration-test:
	docker build -f Dockerfile.execution-worker -t basic-execution-worker:integration .
	docker build -f Dockerfile.verification-worker -t basic-verification-worker:integration .
	docker compose up -d --wait postgres
	@status=0; \
	cd go && TEST_DATABASE_URL='postgres://workflow:workflow@127.0.0.1:55433/workflow_test?sslmode=disable' \
		go test -tags=integration -count=1 \
			./internal/migration ./internal/workflow ./internal/registry ./internal/reasoning/gateway \
			./internal/execution ./internal/verification ./internal/approval \
			./internal/publication ./internal/controlapi || status=$$?; \
	docker compose down --volumes; \
	exit $$status

runtime-e2e:
	docker compose down --volumes
	docker compose up -d --wait postgres
	@status=0; \
	cd go && TEST_DATABASE_URL='postgres://workflow:workflow@127.0.0.1:55433/workflow_test?sslmode=disable' \
		go test -tags=integration -count=1 \
			-run '^TestRuntimeRecoversClaimUsingPreexistingArtifact$$' \
			./internal/orchestration || status=$$?; \
	docker compose down --volumes; \
	exit $$status

beta-preflight:
	@test -n "$(BETA_CONFIG)" || { echo "BETA_CONFIG is required" >&2; exit 2; }
	cd go && go run ./cmd/beta-preflight -config "$(BETA_CONFIG)"

beta-live-e2e:
	@test -n "$$ANTHROPIC_API_KEY" || { echo "ANTHROPIC_API_KEY is required" >&2; exit 2; }
	docker compose down --volumes
	docker build -f Dockerfile.execution-worker -t basic-execution-worker:runtime .
	docker build -f Dockerfile.verification-worker -t basic-verification-worker:runtime .
	mkdir -p .tools/runtime
	cd go && go build -o ../.tools/runtime/api-service ./cmd/api-service
	cd go && go build -o ../.tools/runtime/workflow-service ./cmd/workflow-service
	docker compose up -d --wait postgres
	@status=0; \
	cd go && TEST_DATABASE_URL='postgres://workflow:workflow@127.0.0.1:55433/workflow_test?sslmode=disable' \
		RUNTIME_API_BINARY="$(CURDIR)/.tools/runtime/api-service" \
		RUNTIME_WORKFLOW_BINARY="$(CURDIR)/.tools/runtime/workflow-service" \
		go test -tags=integration -count=1 \
			-run '^TestBetaLiveProcessesCompleteDisposableFixture$$' \
			./internal/runtime || status=$$?; \
	docker compose down --volumes; \
	exit $$status

provider-smoke:
	@test -n "$$ANTHROPIC_API_KEY" || { echo "ANTHROPIC_API_KEY is required" >&2; exit 2; }
	@test -n "$$ANTHROPIC_MODEL" || { echo "ANTHROPIC_MODEL is required" >&2; exit 2; }
	cd go && go test -tags=provider_smoke -count=1 \
		-run '^TestProviderSmoke$$' ./internal/reasoning/gateway

clean:
	rm -rf .cache .pytest_cache .ruff_cache .tools .venv
