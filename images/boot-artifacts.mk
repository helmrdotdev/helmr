ALPINE_VERSION ?= 3.22.2
ALPINE_BRANCH ?= v3.22
ARCH ?= x86_64
SHA256_COMMAND := $(shell if command -v sha256sum >/dev/null 2>&1; then printf '%s' sha256sum; elif command -v shasum >/dev/null 2>&1; then printf '%s' 'shasum -a 256'; fi)
ifeq ($(strip $(SHA256_COMMAND)),)
$(error sha256sum or shasum is required)
endif

ifeq ($(ARCH),x86_64)
ALPINE_ARCH ?= x86_64
APKO_ARCH ?= x86_64
else
$(error unsupported ARCH: $(ARCH))
endif

ALPINE_BASE_URL ?= https://dl-cdn.alpinelinux.org/alpine/$(ALPINE_BRANCH)/releases/$(ALPINE_ARCH)
ALPINE_NETBOOT_URL ?= $(ALPINE_BASE_URL)/netboot-$(ALPINE_VERSION)
REPO_ROOT ?= ../..
ROLE_DIR ?= images/$(ROLE)
OUT ?= out
GUESTD ?= ../../dist/guestd/$(ARCH)/guestd
VMLINUX ?= $(OUT)/vmlinuz-virt
KERNEL ?= $(OUT)/vmlinuz
INITRAMFS_BASE ?= $(OUT)/initramfs-virt
INITRAMFS ?= $(OUT)/initramfs
KERNEL_MODULES ?= $(OUT)/kernel-modules.tar
MODLOOP ?= $(OUT)/modloop-virt
ROOTFS ?= $(OUT)/rootfs.squashfs
RUNTIME_ARTIFACTS ?= $(OUT)/runtime-artifacts.json
NETBOOT_CHECKSUMS ?= ../guest/netboot.$(ARCH).sha256
BOOT_TOOLS_ARCHIVE ?= ../tools/out/boot-tools.tar
BOOT_TOOLS_IMAGE_ID_FILE ?= ../tools/out/boot-tools.image-id
BOOT_TOOLS_CONFIG ?= ../tools/apko.yaml
BOOT_TOOLS_LOCK ?= ../tools/apko.$(APKO_ARCH).lock.json
APKO_CONFIG ?= apko.yaml
APKO_LOCK ?= apko.$(APKO_ARCH).lock.json
GUESTD_INPUT_PATHS := go.mod go.sum cmd/guestd internal scripts/build-guestd-linux.sh
GUESTD_GO_IDENTITY := $(shell command -v go; go version)
GUESTD_INPUT_HASH := $(shell cd "$(REPO_ROOT)" && { git ls-files -z --cached --others --exclude-standard -- $(GUESTD_INPUT_PATHS) | while IFS= read -r -d '' path; do [ ! -f "$$path" ] || $(SHA256_COMMAND) "$$path"; done; printf '%s\n' "$(ARCH)" "$(GUESTD_GO_IDENTITY)"; } | $(SHA256_COMMAND) | awk '{print $$1}')
GUESTD_INPUT_STAMP := $(dir $(GUESTD)).guestd-inputs.$(ARCH).sha256

.PHONY: all clean guestd guestd-check guestd-stamp-current apko-lock boot-tools-image netboot-inputs

all: guestd-check
	$(MAKE) $(RUNTIME_ARTIFACTS)

$(OUT):
	mkdir -p $(OUT)

guestd: guestd-check

# Update one stable stamp only when guestd inputs change. The recursive all
# target then re-evaluates rootfs timestamps after this step completes.
guestd-check:
	@set -eu; \
	mkdir -p "$(dir $(GUESTD))"; \
	stamped_source_hash=$$(awk 'NF == 2 { print $$1 }' "$(GUESTD_INPUT_STAMP)" 2>/dev/null || true); \
	if [ "$$stamped_source_hash" != "$(GUESTD_INPUT_HASH)" ] || [ ! -x "$(GUESTD)" ]; then \
		if [ "$${HELMR_GUESTD_BUILT:-}" = "1" ]; then \
			test -x "$(GUESTD)"; \
		else \
			(cd "$(REPO_ROOT)" && ARCH=$(ARCH) GUESTD_OUTPUT="$(abspath $(GUESTD))" ./scripts/build-guestd-linux.sh); \
		fi; \
	fi; \
	binary_hash=$$($(SHA256_COMMAND) "$(GUESTD)" | awk '{print $$1}'); \
	expected="$(GUESTD_INPUT_HASH) $$binary_hash"; \
	current=$$(cat "$(GUESTD_INPUT_STAMP)" 2>/dev/null || true); \
	if [ "$$current" != "$$expected" ]; then \
		tmp="$(GUESTD_INPUT_STAMP).tmp"; \
		printf '%s\n' "$$expected" > "$$tmp"; \
		mv "$$tmp" "$(GUESTD_INPUT_STAMP)"; \
	fi

