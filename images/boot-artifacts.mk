ALPINE_VERSION ?= 3.22.2
ALPINE_BRANCH ?= v3.22
ARCH ?= aarch64

ifeq ($(ARCH),aarch64)
ALPINE_ARCH ?= aarch64
APKO_ARCH ?= aarch64
else ifeq ($(ARCH),x86_64)
ALPINE_ARCH ?= x86_64
APKO_ARCH ?= x86_64
else
$(error unsupported ARCH: $(ARCH))
endif

ALPINE_BASE_URL ?= https://dl-cdn.alpinelinux.org/alpine/$(ALPINE_BRANCH)/releases/$(ALPINE_ARCH)
APKO_IMAGE ?= cgr.dev/chainguard/apko@sha256:44ee5c39a8e42006372bd66625ac9be0eef78082777d1fcad57013fa84fe53ed
ROOTFS_TOOLS_IMAGE ?= alpine:$(ALPINE_VERSION)
REPO_ROOT ?= ../..
ROLE_DIR ?= images/$(ROLE)
OUT ?= out
GUESTD ?= ../../dist/guestd/$(ARCH)/guestd
VMLINUX ?= $(OUT)/vmlinuz-virt
KERNEL ?= $(OUT)/vmlinuz
INITRAMFS_BASE ?= $(OUT)/initramfs-virt
INITRAMFS ?= $(OUT)/initramfs
INITRAMFS_ROOT ?= $(OUT)/initramfs-root
MODLOOP ?= $(OUT)/modloop-virt
MODLOOP_ROOT ?= $(OUT)/modloop
ROOTFS ?= $(OUT)/rootfs.ext4
APKO_CONFIG ?= apko.yaml
APKO_LOCK ?= apko.$(APKO_ARCH).lock.json
GUESTD_INPUT_PATHS := go.mod go.sum cmd/guestd internal scripts/build-guestd-linux.sh
GUESTD_INPUT_STAMP := $(dir $(GUESTD)).guestd-inputs.$(ARCH).sha256

.PHONY: all clean guestd apko-lock force-guestd

all: $(KERNEL) $(INITRAMFS) $(ROOTFS)

$(OUT):
	mkdir -p $(OUT)

guestd:
	$(MAKE) $(GUESTD)

# Recompute a content hash for guestd inputs on every make invocation so role
# renames and branch switches cannot silently reuse a stale binary.
$(GUESTD): force-guestd
	@set -eu; \
	mkdir -p "$(dir $(GUESTD))"; \
	input_hash=$$(cd "$(REPO_ROOT)" && { \
		git ls-files -z --cached --others --exclude-standard -- $(GUESTD_INPUT_PATHS) | \
			xargs -0 shasum -a 256; \
		printf '%s\n' "$(ARCH)"; \
	} | shasum -a 256 | awk '{print $$1}'); \
	current_hash=$$(cat "$(GUESTD_INPUT_STAMP)" 2>/dev/null || true); \
	if [ ! -x "$(GUESTD)" ] || [ "$$input_hash" != "$$current_hash" ]; then \
		if [ "$${HELMR_GUESTD_BUILT:-}" = "1" ]; then \
			test -x "$(GUESTD)"; \
		else \
			(cd "$(REPO_ROOT)" && ARCH=$(ARCH) GUESTD_OUTPUT="$(abspath $(GUESTD))" ./scripts/build-guestd-linux.sh); \
		fi; \
		printf '%s\n' "$$input_hash" > "$(GUESTD_INPUT_STAMP)"; \
	fi

force-guestd:

$(VMLINUX): | $(OUT)
	curl -fL --output $@ $(ALPINE_BASE_URL)/netboot/vmlinuz-virt

$(KERNEL): $(VMLINUX)
	ruby -rzlib -rstringio -e 'data = File.binread(ARGV[0]); offset = data.index("\x1f\x8b\x08".b) or abort("gzip payload not found in #{ARGV[0]}"); File.binwrite(ARGV[1], Zlib::GzipReader.new(StringIO.new(data.byteslice(offset..))).read)' $< $@

$(INITRAMFS_BASE): | $(OUT)
	curl -fL --output $@ $(ALPINE_BASE_URL)/netboot/initramfs-virt

$(MODLOOP): | $(OUT)
	curl -fL --output $@ $(ALPINE_BASE_URL)/netboot/modloop-virt

$(INITRAMFS): $(INITRAMFS_BASE) $(MODLOOP) ../build-initramfs.sh | $(OUT)
	ROOTFS_TOOLS_IMAGE=$(ROOTFS_TOOLS_IMAGE) ../build-initramfs.sh $(INITRAMFS) $(INITRAMFS_BASE) $(MODLOOP) $(INITRAMFS_ROOT) $(MODLOOP_ROOT)

apko-lock: $(APKO_LOCK)

$(APKO_LOCK): $(APKO_CONFIG)
	docker run --rm -v "$(abspath $(REPO_ROOT))":/work -w /work/$(ROLE_DIR) $(APKO_IMAGE) lock $(APKO_CONFIG) --arch $(APKO_ARCH) --output $(APKO_LOCK)

$(ROOTFS): $(APKO_CONFIG) $(APKO_LOCK) $(INITRAMFS) $(ROLE_ROOTFS_DEPS) ../build-rootfs.sh | $(OUT)
	ARCH=$(ARCH) APKO_ARCH=$(APKO_ARCH) APKO_LOCK=$(APKO_LOCK) APKO_IMAGE=$(APKO_IMAGE) ROOTFS_TOOLS_IMAGE=$(ROOTFS_TOOLS_IMAGE) ../build-rootfs.sh $(ROLE) "$(abspath $(REPO_ROOT))" "$(ROLE_DIR)" "$(OUT)" "$(ROOTFS)" "$(GUESTD)"

clean:
	rm -rf $(OUT)
