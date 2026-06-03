GO ?= go
BUN ?= bun
BUF ?= buf
TOOLS_BIN := $(CURDIR)/.bin
CONSOLE_DIR := $(CURDIR)/packages/console
CONSOLE_OUT := $(CURDIR)/internal/console/out
export PATH := $(TOOLS_BIN):$(CURDIR)/node_modules/.bin:$(PATH)

SQLC_VERSION ?= v1.31.1
MIGRATE_VERSION ?= v4.19.1
PROTOC_GEN_GO_VERSION ?= v1.36.11
PROTOC_GEN_ES_VERSION ?= 2.11.0

.PHONY: all tools generate proto sqlc fmt test test-linux-compile lint build console-build verify dev dev-console-stack images boot-artifacts clean migration migrate-up migrate-down doctor doctor-linux

all: verify

tools:
	@mkdir -p $(TOOLS_BIN)
	GOBIN=$(TOOLS_BIN) $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	@if [ -x "$(CURDIR)/node_modules/.bin/protoc-gen-es" ]; then \
		ln -sf "$(CURDIR)/node_modules/.bin/protoc-gen-es" "$(TOOLS_BIN)/protoc-gen-es"; \
	else \
		printf '%s\n' '#!/usr/bin/env sh' 'exec bunx --bun @bufbuild/protoc-gen-es@$(PROTOC_GEN_ES_VERSION) "$$@"' > "$(TOOLS_BIN)/protoc-gen-es"; \
		chmod +x "$(TOOLS_BIN)/protoc-gen-es"; \
	fi

generate: proto sqlc

proto: tools
	$(BUF) generate proto --template proto/buf.gen.yaml --path proto/bundle.proto --path proto/run.proto

sqlc:
	$(GO) run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate

GO_CONSOLE_TAGS := -tags embed_console

fmt:
	$(GO) fmt ./...

test: console-build
	$(GO) test $(GO_CONSOLE_TAGS) ./...

test-linux-compile:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) test -c -o /tmp/helmr-guestd-linux-amd64.test ./cmd/guestd
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) test -c -o /tmp/helmr-firecracker-linux-amd64.test ./internal/firecracker
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) test -c -o /tmp/helmr-worker-linux-amd64.test ./cmd/helmr-worker
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) test -c -o /tmp/helmr-guestd-linux-arm64.test ./cmd/guestd
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) test -c -o /tmp/helmr-firecracker-linux-arm64.test ./internal/firecracker
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) test -c -o /tmp/helmr-worker-linux-arm64.test ./cmd/helmr-worker
	rm -f /tmp/helmr-guestd-linux-amd64.test /tmp/helmr-firecracker-linux-amd64.test /tmp/helmr-worker-linux-amd64.test /tmp/helmr-guestd-linux-arm64.test /tmp/helmr-firecracker-linux-arm64.test /tmp/helmr-worker-linux-arm64.test

lint: console-build
	$(GO) vet $(GO_CONSOLE_TAGS) ./...

build: console-build
	$(GO) build $(GO_CONSOLE_TAGS) ./cmd/...

console-build:
	$(BUN) run --cwd $(CONSOLE_DIR) build
	rm -rf $(CONSOLE_OUT)
	cp -R $(CONSOLE_DIR)/dist $(CONSOLE_OUT)

verify: generate fmt test lint build

dev: dev-console-stack

dev-console-stack:
	./scripts/dev-console-stack.sh

images boot-artifacts:
	$(MAKE) -C images/guest all

doctor:
	./scripts/doctor.sh auto

doctor-linux:
	./scripts/doctor.sh linux

migration:
	@test -n "$(name)" || (echo "usage: make migration name=add_thing" >&2; exit 1)
	@case "$(name)" in (*[!a-z0-9_]*) echo "migration name must use lowercase letters, digits, and underscores only" >&2; exit 1;; esac
	$(GO) run github.com/golang-migrate/migrate/v4/cmd/migrate@$(MIGRATE_VERSION) create -seq -digits 6 -ext sql -dir internal/db/schema/migrations "$(name)"

migrate-up:
	$(GO) run github.com/golang-migrate/migrate/v4/cmd/migrate@$(MIGRATE_VERSION) -path internal/db/schema/migrations -database "$$HELMR_DATABASE_URL" up

migrate-down:
	$(GO) run github.com/golang-migrate/migrate/v4/cmd/migrate@$(MIGRATE_VERSION) -path internal/db/schema/migrations -database "$$HELMR_DATABASE_URL" down 1

clean:
	rm -rf $(TOOLS_BIN) $(CONSOLE_OUT) $(CONSOLE_DIR)/dist dist images/*/out
