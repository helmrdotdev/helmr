package deployment

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestDependencyProjectEntriesContainOnlyManagerInputs(t *testing.T) {
	source := dependencyProjectionSource(t)
	entries, err := dependencyProjectEntries(source)
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.path)
		switch {
		case entry.path == "bun.lock",
			entry.path == "package.json",
			entry.path == "packages/tools/cli/package.json":
			if entry.mode != 0644 || len(entry.content) == 0 {
				t.Fatalf("file entry = %#v", entry)
			}
		default:
			if entry.mode != 0755 || entry.content != nil {
				t.Fatalf("directory entry = %#v", entry)
			}
		}
	}
	want := []string{
		"bun.lock",
		"package.json",
		"packages",
		"packages/tools",
		"packages/tools/cli",
		"packages/tools/cli/package.json",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("project paths = %#v, want %#v", got, want)
	}
	for _, entry := range entries {
		if strings.Contains(entry.path, "src") ||
			strings.Contains(entry.path, "nested") ||
			strings.Contains(entry.path, "node_modules") {
			t.Fatalf("project contains unrelated path %q", entry.path)
		}
	}

	entries[0].content[0] ^= 0xff
	if source.LockfileBytes[0] == entries[0].content[0] {
		t.Fatal("project entry aliases dependency source bytes")
	}
}

func TestManagerProjectArchiveMatchesLogicalProjection(t *testing.T) {
	source := dependencyProjectionSource(t)
	entries, err := dependencyProjectEntries(source)
	if err != nil {
		t.Fatal(err)
	}
	var first bytes.Buffer
	if err := writeManagerProjectArchive(
		context.Background(),
		&first,
		source,
	); err != nil {
		t.Fatal(err)
	}
	var second bytes.Buffer
	if err := writeManagerProjectArchive(
		context.Background(),
		&second,
		source,
	); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("manager project archive changed across identical writes")
	}
	if first.Len() > maxManagerProjectBytes {
		t.Fatalf("manager project archive size = %d", first.Len())
	}

	reader := tar.NewReader(bytes.NewReader(first.Bytes()))
	for index, entry := range entries {
		header, err := reader.Next()
		if err != nil {
			t.Fatalf("read manager project entry %d: %v", index, err)
		}
		if header.Name != entry.path || header.Mode != int64(entry.mode) {
			t.Fatalf("manager project header %d = %#v", index, header)
		}
		if len(header.PAXRecords) != 1 ||
			header.PAXRecords["path"] != entry.path {
			t.Fatalf(
				"manager project PAX records %q = %#v",
				entry.path,
				header.PAXRecords,
			)
		}
		wantType := byte(tar.TypeReg)
		if entry.mode == 0755 {
			wantType = tar.TypeDir
		}
		if header.Typeflag != wantType {
			t.Fatalf(
				"manager project type %q = %q, want %q",
				entry.path,
				header.Typeflag,
				wantType,
			)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(content, entry.content) {
			t.Fatalf(
				"manager project content %q = %q, want %q",
				entry.path,
				content,
				entry.content,
			)
		}
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("manager project archive end = %v, want EOF", err)
	}
	digest := sha256.Sum256(first.Bytes())
	const wantDigest = "c67f956795df126a759f1c3f0bff576896c87b444107865be9f9f681577ea26e"
	if got := fmt.Sprintf("%x", digest); got != wantDigest {
		t.Fatalf(
			"manager project archive digest = %q, want %q; size = %d",
			got,
			wantDigest,
			first.Len(),
		)
	}
	if first.Len() != 11776 {
		t.Fatalf("manager project archive size = %d, want 11776", first.Len())
	}
}

func TestManagerProjectArchiveRejectsInvalidSourceAndCancellation(t *testing.T) {
	source := dependencyProjectionSource(t)
	source.ManifestFiles[1].PackagePath = "../escape"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	if err := writeManagerProjectArchive(
		context.Background(),
		&output,
		source,
	); !errors.Is(err, ErrDependencySourceInvalid) {
		t.Fatalf("manager project invalid source error = %v", err)
	}

	if err := writeManagerProjectArchive(
		ctx,
		&output,
		dependencyProjectionSource(t),
	); err == nil {
		t.Fatal("writeManagerProjectArchive ignored cancellation")
	}
}

func TestManagerProjectPathEnforcesSharedDepth(t *testing.T) {
	atLimit := strings.Repeat("a/", maxArtifactDepth-1) + "a"
	if err := validateManagerProjectPath(atLimit); err != nil {
		t.Fatalf("manager project path at depth limit: %v", err)
	}
	overLimit := strings.Repeat("a/", maxArtifactDepth) + "a"
	if err := validateManagerProjectPath(overLimit); err == nil {
		t.Fatal("manager project path exceeded the depth limit")
	}
}

