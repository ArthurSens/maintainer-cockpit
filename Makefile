SHELL := /bin/bash

CONFIG ?= config/maintainer-cockpit.yaml
DATABASE ?= data/maintainer-cockpit.db
LISTEN ?= 127.0.0.1:8767
WEAVER ?= weaver
GOLANGCI_LINT ?= golangci-lint
GOLANGCI_LINT_VERSION ?= 2.13.1
MDOX ?= go tool mdox
MDOX_FILES := $(filter-out docs/generated-telemetry.md,$(wildcard *.md docs/*.md docs/**/*.md .github/*.md))

.PHONY: start config-check docs docs-check lint test test-go test-js test-container telemetry-generate telemetry-check

start:
	go run ./cmd/maintainer-cockpit serve \
		--config "$(CONFIG)" \
		--database "$(DATABASE)" \
		--listen "$(LISTEN)"

config-check:
	go run ./cmd/maintainer-cockpit config-check --config "$(CONFIG)"

docs:
	$(MDOX) fmt --soft-wraps -l \
		--links.validate.config-file=.mdox.validate.yaml \
		$(MDOX_FILES)

docs-check:
	$(MDOX) fmt --soft-wraps --check -l \
		--links.validate.config-file=.mdox.validate.yaml \
		$(MDOX_FILES)

lint:
	@test "$$($(GOLANGCI_LINT) version | awk '{print $$4}')" = "$(GOLANGCI_LINT_VERSION)" || \
		(echo "golangci-lint v$(GOLANGCI_LINT_VERSION) is required" >&2; exit 1)
	$(GOLANGCI_LINT) run

test: test-go test-js

test-go:
	go test ./...

test-js:
	cd internal/web/static && node --test

test-container:
	./scripts/container-smoke.sh

telemetry-generate:
	@test "$$($(WEAVER) --version | awk '{print $$2}')" = "0.26.1" || \
		(echo "OpenTelemetry Weaver v0.26.1 is required" >&2; exit 1)
	$(WEAVER) registry check --registry telemetry/registry
	$(WEAVER) registry generate go internal/telemetry \
		--registry telemetry/registry --templates telemetry/templates --v2
	$(WEAVER) registry generate markdown docs \
		--registry telemetry/registry --templates telemetry/templates --v2
	mkdir -p examples/otel-lgtm/grafana/dashboards
	$(WEAVER) registry generate grafana examples/otel-lgtm/grafana/dashboards \
		--registry telemetry/registry --templates telemetry/templates --v2
	gofmt -w internal/telemetry/*_generated.go

telemetry-check:
	go test ./internal/telemetry
