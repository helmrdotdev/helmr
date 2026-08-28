package runtimeid

import (
	"strings"
	"testing"
)

func TestRuntimeIdentityDigestIsStable(t *testing.T) {
	runtime := Profile{
		Arch:                      "x86_64",
		Contract:                  Contract,
		VMRuntimeDescriptorDigest: "sha256:" + strings.Repeat("a", 64),
		FirecrackerDigest:         "sha256:" + strings.Repeat("b", 64),
		FirecrackerVersion:        "1.16.1",
		SnapshotFormatVersion:     "6.0.0",
		HostKernelRelease:         "6.8.0-1024-aws",
		CPUTemplate:               CPUTemplateSelector{Kind: CPUTemplateNone},
		KernelDigest:              "sha256:" + strings.Repeat("1", 64),
		InitramfsDigest:           "sha256:" + strings.Repeat("2", 64),
		RootfsDigest:              "sha256:" + strings.Repeat("3", 64),
	}
	got, err := runtime.ExpectedID()
	if err != nil {
		t.Fatal(err)
	}
	want := "sha256:73c9df7f1c1d972683a196b6d36314f26182045579d914ba61565c30cb7f417c"
	if got != want {
		t.Fatalf("runtime identity digest = %q, want %q", got, want)
	}
}

func TestProfileRejectsInconsistentIdentity(t *testing.T) {
	profile := testProfile(t)
	profile.ID = "sha256:" + strings.Repeat("f", 64)
	if err := profile.Validate(); err == nil {
		t.Fatal("profile with a forged runtime identity was accepted")
	}
}

func TestProfileExpectedIDRejectsNoncanonicalSelector(t *testing.T) {
	profile := testProfile(t)
	profile.KernelDigest = strings.ToUpper(profile.KernelDigest)
	if _, err := profile.ExpectedID(); err == nil {
		t.Fatal("ExpectedID accepted a noncanonical selector")
	}
}

func TestProfileRejectsInvalidCPUTemplateSelector(t *testing.T) {
	profile := testProfile(t)
	profile.CPUTemplate.Digest = "sha256:" + strings.Repeat("4", 64)
	if _, err := profile.ExpectedID(); err == nil {
		t.Fatal("no-template selector with a digest was accepted")
	}
	profile.CPUTemplate = CPUTemplateSelector{Kind: CPUTemplateCustom}
	if _, err := profile.ExpectedID(); err == nil {
		t.Fatal("custom-template selector without a digest was accepted")
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

func testProfile(t *testing.T) Profile {
	t.Helper()
	profile := Profile{
		Arch: "x86_64", Contract: Contract,
		VMRuntimeDescriptorDigest: "sha256:" + strings.Repeat("a", 64),
		FirecrackerDigest:         "sha256:" + strings.Repeat("b", 64),
		FirecrackerVersion:        "1.16.1",
		SnapshotFormatVersion:     "6.0.0",
		HostKernelRelease:         "6.8.0-1024-aws",
		CPUTemplate:               CPUTemplateSelector{Kind: CPUTemplateNone},
		KernelDigest:              "sha256:" + strings.Repeat("1", 64),
		InitramfsDigest:           "sha256:" + strings.Repeat("2", 64),
		RootfsDigest:              "sha256:" + strings.Repeat("3", 64),
	}
	var err error
	profile.ID, err = profile.ExpectedID()
	if err != nil {
		t.Fatal(err)
	}
	return profile
}