func TestDependencyProjectionRejectsMutatedPreflightState(t *testing.T) {
	tests := map[string]func(*DependencySource){
		"lock bytes": func(source *DependencySource) {
			source.LockfileBytes = append([]byte(nil), source.LockfileBytes...)
			source.LockfileBytes[0] ^= 0xff
		},
		"lock digest": func(source *DependencySource) {
			source.Lockfile.Digest = "sha256:" + strings.Repeat("0", 64)
		},
		"manifest bytes": func(source *DependencySource) {
			source.ManifestFiles[0].Bytes = append(
				[]byte(nil),
				source.ManifestFiles[0].Bytes...,
			)
			source.ManifestFiles[0].Bytes[0] ^= 0xff
		},
		"manifest digest": func(source *DependencySource) {
			source.ManifestFiles[0].ManifestDigest =
				"sha256:" + strings.Repeat("0", 64)
		},
		"manifest metadata": func(source *DependencySource) {
			value := "other"
			source.ManifestFiles[0].Name = &value
		},
		"manifest path": func(source *DependencySource) {
			source.ManifestFiles[1].PackagePath = "../escape"
		},
		"registry pins": func(source *DependencySource) {
			source.RegistryPins[0].Version = "4.4.4"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			source := dependencyProjectionSource(t)
			mutate(&source)
			if _, err := dependencyProjectEntries(source); !errors.Is(
				err,
				ErrDependencySourceInvalid,
			) {
				t.Fatalf("project error = %v", err)
			}
			if _, err := NewDependencyCacheInput(
				source,
				dependencyPlanFixture(t, PackageManagerBun, ArchitectureX8664),
			); !errors.Is(err, ErrDependencySourceInvalid) {
				t.Fatalf("cache input error = %v", err)
			}
		})
	}
}

func TestNewDependencyCacheInputBindsPreflightAndPlan(t *testing.T) {
	source := dependencyProjectionSource(t)
	x86 := dependencyPlanFixture(t, PackageManagerBun, ArchitectureX8664)
	input, err := NewDependencyCacheInput(source, x86)
	if err != nil {
		t.Fatal(err)
	}
	if input.PackageManager != source.PackageManager ||
		input.Lockfile != source.Lockfile ||
		input.RuntimeDigest != x86.ManagedRuntimeDigest ||
		input.Architecture != x86.Architecture ||
		input.MaterializerVersion != x86.MaterializerVersion {
		t.Fatalf("cache input = %#v", input)
	}
	localDigest, err := LocalManifestsDigest(source.LocalManifests)
	if err != nil {
		t.Fatal(err)
	}
	if input.LocalManifestsDigest != "sha256:"+hex.EncodeToString(localDigest[:]) {
		t.Fatalf("local manifest digest = %q", input.LocalManifestsDigest)
	}
	x86Key, err := DependencyCacheKey(input)
	if err != nil {
		t.Fatal(err)
	}

	arm := dependencyPlanFixture(t, PackageManagerBun, ArchitectureAArch64)
	armInput, err := NewDependencyCacheInput(source, arm)
	if err != nil {
		t.Fatal(err)
	}
	armKey, err := DependencyCacheKey(armInput)
	if err != nil {
		t.Fatal(err)
	}
	if x86Key == armKey {
		t.Fatal("architecture and dependency plan did not change the cache key")
	}

	npmPlan := dependencyPlanFixture(t, PackageManagerNPM, ArchitectureX8664)
	if _, err := NewDependencyCacheInput(source, npmPlan); !errors.Is(
		err,
		ErrDependencySourceInvalid,
	) {
		t.Fatalf("manager mismatch error = %v", err)
	}
}

func dependencyProjectionSource(t *testing.T) DependencySource {
	t.Helper()
	root := t.TempDir()
	writeLockTestFile(t, root, "package.json", `{
		"name":"app",
		"packageManager":"bun@1.3.10",
		"workspaces":["packages/*"]
	}`)
	writeLockTestFile(t, root, "packages/tools/cli/package.json", `{
		"name":"cli",
		"version":"1.0.0"
	}`)
	writeLockTestFile(t, root, "packages/tools/cli/src/index.ts", "export {};")
	writeLockTestFile(t, root, "packages/tools/cli/nested/bun.lock", `{}`)
	writeLockTestFile(t, root, "bun.lock", `{
		"lockfileVersion":1,
		"workspaces":{
			"":{"name":"app"},
			"packages/tools/cli":{"name":"cli","version":"1.0.0"}
		},
		"packages":{
			"cli":["cli@workspace:packages/tools/cli"],
			"zod":["zod@4.4.3","",{},"`+lockTestIntegrity()+`"]
		}
	}`)
	source, err := InspectDependencySource(
		root,
		PackageManager{Name: PackageManagerBun, Version: "1.3.10"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return source
}
