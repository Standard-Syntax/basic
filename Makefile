SHELL := /usr/bin/env bash
.DEFAULT_GOAL := check

PROTO_FILES := $(shell find proto -name '*.proto' -type f | sort)
GO_PACKAGES := ./...
REPORT_OUTPUT ?= $(CURDIR)/.tools/evidence/python-project-report.json

.PHONY: build tools generate generate-check no-fake-provider-adapters format-check lint type-check test check integration-test runtime-e2e beta-preflight beta-images beta-deploy-smoke beta-live-e2e beta-python-project-e2e beta-canary-e2e beta-canary-cleanup beta-readiness provider-smoke clean

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
			./internal/execution ./internal/verification ./internal/release ./internal/approval \
			./internal/publication ./internal/controlapi ./internal/runtime ./internal/postgres || status=$$?; \
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

beta-images:
	$(eval SOURCE_REVISION := $(shell git rev-parse HEAD))
	docker build -f Dockerfile.execution-worker -t basic-execution-worker:beta .
	docker build -f Dockerfile.verification-worker -t basic-verification-worker:beta .
	docker build --build-arg SOURCE_REVISION="$(SOURCE_REVISION)" \
		-f Dockerfile.api-service -t basic-api-service:beta .
	docker build --build-arg SOURCE_REVISION="$(SOURCE_REVISION)" \
		-f Dockerfile.workflow-service -t basic-workflow-service:beta .
	SOURCE_REVISION="$(SOURCE_REVISION)" ./scripts/inspect-beta-images.sh

beta-deploy-smoke: beta-images
	@test -n "$(BETA_CONFIG)" || { echo "BETA_CONFIG is required" >&2; exit 2; }
	@mkdir -p .tools/beta
	@env_file=$$(mktemp); status=0; \
		trap 'docker compose -f compose.yaml -f compose.beta.yaml -p basic-beta-smoke --profile beta --env-file "$$env_file" down --volumes >/dev/null 2>&1 || true; rm -f "$$env_file"' EXIT; \
		cd go && go run ./cmd/beta-compose-env -config "$(BETA_CONFIG)" >"$$env_file"; cd ..; \
		docker compose -f compose.yaml -f compose.beta.yaml -p basic-beta-smoke --profile beta --env-file "$$env_file" \
			up -d --wait beta-postgres api-service workflow-service || status=$$?; \
		if ((status == 0)); then \
			claimed=$$(docker compose -f compose.yaml -f compose.beta.yaml -p basic-beta-smoke --profile beta --env-file "$$env_file" \
				exec -T beta-postgres psql -U workflow -d workflow -Atc \
				"SELECT count(*) FROM runtime_stage_jobs WHERE state='CLAIMED'"); \
			test "$$claimed" = 0 || status=1; \
		fi; \
		if ((status == 0)); then \
			cd go && go run ./cmd/beta-deployment-record -config "$(BETA_CONFIG)" \
				-output "$(CURDIR)/.tools/beta/deployment-record.json" || status=$$?; \
		fi; \
		exit $$status

beta-live-e2e:
	@test -n "$$ANTHROPIC_API_KEY" || { echo "ANTHROPIC_API_KEY is required" >&2; exit 2; }

ifneq ($(strip $(BETA_CONFIG)),)
	@set -e; \
		$(MAKE) beta-images; \
		env_file=$$(mktemp); trap 'rm -f "$$env_file"' EXIT; \
		cd go && go run ./cmd/beta-compose-env -config "$(BETA_CONFIG)" >"$$env_file"; cd ..; \
		set -a; source "$$env_file"; set +a; \
		test "$$(docker image inspect --format '{{.Id}}' basic-api-service:beta)" = "$$BETA_API_IMAGE"; \
		test "$$(docker image inspect --format '{{.Id}}' basic-workflow-service:beta)" = "$$BETA_WORKFLOW_IMAGE"; \
		test "$$(docker image inspect --format '{{.Id}}' basic-execution-worker:beta)" = "$$BETA_EXECUTION_IMAGE"; \
		test "$$(docker image inspect --format '{{.Id}}' basic-verification-worker:beta)" = "$$BETA_VERIFICATION_IMAGE"; \
		docker compose down --volumes; docker compose up -d --wait postgres; status=0; \
		cd go && TEST_DATABASE_URL='postgres://workflow:workflow@127.0.0.1:55433/workflow_test?sslmode=disable' \
			BETA_LIVE_E2E=1 \
			RUNTIME_PACKAGED=1 RUNTIME_API_BINARY="$$BETA_API_IMAGE" \
			RUNTIME_WORKFLOW_BINARY="$$BETA_WORKFLOW_IMAGE" \
			RUNTIME_EXECUTION_IMAGE="$$BETA_EXECUTION_IMAGE" \
			RUNTIME_VERIFICATION_IMAGE="$$BETA_VERIFICATION_IMAGE" \
			RUNTIME_DOCKER_GID="$$BETA_DOCKER_GID" \
			go test -v -tags=integration -count=1 -run '^TestBetaLiveProcessesCompleteDisposableFixture$$' \
			./internal/runtime || status=$$?; cd ..; \
		docker compose down --volumes; exit $$status
