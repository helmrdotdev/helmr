package runtimeid

import (
	"testing"

	"github.com/helmrdotdev/helmr/capacityapi"
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
	want, err := (capacityapi.RuntimeProfile{
		Arch: runtime.Arch, Contract: runtime.Contract, KernelDigest: runtime.KernelDigest,
		InitramfsDigest: runtime.InitramfsDigest, RootfsDigest: runtime.RootfsDigest,
	}).ExpectedID()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
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
