package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/capacityapi"
	"github.com/helmrdotdev/helmr/internal/runtimeid"
)

func TestRuntimeProfileAndManifestPreserveValidatedArtifactIdentity(t *testing.T) {
	directory := t.TempDir()
	artifacts := map[string]map[string]any{}
	for _, item := range []struct{ name, contents string }{
		{name: "vmlinuz", contents: "kernel"},
		{name: "initramfs", contents: "initramfs"},
		{name: "rootfs.ext4", contents: "rootfs"},
	} {
		path := filepath.Join(directory, item.name)
		if err := os.WriteFile(path, []byte(item.contents), 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(item.contents))
		artifacts[item.name] = map[string]any{
			"path": item.name, "digest": "sha256:" + hex.EncodeToString(digest[:]), "size_bytes": len(item.contents),
		}
	}
	runtimeManifest := map[string]any{
		"schema": "helmr.runtime-artifacts.v0", "arch": "amd64", "vm_runtime_contract": runtimeid.Contract,
		"kernel": artifacts["vmlinuz"], "initramfs": artifacts["initramfs"], "rootfs": artifacts["rootfs.ext4"],
	}
	payload, _ := json.Marshal(runtimeManifest)
	if err := os.WriteFile(filepath.Join(directory, "runtime-artifacts.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	var profileOutput bytes.Buffer
	if err := runRuntimeProfile([]string{"--runtime-artifacts-dir", directory}, &profileOutput); err != nil {
		t.Fatal(err)
	}
	var profile capacityapi.RuntimeProfile
	if err := json.Unmarshal(profileOutput.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	if profile.Arch != "x86_64" || profile.ID == "" {
		t.Fatalf("runtime profile = %+v", profile)
	}
	var manifestProfileOutput bytes.Buffer
	if err := runRuntimeProfile([]string{"--runtime-artifacts-manifest", filepath.Join(directory, "runtime-artifacts.json")}, &manifestProfileOutput); err != nil {
		t.Fatal(err)
	}
	if manifestProfileOutput.String() != profileOutput.String() {
		t.Fatalf("profile from manifest = %s, want %s", manifestProfileOutput.String(), profileOutput.String())
	}
	profileFile := filepath.Join(t.TempDir(), "runtime-profile.json")
	if err := os.WriteFile(profileFile, profileOutput.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	arguments := []string{
		"--runtime-profile-file", profileFile, "--worker-version", "0123456789abcdef0123456789abcdef01234567", "--roles", "run,build",
		"--capacity-cpu-millis", "8000", "--capacity-memory-bytes", "17179869184",
		"--capacity-guest-disk-bytes", "137438953472", "--per-vm-cpu-millis", "4000",
		"--per-vm-memory-bytes", "8589934592", "--per-vm-guest-disk-bytes", "34359738368",
		"--vm-slots", "2", "--build-executors", "1", "--runtime-starts", "2",
	}
	var output bytes.Buffer
	if err := runManifest(arguments, &output); err != nil {
		t.Fatal(err)
	}
	var manifest capacityapi.WorkerReleaseManifest
	if err := json.Unmarshal(output.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if manifest.Runtime != profile || !manifest.SupportsRun || !manifest.SupportsBuild {
		t.Fatalf("manifest = %+v", manifest)
	}
}
