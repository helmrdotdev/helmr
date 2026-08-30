package builder

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestFinalizeBundleWritesExactAtomicDirectory(t *testing.T) {
	root := t.TempDir()
	programPath, programBytes, index := writeVerifiedProgramFixture(t, root)
	input := testBundleInput(programPath, programBytes)
	input.Program.Index = index
	output := filepath.Join(root, "output", "deployment-bundle")

	finalized, err := FinalizeBundle(context.Background(), output, input)
	if err != nil {
		t.Fatal(err)
	}
	if finalized.Digest == "" || finalized.Bundle.Contract != deployment.DeploymentBundleContract {
		t.Fatalf("finalized = %+v", finalized)
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != "bundle.json" || entries[1].Name() != "objects" {
		t.Fatalf("output entries = %+v", entries)
	}
	if _, exists := finalized.Objects[input.Runtime.Digest]; exists {
		t.Fatal("Runtime release object was copied into the deployment bundle")
	}
	programDigest := input.Program.Artifact.Digest
	objectPath := filepath.Join(output, "objects", "sha256", strings.TrimPrefix(programDigest, "sha256:"))
	if finalized.Objects[programDigest] != objectPath {
		t.Fatalf("object path = %q, want %q", finalized.Objects[programDigest], objectPath)
	}
	got, err := os.ReadFile(objectPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(programBytes) {
		t.Fatalf("program bytes = %q", got)
	}
	if _, err := deployment.ReadDeploymentBundleDirectory(output); err != nil {
		t.Fatal(err)
	}
	partials, err := filepath.Glob(filepath.Join(filepath.Dir(output), ".deployment-bundle.partial-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(partials) != 0 {
		t.Fatalf("partial directories = %v", partials)
	}
}

func TestFinalizeBundleRejectsStructurallyInvalidProgram(t *testing.T) {
	root := t.TempDir()
	programPath := filepath.Join(root, "program.squashfs")
	programBytes := []byte("digest-correct but not a Program SquashFS")
	if err := os.WriteFile(programPath, programBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := FinalizeBundle(
		context.Background(),
		filepath.Join(root, "bundle"),
		testBundleInput(programPath, programBytes),
	); err == nil || !strings.Contains(err.Error(), "verify finalized Program object") {
		t.Fatalf("FinalizeBundle error = %v", err)
	}
}

func TestVerifyFinalObjectRejectsStructurallyInvalidWorkspaceImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.oci.tar")
	if err := os.WriteFile(path, []byte("not an OCI archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := verifyFinalObject(
		context.Background(),
		path,
		deployment.BundleObject{
			Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 18,
			MediaType: deployment.WorkspaceImageArtifactMediaType,
		},
		deployment.ProgramOutput{},
	)
	if err == nil || !strings.Contains(err.Error(), "verify finalized workspace image object") {
		t.Fatalf("verifyFinalObject error = %v", err)
	}
}

func TestReferencedBundleObjectsDeduplicatesSharedWorkspaceImage(t *testing.T) {
	program := deployment.ProgramDescriptor{
		Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 10,
		MediaType: deployment.ProgramArtifactMediaType,
	}
	image := deployment.BundleWorkspaceImage{
		DeclaredID: "first",
		Artifact: deployment.BundleWorkspaceImageArtifact{
			Architecture: deployment.ArchitectureX8664,
			Digest:       "sha256:" + strings.Repeat("b", 64), SizeBytes: 20,
			MediaType: deployment.WorkspaceImageArtifactMediaType,
		},
	}
	shared := image
	shared.DeclaredID = "second"
	objects, err := referencedBundleObjects(program, []deployment.BundleWorkspaceImage{image, shared})
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 {
		t.Fatalf("objects = %+v", objects)
	}

	shared.Artifact.SizeBytes++
	if _, err := referencedBundleObjects(
		program,
		[]deployment.BundleWorkspaceImage{image, shared},
	); err == nil || !strings.Contains(err.Error(), "conflicting reference metadata") {
		t.Fatalf("referencedBundleObjects error = %v", err)
	}
}

func TestFinalizeBundlePublishesExactlyOneConcurrentWriter(t *testing.T) {
	root := t.TempDir()
	programPath, programBytes, index := writeVerifiedProgramFixture(t, root)
	input := testBundleInput(programPath, programBytes)
	input.Program.Index = index
	output := filepath.Join(root, "bundle")
	start := make(chan struct{})
	errorsByWriter := make([]error, 2)
	var group sync.WaitGroup
	for writer := range errorsByWriter {
		group.Go(func() {
			<-start
			_, errorsByWriter[writer] = FinalizeBundle(context.Background(), output, input)
		})
	}
	close(start)
	group.Wait()
	succeeded, rejected := 0, 0
	for _, err := range errorsByWriter {
		switch {
		case err == nil:
			succeeded++
		case strings.Contains(err.Error(), "already exists"):
			rejected++
		default:
			t.Fatalf("concurrent FinalizeBundle error = %v", err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("concurrent results: succeeded=%d rejected=%d", succeeded, rejected)
	}
	if _, err := deployment.ReadDeploymentBundleDirectory(output); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizeBundleFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testing.T, *BundleInput, string)
		want   string
	}{
		{
			name: "digest mismatch",
			change: func(_ *testing.T, input *BundleInput, _ string) {
				input.Program.Artifact.Digest = "sha256:" + strings.Repeat("a", 64)
				input.Program.Index.RuntimeDigest = input.Runtime.Digest
				input.Objects[0].Digest = input.Program.Artifact.Digest
			},
			want: "digest does not match descriptor",
		},
		{
			name: "missing source",
			change: func(_ *testing.T, input *BundleInput, _ string) {
				input.Objects = nil
			},
			want: "sources do not match",
		},
		{
			name: "extra source",
			change: func(_ *testing.T, input *BundleInput, path string) {
				input.Objects = append(input.Objects, ObjectSource{
					Digest: "sha256:" + strings.Repeat("b", 64), Path: path,
				})
			},
			want: "sources do not match",
		},
		{
			name: "symlink source",
			change: func(t *testing.T, input *BundleInput, path string) {
				link := path + ".link"
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				input.Objects[0].Path = link
			},
			want: "not a regular file",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			programPath := filepath.Join(root, "program.squashfs")
			programBytes := []byte("verified program bytes")
			if err := os.WriteFile(programPath, programBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			input := testBundleInput(programPath, programBytes)
			test.change(t, &input, programPath)
			output := filepath.Join(root, "bundle")
			if _, err := FinalizeBundle(context.Background(), output, input); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("FinalizeBundle error = %v, want %q", err, test.want)
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("failed finalization left output: %v", err)
			}
			partials, err := filepath.Glob(filepath.Join(root, ".bundle.partial-*"))
			if err != nil {
				t.Fatal(err)
			}
			if len(partials) != 0 {
				t.Fatalf("failed finalization left partial directories: %v", partials)
			}
		})
	}
}

func TestFinalizeBundleRejectsExistingOutput(t *testing.T) {
	root := t.TempDir()
	programPath := filepath.Join(root, "program.squashfs")
	programBytes := []byte("verified program bytes")
	if err := os.WriteFile(programPath, programBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "bundle")
	if err := os.Mkdir(output, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(output, "owned-by-user")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := FinalizeBundle(
		context.Background(),
		output,
		testBundleInput(programPath, programBytes),
	); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("FinalizeBundle error = %v", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "keep" {
		t.Fatalf("existing output changed: got %q, err %v", got, err)
	}
}

func testBundleInput(programPath string, programBytes []byte) BundleInput {
	runtimeDigest := "sha256:" + strings.Repeat("f", 64)
	programDigest := sha256sum.DigestBytes(programBytes)
	index := deployment.ProgramIndex{
		Architecture:       deployment.ArchitectureX8664,
		ConfigResultDigest: "sha256:" + strings.Repeat("c", 64),
		Declarations: []deployment.ProgramIndexDeclaration{{
			Kind:       deployment.DefinitionKindTask,
			DeclaredID: "hello",
			Task: &deployment.TaskManifest{
				Payload: deployment.SchemaManifest{Kind: deployment.SchemaKindNone},
				Run: deployment.RunManifest{
					Queue: "tasks", MaxDurationMs: 5000,
					Retry: deployment.RetryManifest{Enabled: false},
				},
			},
			Locator: &deployment.ProgramLocator{
				ExportName: "hello",
				ModulePath: ".helmr/modules/" + strings.Repeat("d", 64) + ".mjs",
				Slot:       deployment.DeclarationSlotHandler,
			},
		}},
		Queues:          []deployment.QueueInput{{Name: "tasks"}},
		RuntimeContract: deployment.RuntimeContract,
		RuntimeDigest:   runtimeDigest,
	}
	return BundleInput{
		Runtime: deployment.RuntimeDescriptor{
			Architecture: deployment.ArchitectureX8664,
			Digest:       runtimeDigest, FormatVersion: deployment.RuntimeDescriptorFormatVersion,
			MediaType:       deployment.RuntimeArtifactMediaType,
			RuntimeContract: deployment.RuntimeContract, SizeBytes: 4096,
		},
		Program: deployment.ProgramOutput{
			Artifact: deployment.ProgramDescriptor{
				Digest: programDigest, SizeBytes: int64(len(programBytes)),
				MediaType: deployment.ProgramArtifactMediaType,
			},
			Index: index,
		},
		WorkspaceImages: []deployment.BundleWorkspaceImage{},
		Objects:         []ObjectSource{{Digest: programDigest, Path: programPath}},
	}
}

func writeVerifiedProgramFixture(
	t *testing.T,
	root string,
) (string, []byte, deployment.ProgramIndex) {
	t.Helper()
	encoder := os.Getenv("HELMR_SQUASHFS_ENCODER")
	if encoder == "" {
		t.Skip("HELMR_SQUASHFS_ENCODER is not set")
	}
	configRaw := []byte(`{"dirs":["tasks"],"ignorePatterns":[]}`)
	sourcePath := "tasks/build.ts"
	sourceRaw := []byte("export const build = task({ id: \"build\" })\n")
	moduleHash := sha256.Sum256([]byte(sourcePath))
	modulePath := "tasks/.helmr/modules/" + hex.EncodeToString(moduleHash[:]) + ".mjs"
	moduleRaw := []byte("export const build = {}\n")
	sourceMapRaw := canonicalJSON(t, map[string]any{
		"mappings": "AAAA", "names": []string{},
		"sources": []string{"file:///opt/helmr/program/tasks/build.ts"},
		"version": 3,
	})
	runtimeDigest := "sha256:" + strings.Repeat("f", 64)
	index := deployment.ProgramIndex{
		Architecture:       deployment.ArchitectureX8664,
		ConfigResultDigest: sha256sum.DigestBytes(configRaw),
		Declarations: []deployment.ProgramIndexDeclaration{{
			Kind: deployment.DefinitionKindTask, DeclaredID: "hello",
			Task: &deployment.TaskManifest{
				Payload: deployment.SchemaManifest{Kind: deployment.SchemaKindNone},
				Run: deployment.RunManifest{
					Queue: "tasks", MaxDurationMs: 5000,
					Retry: deployment.RetryManifest{Enabled: false},
				},
			},
			Locator: &deployment.ProgramLocator{
				ExportName: "build", ModulePath: modulePath,
				Slot: deployment.DeclarationSlotHandler,
			},
		}},
		Queues:          []deployment.QueueInput{{Name: "tasks"}},
		RuntimeContract: deployment.RuntimeContract, RuntimeDigest: runtimeDigest,
	}
	indexRaw, err := deployment.CanonicalProgramIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	manifestRaw := canonicalJSON(t, deployment.ProgramManifest{
		FormatVersion: deployment.ProgramManifestFormatVersion,
		Config: deployment.ProgramPathDigest{
			Digest: sha256sum.DigestBytes(configRaw), Path: "helmr/config.json",
		},
		ExternalEdges: []deployment.ProgramExternalEdge{},
		LocalPackages: []deployment.ProgramLocalPackage{},
		Modules: []deployment.ProgramModule{{
			ModuleDigest: sha256sum.DigestBytes(moduleRaw), ModulePath: modulePath,
			SourceMapDigest: sha256sum.DigestBytes(sourceMapRaw),
			SourceMapPath:   modulePath + ".map", SourcePath: sourcePath,
		}},
		ProgramIndexDigest: sha256sum.DigestBytes(indexRaw),
	})
	files := map[string][]byte{
		"bun.lock":                    []byte("lockfileVersion = 1\n"),
		"helmr.config.ts":             []byte("export default { dirs: [\"tasks\"] };\n"),
		"helmr/config.json":           configRaw,
		"helmr/declarations.json":     indexRaw,
		"helmr/entry.mjs":             []byte(deployment.ProgramEntry),
		"helmr/program-manifest.json": manifestRaw,
		modulePath:                    moduleRaw,
		modulePath + ".map":           sourceMapRaw,
		"package.json":                []byte(`{"packageManager":"yarn@4.9.2"}`),
		sourcePath:                    sourceRaw,
	}
	directories := []string{"helmr", "node_modules", "tasks", "tasks/.helmr", "tasks/.helmr/modules"}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	entries := append([]string(nil), directories...)
	for path := range files {
		entries = append(entries, path)
	}
	sort.Strings(entries)
	for _, path := range entries {
		body, regular := files[path]
		header := &tar.Header{
			Name: path, Uid: 0, Gid: 0, ModTime: time.Unix(0, 0),
			AccessTime: time.Unix(0, 0), ChangeTime: time.Unix(0, 0),
			Format: tar.FormatPAX,
		}
		if regular {
			header.Typeflag, header.Mode, header.Size = tar.TypeReg, 0o644, int64(len(body))
		} else {
			header.Typeflag, header.Mode = tar.TypeDir, 0o755
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if regular {
			if _, err := writer.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	programPath := filepath.Join(root, "program.squashfs")
	arguments := []string{
		"-", programPath, "-tar", "-noappend", "-all-root", "-no-xattrs",
		"-no-exports", "-no-fragments", "-no-tailends", "-no-duplicates",
		"-no-hardlinks", "-no-progress", "-exit-on-error", "-processors", "2",
		"-mem", "1024M", "-comp", "zstd", "-b", "131072", "-root-mode",
		"0755", "-mkfs-time", "0", "-all-time", "0",
	}
	command := exec.Command(encoder, arguments...)
	command.Env = []string{"LC_ALL=C", "TZ=UTC"}
	command.Stdin = bytes.NewReader(archive.Bytes())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("encode Program fixture: %v: %s", err, output)
	}
	programBytes, err := os.ReadFile(programPath)
	if err != nil {
		t.Fatal(err)
	}
	return programPath, programBytes, index
}

func canonicalJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}
