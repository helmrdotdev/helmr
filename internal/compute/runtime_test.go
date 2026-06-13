package compute

import (
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

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
	if want := sha256sum.DigestBytes(payload); got != want {
		t.Fatalf("runtime identity digest = %q, want %q", got, want)
	}
}
