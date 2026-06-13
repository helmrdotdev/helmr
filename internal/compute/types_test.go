package compute

import (
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
)

func TestResourceVectorFits(t *testing.T) {
	capacity := ResourceVector{
		MilliCPU:  4000,
		MemoryMiB: 8192,
		DiskMiB:   20480,
		Slots:     4,
	}

	if !capacity.Fits(ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 1024, Slots: 2}) {
		t.Fatal("expected capacity to satisfy smaller request")
	}
	if capacity.Fits(ResourceVector{MilliCPU: 5000, MemoryMiB: 4096, DiskMiB: 1024, Slots: 1}) {
		t.Fatal("expected CPU overcommit to fail")
	}
	if capacity.Fits(ResourceVector{MilliCPU: 1000, MemoryMiB: 16384, DiskMiB: 1024, Slots: 1}) {
		t.Fatal("expected memory overcommit to fail")
	}
	if capacity.Fits(ResourceVector{MilliCPU: 1000, MemoryMiB: 4096, DiskMiB: 1024, Slots: 5}) {
		t.Fatal("expected slot overcommit to fail")
	}
}

func TestRuntimeIdentityDigestMatchesCASDigest(t *testing.T) {
	runtime := RuntimeSelector{
		Arch:            "amd64",
		ABI:             "linux",
		KernelDigest:    "sha256:kernel",
		InitramfsDigest: "sha256:initramfs",
		RootfsDigest:    "sha256:rootfs",
		CNIProfile:      "default",
	}
	got, err := RuntimeIdentityDigest(runtime)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(struct {
		Schema          string `json:"schema"`
		Backend         string `json:"backend"`
		Arch            string `json:"arch"`
		ABI             string `json:"abi"`
		KernelDigest    string `json:"kernel_digest"`
		InitramfsDigest string `json:"initramfs_digest"`
		RootfsDigest    string `json:"rootfs_digest"`
		CNIProfile      string `json:"cni_profile"`
	}{
		Schema:          RuntimeIdentitySchema,
		Backend:         "firecracker",
		Arch:            runtime.Arch,
		ABI:             runtime.ABI,
		KernelDigest:    runtime.KernelDigest,
		InitramfsDigest: runtime.InitramfsDigest,
		RootfsDigest:    runtime.RootfsDigest,
		CNIProfile:      runtime.CNIProfile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := cas.DigestBytes(payload); got != want {
		t.Fatalf("runtime identity digest = %q, want %q", got, want)
	}
}
