package deployment

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
)

func TestInstallRuntimePolicyPinsAndAtomicallyInstallsCanonicalBytes(t *testing.T) {
	raw, catalog := runtimePolicyInstallFixture(t)
	digest := digestBytes(raw)
	output := filepath.Join(t.TempDir(), "runtime-policy.json")
	if err := os.WriteFile(output, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := installRuntimePolicy(
		context.Background(),
		runtimeObjectStore{
			object: cas.Object{
				Digest:    digest,
				SizeBytes: int64(len(raw)),
				MediaType: RuntimePolicyMediaType,
			},
			body: raw,
		},
		digest,
		output,
		catalog,
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
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(output), ".runtime-policy-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary policies remain: %v", matches)
	}
}

func TestInstallRuntimePolicyRejectsObjectAndCatalogDrift(t *testing.T) {
	raw, catalog := runtimePolicyInstallFixture(t)
	digest := digestBytes(raw)
	base := runtimeObjectStore{
		object: cas.Object{
			Digest:    digest,
			SizeBytes: int64(len(raw)),
			MediaType: RuntimePolicyMediaType,
		},
		body: raw,
	}
	other := testRuntimeDescriptor()
	other.Digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	otherCatalog := authenticatedRuntimeCatalogForTest(t, []RuntimeDescriptor{other})
	tests := map[string]func(*runtimeObjectStore, **RuntimeCatalog){
		"metadata": func(store *runtimeObjectStore, _ **RuntimeCatalog) {
			store.object.MediaType = "application/octet-stream"
		},
		"bytes": func(store *runtimeObjectStore, _ **RuntimeCatalog) {
			store.body = append([]byte(nil), raw...)
			store.body[len(store.body)-1] ^= 1
		},
		"catalog": func(_ *runtimeObjectStore, selected **RuntimeCatalog) {
			*selected = otherCatalog
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			store := base
			selected := catalog
			mutate(&store, &selected)
			err := installRuntimePolicy(
				context.Background(),
				store,
				digest,
				filepath.Join(t.TempDir(), "runtime-policy.json"),
				selected,
				os.Getuid(),
				os.Getgid(),
			)
			if err == nil {
				t.Fatal("runtime policy drift was accepted")
			}
		})
	}
}

func TestInstallRuntimePolicyRequiresCanonicalAbsoluteOutput(t *testing.T) {
	raw, catalog := runtimePolicyInstallFixture(t)
	digest := digestBytes(raw)
	store := runtimeObjectStore{
		object: cas.Object{
			Digest:    digest,
			SizeBytes: int64(len(raw)),
			MediaType: RuntimePolicyMediaType,
		},
		body: raw,
	}
	if err := installRuntimePolicy(
		context.Background(),
		store,
		digest,
		"runtime-policy.json",
		catalog,
		os.Getuid(),
		os.Getgid(),
	); err == nil {
		t.Fatal("relative runtime policy output was accepted")
	}
}

func runtimePolicyInstallFixture(t *testing.T) ([]byte, *RuntimeCatalog) {
	t.Helper()
	descriptor := testRuntimeDescriptor()
	raw, err := canonicalRuntimePolicyDocument(runtimePolicyDocument{
		Current:       map[string]string{"us-east-1": descriptor.Digest},
		FormatVersion: RuntimePolicyFormatVersion,
		Runtimes:      []RuntimeDescriptor{descriptor},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw, authenticatedRuntimeCatalogForTest(t, []RuntimeDescriptor{descriptor})
}