guestd-stamp-current:
	@set -eu; \
	test -x "$(GUESTD)"; \
	binary_hash=$$($(SHA256_COMMAND) "$(GUESTD)" | awk '{print $$1}'); \
	test "$$(cat "$(GUESTD_INPUT_STAMP)" 2>/dev/null || true)" = "$(GUESTD_INPUT_HASH) $$binary_hash"

netboot-inputs: $(NETBOOT_CHECKSUMS) ../fetch-netboot.sh | $(OUT)
	../fetch-netboot.sh $(NETBOOT_CHECKSUMS) $(ALPINE_NETBOOT_URL) $(OUT)

$(VMLINUX) $(INITRAMFS_BASE) $(MODLOOP): | netboot-inputs
	@test -f $@

$(KERNEL): $(VMLINUX)
	ruby -rzlib -rstringio -e 'data = File.binread(ARGV[0]); offset = data.index("\x1f\x8b\x08".b) or abort("gzip payload not found in #{ARGV[0]}"); File.binwrite(ARGV[1], Zlib::GzipReader.new(StringIO.new(data.byteslice(offset..))).read)' $< $@

$(BOOT_TOOLS_ARCHIVE): $(BOOT_TOOLS_CONFIG) $(BOOT_TOOLS_LOCK) ../build-tools-image.sh $(REPO_ROOT)/flake.lock
	BOOT_TOOLS_REBUILD=1 ../build-tools-image.sh $(BOOT_TOOLS_ARCHIVE) $(BOOT_TOOLS_IMAGE_ID_FILE)

boot-tools-image: $(BOOT_TOOLS_ARCHIVE)
	@if [ ! -s $(BOOT_TOOLS_IMAGE_ID_FILE) ] || ! docker image inspect "$$(cat $(BOOT_TOOLS_IMAGE_ID_FILE) 2>/dev/null)" >/dev/null 2>&1; then \
		../build-tools-image.sh $(BOOT_TOOLS_ARCHIVE) $(BOOT_TOOLS_IMAGE_ID_FILE); \
	fi

$(INITRAMFS) $(KERNEL_MODULES) &: $(INITRAMFS_BASE) $(MODLOOP) $(BOOT_TOOLS_ARCHIVE) ../build-initramfs.sh | $(OUT) boot-tools-image
	BOOT_TOOLS_IMAGE=$$(cat $(BOOT_TOOLS_IMAGE_ID_FILE)) ../build-initramfs.sh $(INITRAMFS) $(INITRAMFS_BASE) $(MODLOOP) $(KERNEL_MODULES)

apko-lock:
	apko lock $(APKO_CONFIG) --arch $(APKO_ARCH) --output $(APKO_LOCK)

$(ROOTFS): $(APKO_CONFIG) $(APKO_LOCK) $(INITRAMFS) $(KERNEL_MODULES) $(BOOT_TOOLS_ARCHIVE) $(ROLE_ROOTFS_DEPS) ../build-rootfs.sh $(REPO_ROOT)/flake.lock $(REPO_ROOT)/nix/packages/squashfs-tools.nix | $(OUT) boot-tools-image guestd-stamp-current
	ARCH=$(ARCH) APKO_ARCH=$(APKO_ARCH) APKO_LOCK=$(APKO_LOCK) BOOT_TOOLS_IMAGE=$$(cat $(BOOT_TOOLS_IMAGE_ID_FILE)) HELMR_SQUASHFS_ENCODER=$$(command -v mksquashfs) ../build-rootfs.sh $(ROLE) "$(abspath $(REPO_ROOT))" "$(ROLE_DIR)" "$(OUT)" "$(ROOTFS)" "$(GUESTD)" "$(KERNEL_MODULES)"

$(RUNTIME_ARTIFACTS): $(KERNEL) $(INITRAMFS) $(ROOTFS) ../boot-artifacts.mk
	@set -eu; \
	tmp="$@.tmp"; trap 'rm -f "$$tmp"' EXIT; \
	printf '{\n  "schema": "helmr.runtime-artifacts.v0",\n  "arch": "%s",\n  "vm_runtime_contract": "helmr.vm-runtime.v0",\n  "kernel": {"path": "%s", "digest": "sha256:%s", "size_bytes": %s},\n  "initramfs": {"path": "%s", "digest": "sha256:%s", "size_bytes": %s},\n  "rootfs": {"path": "%s", "digest": "sha256:%s", "size_bytes": %s}\n}\n' \
		"amd64" "$(notdir $(KERNEL))" "$$($(SHA256_COMMAND) "$(KERNEL)" | awk '{print $$1}')" "$$(wc -c < "$(KERNEL)" | tr -d ' ')" \
		"$(notdir $(INITRAMFS))" "$$($(SHA256_COMMAND) "$(INITRAMFS)" | awk '{print $$1}')" "$$(wc -c < "$(INITRAMFS)" | tr -d ' ')" \
		"$(notdir $(ROOTFS))" "$$($(SHA256_COMMAND) "$(ROOTFS)" | awk '{print $$1}')" "$$(wc -c < "$(ROOTFS)" | tr -d ' ')" > "$$tmp"; \
	mv "$$tmp" "$@"; trap - EXIT

clean:
	rm -rf $(OUT)
