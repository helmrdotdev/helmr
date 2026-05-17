ROLE := guest
REPO_ROOT := ../..
ADAPTER_BUNDLE_DEPS := $(shell find ../../runtime/typescript/src ../../sdk/typescript/src ../../proto/typescript/src -type f) $(wildcard ../../proto/*.proto) ../../package.json ../../bun.lock
ROLE_ROOTFS_DEPS = init.sh $(GUESTD) $(ADAPTER_BUNDLE_DEPS)

include ../boot-artifacts.mk
