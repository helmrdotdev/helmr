package deployment

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
)

func TestInstallBuildPolicyPinsAndAtomicallyInstallsCanonicalBytes(t *testing.T) {
	raw, catalog, registry := buildPolicyInstallFixture(t)
	digest := digestBytes(raw)
	output := filepath.Join(t.TempDir(), "build-policy.json")
	if err := os.WriteFile(output, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := installBuildPolicy(
		context.Background(),
		runtimeObjectStore{
			object: cas.Object{
				Digest:    digest,
				SizeBytes: int64(len(raw)),
				MediaType: BuildPolicyMediaType,
			},
			body: raw,
		},
		digest,
		output,
		catalog,
		registry,
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

func TestInstallBuildPolicyRejectsAuthorityDrift(t *testing.T) {
	raw, catalog, registry := buildPolicyInstallFixture(t)
	digest := digestBytes(raw)
	base := runtimeObjectStore{
		object: cas.Object{
			Digest:    digest,
			SizeBytes: int64(len(raw)),
			MediaType: BuildPolicyMediaType,
		},
		body: raw,
	}
	tests := map[string]func(*runtimeObjectStore, **RuntimeCatalog, **ToolRegistry){
		"metadata": func(store *runtimeObjectStore, _ **RuntimeCatalog, _ **ToolRegistry) {
			store.object.MediaType = "application/octet-stream"
		},
		"bytes": func(store *runtimeObjectStore, _ **RuntimeCatalog, _ **ToolRegistry) {
			store.body = append([]byte(nil), raw...)
			store.body[len(store.body)-1] ^= 1
		},
		"catalog": func(_ *runtimeObjectStore, selected **RuntimeCatalog, _ **ToolRegistry) {
			other := testRuntimeDescriptor()
			other.Digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			*selected = authenticatedRuntimeCatalogForTest(t, []RuntimeDescriptor{other})
		},
		"registry": func(_ *runtimeObjectStore, _ **RuntimeCatalog, selected **ToolRegistry) {
			(*selected).authenticated = false
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			store := base
			selectedCatalog := catalog
			selectedRegistry := *registry
			registryPointer := &selectedRegistry
			mutate(&store, &selectedCatalog, &registryPointer)
			err := installBuildPolicy(
				context.Background(),
				store,
				digest,
				filepath.Join(t.TempDir(), "build-policy.json"),
				selectedCatalog,
				registryPointer,
				os.Getuid(),
				os.Getgid(),
			)
			if err == nil {
				t.Fatal("build policy drift was accepted")
			}
		})
	}
}

func TestInstallBuildPolicyRequiresCanonicalAbsoluteOutput(t *testing.T) {
	raw, catalog, registry := buildPolicyInstallFixture(t)
	digest := digestBytes(raw)
	store := runtimeObjectStore{
		object: cas.Object{
			Digest:    digest,
			SizeBytes: int64(len(raw)),
			MediaType: BuildPolicyMediaType,
		},
		body: raw,
	}
	if err := installBuildPolicy(
		context.Background(),
		store,
		digest,
		"build-policy.json",
		catalog,
		registry,
		os.Getuid(),
		os.Getgid(),
	); err == nil {
		t.Fatal("relative build policy output was accepted")
	}
}

func buildPolicyInstallFixture(
	t *testing.T,
) ([]byte, *RuntimeCatalog, *ToolRegistry) {
	t.Helper()
	runtime := testRuntimeDescriptor()
	registry, toolchainDigest := authenticatedToolRegistryForRuntime(t, runtime)
	document := buildPolicyForRuntime(runtime, toolchainDigest)
	document.ToolRegistryDigest = registry.digest
	raw, err := canonicalBuildPolicyDocument(document)
	if err != nil {
		t.Fatal(err)
	}
	return raw, authenticatedRuntimeCatalogForTest(t, []RuntimeDescriptor{runtime}), registry
}
