package runtimeid

import (
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestRuntimeIdentityDigestMatchesCASDigest(t *testing.T) {
	runtime := Selector{
		Arch:            "amd64",
		Contract:        Contract,
		KernelDigest:    "sha256:kernel",
		InitramfsDigest: "sha256:initramfs",
		RootfsDigest:    "sha256:rootfs",
	}
	got, err := Digest(runtime)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(struct {
		Domain          string `json:"domain"`
		Backend         string `json:"backend"`
		Arch            string `json:"arch"`
		Contract        string `json:"contract"`
		KernelDigest    string `json:"kernel_digest"`
		InitramfsDigest string `json:"initramfs_digest"`
		RootfsDigest    string `json:"rootfs_digest"`
	}{
		Domain:          digestDomain,
		Backend:         "firecracker",
		Arch:            runtime.Arch,
		Contract:        runtime.Contract,
		KernelDigest:    runtime.KernelDigest,
		InitramfsDigest: runtime.InitramfsDigest,
		RootfsDigest:    runtime.RootfsDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := sha256sum.DigestBytes(payload); got != want {
		t.Fatalf("runtime identity digest = %q, want %q", got, want)
	}
}

func TestRuntimeArchitectureFromGo(t *testing.T) {
	for goArchitecture, want := range map[string]string{
		"amd64": "x86_64",
	} {
		got, err := ArchitectureFromGo(goArchitecture)
		if err != nil || got != want {
			t.Fatalf("RuntimeArchitectureFromGo(%q) = %q, %v; want %q", goArchitecture, got, err, want)
		}
	}
	if _, err := ArchitectureFromGo("x86_64"); err == nil {
		t.Fatal("canonical architecture was accepted as a Go architecture")
	}
	if _, err := ArchitectureFromGo("arm64"); err == nil {
		t.Fatal("unsupported arm64 was accepted")
	}
}
