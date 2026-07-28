package deployment

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
)

func TestInstallBuildPolicyPinsAndAtomicallyInstallsCanonicalBytes(t *testing.T) {
	raw := testBuildPolicy(t)
	digest := digestBytes(raw)
	output := filepath.Join(t.TempDir(), "build-policy.json")
	if err := os.WriteFile(output, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := installBuildPolicy(
		context.Background(),
		runtimeObjectStore{
			object: cas.Object{
				Digest: digest, SizeBytes: int64(len(raw)), MediaType: BuildPolicyMediaType,
			},
			body: raw,
		},
		digest,
		output,
		os.Getuid(),
		os.Getgid(),
	)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(installed) != string(raw) {
		t.Fatalf("installed policy = %q, want %q", installed, raw)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Fatalf("installed policy mode = %#o", info.Mode().Perm())
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(output), ".build-policy-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary policies remain: %v", matches)
	}
}

func TestInstallBuildPolicyRejectsObjectDrift(t *testing.T) {
	raw := testBuildPolicy(t)
	digest := digestBytes(raw)
	tests := map[string]runtimeObjectStore{
		"metadata": {
			object: cas.Object{
				Digest: digest, SizeBytes: int64(len(raw)), MediaType: "application/octet-stream",
			},
			body: raw,
		},
		"bytes": {
			object: cas.Object{
				Digest: digest, SizeBytes: int64(len(raw)), MediaType: BuildPolicyMediaType,
			},
			body: append(append([]byte(nil), raw...), '\n'),
		},
	}
	for name, store := range tests {
		t.Run(name, func(t *testing.T) {
			if err := installBuildPolicy(
				context.Background(),
				store,
				digest,
				filepath.Join(t.TempDir(), "build-policy.json"),
				os.Getuid(),
				os.Getgid(),
			); err == nil {
				t.Fatal("build policy drift was accepted")
			}
		})
	}
}

func TestInstallBuildPolicyRequiresCanonicalAbsoluteOutput(t *testing.T) {
	raw := testBuildPolicy(t)
	digest := digestBytes(raw)
	store := runtimeObjectStore{
		object: cas.Object{
			Digest: digest, SizeBytes: int64(len(raw)), MediaType: BuildPolicyMediaType,
		},
		body: raw,
	}
	if err := installBuildPolicy(
		context.Background(),
		store,
		digest,
		"build-policy.json",
		os.Getuid(),
		os.Getgid(),
	); err == nil {
		t.Fatal("relative build policy output was accepted")
	}
}
