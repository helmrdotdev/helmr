GO ?= go
BUN ?= bun
BUF ?= buf
SQLC ?= sqlc
CONSOLE_DIR := $(CURDIR)/packages/console
CONSOLE_OUT := $(CURDIR)/internal/console/out

MIGRATE_VERSION ?= v4.19.1

.PHONY: all tools generate proto sqlc fmt test test-race test-linux-compile lint build console-build verify dev dev-console-stack images boot-artifacts clean migration migrate-up migrate-down doctor doctor-linux

all: verify

tools:
	@command -v protoc-gen-go >/dev/null
	@command -v protoc-gen-es >/dev/null

generate: proto sqlc

proto: tools
	$(BUF) generate proto --template proto/buf.gen.yaml --path proto/bundle.proto --path proto/run.proto

sqlc:
	$(SQLC) generate

GO_CONSOLE_TAGS := -tags embed_console

fmt:
	$(GO) fmt ./...

test: console-build
	$(GO) test $(GO_CONSOLE_TAGS) ./...

test-race: console-build
	CGO_ENABLED=1 $(GO) test -race $(GO_CONSOLE_TAGS) ./...

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
	rm -rf $(CONSOLE_OUT) $(CONSOLE_DIR)/dist dist images/*/out
