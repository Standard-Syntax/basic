SHELL := /usr/bin/env bash
.DEFAULT_GOAL := check

PROTO_FILES := $(shell find proto -name '*.proto' -type f | sort)
GO_PACKAGES := ./...

.PHONY: build tools generate generate-check format-check lint test check clean

build:
	cd go && go build ./...
	uv run --frozen python -c "import harness_agents"

tools:
	mkdir -p .tools/bin
	cd go && GOBIN="$(CURDIR)/.tools/bin" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.10

generate: tools
	uv run --frozen python scripts/generate_proto.py

generate-check:
	./scripts/generate-check.sh

format-check:
	test -z "$$(gofmt -l go)"
	uv run --frozen ruff format --check python scripts

lint: format-check
	cd go && go vet $(GO_PACKAGES)
	uv run --frozen ruff check python scripts

test:
	cd go && go test $(GO_PACKAGES)
	uv run --frozen pytest

check: generate-check lint test build

clean:
	rm -rf .cache .pytest_cache .ruff_cache .tools .venv