else
	docker compose down --volumes
	docker build -f Dockerfile.execution-worker -t basic-execution-worker:runtime .
	docker build -f Dockerfile.verification-worker -t basic-verification-worker:runtime .
	mkdir -p .tools/runtime
	cd go && go build -o ../.tools/runtime/api-service ./cmd/api-service
	cd go && go build -o ../.tools/runtime/workflow-service ./cmd/workflow-service
	docker compose up -d --wait postgres
	@status=0; \
	cd go && TEST_DATABASE_URL='postgres://workflow:workflow@127.0.0.1:55433/workflow_test?sslmode=disable' \
		BETA_LIVE_E2E=1 \
		RUNTIME_API_BINARY="$(CURDIR)/.tools/runtime/api-service" \
		RUNTIME_WORKFLOW_BINARY="$(CURDIR)/.tools/runtime/workflow-service" \
		go test -tags=integration -count=1 \
			-run '^TestBetaLiveProcessesCompleteDisposableFixture$$' \
			./internal/runtime || status=$$?; \
	docker compose down --volumes; \
	exit $$status
endif

beta-python-project-e2e:
	@mkdir -p "$(CURDIR)/.tools/evidence"
	@PROJECT_SPEC="$(PROJECT_SPEC)" CHECKS="$(CHECKS)" \
		REPORT_OUTPUT="$(REPORT_OUTPUT)" PRESERVE_PROJECT="$(PRESERVE_PROJECT)" \
		uv run --frozen python -m harness_agents.project_inputs
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
		BETA_LIVE_E2E=1 BETA_PYTHON_PROJECT=1 RUNTIME_SOURCE_ROOT="$(CURDIR)" \
		PROJECT_SPEC="$(PROJECT_SPEC)" CHECKS="$(CHECKS)" \
		REPORT_OUTPUT="$(REPORT_OUTPUT)" PRESERVE_PROJECT="$(PRESERVE_PROJECT)" \
		RUNTIME_API_BINARY="$(CURDIR)/.tools/runtime/api-service" \
		RUNTIME_WORKFLOW_BINARY="$(CURDIR)/.tools/runtime/workflow-service" \
		go test -v -tags=integration -count=1 \
			-run '^TestBetaLiveProcessesCompleteGeneratedPythonProject$$' \
			./internal/runtime || status=$$?; \
	docker compose down --volumes; \
	exit $$status

beta-canary-e2e:
	@test -n "$(BETA_CONFIG)" || { echo "BETA_CONFIG is required" >&2; exit 2; }
	@test -n "$$ANTHROPIC_API_KEY" || { echo "ANTHROPIC_API_KEY is required" >&2; exit 2; }
	cd go && go run ./cmd/beta-preflight -canary -config "$(BETA_CONFIG)"
	$(MAKE) beta-images
	@set -euo pipefail; \
		api_image=$$(docker image inspect --format '{{.Id}}' basic-api-service:beta); \
		workflow_image=$$(docker image inspect --format '{{.Id}}' basic-workflow-service:beta); \
		execution_image=$$(docker image inspect --format '{{.Id}}' basic-execution-worker:beta); \
		verification_image=$$(docker image inspect --format '{{.Id}}' basic-verification-worker:beta); \
		docker_gid=$$(stat -c '%g' /var/run/docker.sock); \
		cd go && BETA_CANARY=1 BETA_CONFIG="$(BETA_CONFIG)" RUNTIME_PACKAGED=1 \
		RUNTIME_API_BINARY="$$api_image" RUNTIME_WORKFLOW_BINARY="$$workflow_image" \
		RUNTIME_EXECUTION_IMAGE="$$execution_image" \
		RUNTIME_VERIFICATION_IMAGE="$$verification_image" RUNTIME_DOCKER_GID="$$docker_gid" \
		go test -v -tags=integration -count=1 \
			-run '^TestBetaCanaryProcessesPublishRealDraft$$' ./internal/runtime

beta-canary-cleanup:
	@test -n "$(BETA_CONFIG)" || { echo "BETA_CONFIG is required" >&2; exit 2; }
	@test -n "$(CANARY_PUBLICATION_ID)" || { echo "CANARY_PUBLICATION_ID is required" >&2; exit 2; }
	cd go && go run ./cmd/beta-canary-cleanup \
		-config "$(BETA_CONFIG)" -publication "$(CANARY_PUBLICATION_ID)"

beta-readiness:
	@test -n "$(RELEASE_MANIFEST)" || { echo "RELEASE_MANIFEST is required" >&2; exit 2; }
	cd go && go run ./cmd/beta-readiness -manifest "$(RELEASE_MANIFEST)"

provider-smoke:
	@test -n "$$ANTHROPIC_API_KEY" || { echo "ANTHROPIC_API_KEY is required" >&2; exit 2; }
	@test -n "$$ANTHROPIC_MODEL" || { echo "ANTHROPIC_MODEL is required" >&2; exit 2; }
	cd go && go test -tags=provider_smoke -count=1 \
		-run '^TestProviderSmoke$$' ./internal/reasoning/gateway

clean:
	rm -rf .cache .pytest_cache .ruff_cache .tools .venv
