package runtimeid

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/capacity"
)

func TestRuntimeIdentityDigestIsStable(t *testing.T) {
	runtime := Selector{
		Arch:                      "x86_64",
		Contract:                  Contract,
		VMRuntimeDescriptorDigest: "sha256:" + strings.Repeat("a", 64),
		FirecrackerDigest:         "sha256:" + strings.Repeat("b", 64),
		FirecrackerVersion:        "1.16.1",
		SnapshotFormatVersion:     "6.0.0",
		HostKernelRelease:         "6.8.0-1024-aws",
		CPUTemplate:               capacity.CPUTemplateSelector{Kind: capacity.CPUTemplateNone},
		KernelDigest:              "sha256:" + strings.Repeat("1", 64),
		InitramfsDigest:           "sha256:" + strings.Repeat("2", 64),
		RootfsDigest:              "sha256:" + strings.Repeat("3", 64),
	}
	got, err := Digest(runtime)
	if err != nil {
		t.Fatal(err)
	}
	want := "sha256:73c9df7f1c1d972683a196b6d36314f26182045579d914ba61565c30cb7f417c"
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
