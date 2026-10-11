GO ?= go
BUN ?= bun
BUF ?= buf
SQLC ?= sqlc
CONSOLE_DIR := $(CURDIR)/packages/console
CONSOLE_OUT := $(CURDIR)/internal/console/out

MIGRATE_VERSION ?= v4.20.1

.PHONY: all tools generate platform-entries datapath-bpf proto sqlc fmt modernize modernize-check test go-test test-race go-test-race test-linux-compile lint go-lint build go-build console-build verify dev dev-console-stack dev-reset images boot-artifacts clean migration migrate-up migrate-down doctor doctor-linux

all: verify

tools:
	@command -v protoc-gen-go >/dev/null
	@command -v protoc-gen-es >/dev/null

generate: datapath-bpf proto sqlc

platform-entries:
	$(BUN) scripts/build-platform-entries.ts all

datapath-bpf:
	$(GO) generate ./internal/firecracker/datapath

proto: tools
	$(BUF) generate proto --template proto/buf.gen.yaml --path proto/computer.proto --path proto/agent.proto
	@for file in proto/typescript/src/gen/*.ts; do \
		awk 'NF { last = NR } { lines[NR] = $$0 } END { for (i = 1; i <= last; i++) print lines[i] }' "$$file" >"$$file.tmp"; \
		mv "$$file.tmp" "$$file"; \
	done

sqlc:
	$(SQLC) generate

GO_CONSOLE_TAGS := -tags embed_console

fmt:
	$(GO) fmt ./...

modernize: | platform-entries
	$(GO) fix ./...

modernize-check: | platform-entries
	$(GO) fix -diff ./...

test: go-test

go-test: | platform-entries console-build
	$(GO) test $(GO_CONSOLE_TAGS) ./...

test-race: go-test-race

go-test-race: | platform-entries console-build
	CGO_ENABLED=1 $(GO) test -race -count=1 $(GO_CONSOLE_TAGS) ./...

test-linux-compile:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) test -c -o /tmp/helmr-guestd-linux-amd64.test ./cmd/guestd
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) test -c -o /tmp/helmr-firecracker-linux-amd64.test ./internal/firecracker
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) test -c -o /tmp/worker-linux-amd64.test ./cmd/worker
	rm -f /tmp/helmr-guestd-linux-amd64.test /tmp/helmr-firecracker-linux-amd64.test /tmp/worker-linux-amd64.test

lint: go-lint

go-lint: | platform-entries console-build
	$(GO) vet $(GO_CONSOLE_TAGS) ./...
	staticcheck $(GO_CONSOLE_TAGS) ./...
	unparam ./...

build: go-build

go-build: | platform-entries console-build
	$(GO) build $(GO_CONSOLE_TAGS) ./cmd/...

console-build:
	$(BUN) run --cwd $(CONSOLE_DIR) build
	rm -rf $(CONSOLE_OUT)
	cp -R $(CONSOLE_DIR)/dist $(CONSOLE_OUT)

verify: generate fmt test lint build

dev: dev-console-stack

dev-console-stack:
	./dev/local/start.sh

dev-reset:
	./dev/local/reset.sh

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
	$(GO) run ./cmd/control-plane migrate up

migrate-down:
	$(GO) run github.com/golang-migrate/migrate/v4/cmd/migrate@$(MIGRATE_VERSION) -path internal/db/schema/migrations -database "$$DATABASE_URL" down 1

clean:
	rm -rf $(CONSOLE_OUT) $(CONSOLE_DIR)/dist dist images/*/out
	rm -f internal/compiler/program-compiler.mjs internal/hostconfig/config-evaluator.mjs internal/runtime/entry.mjs internal/runtime/module-preload.mjs
