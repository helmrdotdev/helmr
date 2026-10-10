package builder

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestFinalizeBundleWritesExactAtomicDirectory(t *testing.T) {
	root := t.TempDir()
	programPath, programBytes, index := writeVerifiedProgramFixture(t, root)
	input := testBundleInput(t, programPath, programBytes)
	input.Program.Metadata = index
	output := filepath.Join(root, "output", "deployment-bundle")

	finalized, err := FinalizeBundle(context.Background(), output, input)
	if err != nil {
		t.Fatal(err)
	}
	if finalized.Digest == "" || finalized.Bundle.Contract != bundle.Contract {
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
	if _, err := bundle.ReadDirectory(output); err != nil {
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
		testBundleInput(t, programPath, programBytes),
	); err == nil || !strings.Contains(err.Error(), "verify finalized Program object") {
		t.Fatalf("FinalizeBundle error = %v", err)
	}
}

func TestVerifyFinalObjectRejectsStructurallyInvalidComputerSeed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.oci.tar")
	if err := os.WriteFile(path, []byte("not an OCI archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := verifyFinalObject(
		context.Background(),
		path,
		bundle.Object{
			Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 18,
			MediaType: definition.ComputerSeedMediaType,
		},
		artifact.ProgramOutput{},
	)
	if err == nil || !strings.Contains(err.Error(), "verify finalized Computer seed object") {
		t.Fatalf("verifyFinalObject error = %v", err)
	}
}

func TestReferencedBundleObjectsDeduplicatesSharedComputerSeed(t *testing.T) {
	program := artifact.ProgramDescriptor{
		Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 10,
		MediaType: artifact.ProgramArtifactMediaType,
	}
	image := bundle.ComputerSeed{
		DeclaredID: "first",
		Artifact: bundle.ComputerSeedArtifact{
			Profile:      definition.ComputerSeedProfile,
			Architecture: definition.ArchitectureX8664,
			Digest:       "sha256:" + strings.Repeat("b", 64), SizeBytes: 20,
			MediaType: definition.ComputerSeedMediaType,
		},
	}
	shared := image
	shared.DeclaredID = "second"
	objects, err := referencedBundleObjects(program, []bundle.ComputerSeed{image, shared})
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 {
		t.Fatalf("objects = %+v", objects)
	}

	shared.Artifact.SizeBytes++
	if _, err := referencedBundleObjects(
		program,
		[]bundle.ComputerSeed{image, shared},
	); err == nil || !strings.Contains(err.Error(), "conflicting reference metadata") {
		t.Fatalf("referencedBundleObjects error = %v", err)
	}
}

func TestFinalizeBundlePublishesExactlyOneConcurrentWriter(t *testing.T) {
	root := t.TempDir()
	programPath, programBytes, index := writeVerifiedProgramFixture(t, root)
	input := testBundleInput(t, programPath, programBytes)
	input.Program.Metadata = index
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
	if _, err := bundle.ReadDirectory(output); err != nil {
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
				input.Program.Metadata.RuntimeDigest = input.Runtime.Digest
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
			input := testBundleInput(t, programPath, programBytes)
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
		testBundleInput(t, programPath, programBytes),
	); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("FinalizeBundle error = %v", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "keep" {
		t.Fatalf("existing output changed: got %q, err %v", got, err)
	}
}

func testBundleInput(t *testing.T, programPath string, programBytes []byte) BundleInput {
	runtimeDigest := "sha256:" + strings.Repeat("f", 64)
	programDigest := sha256sum.DigestBytes(programBytes)
	image, source := writeSeedFixture(t, filepath.Dir(programPath))
	index := fixtureProgramMetadata([]bundle.ComputerSeed{image}, "sha256:"+strings.Repeat("c", 64), runtimeDigest)

	return BundleInput{
		Runtime: artifact.RuntimeDescriptor{
			Architecture: definition.ArchitectureX8664,
			Digest:       runtimeDigest, FormatVersion: artifact.RuntimeDescriptorFormatVersion,
			MediaType:       artifact.RuntimeArtifactMediaType,
			RuntimeContract: definition.RuntimeContract, SizeBytes: 4096,
		},
		Program: artifact.ProgramOutput{
			Artifact: artifact.ProgramDescriptor{
				Digest: programDigest, SizeBytes: int64(len(programBytes)),
				MediaType: artifact.ProgramArtifactMediaType,
			},
			Metadata: index,
		},
		ComputerSeeds: []bundle.ComputerSeed{image},
		Objects:       []ObjectSource{{Digest: programDigest, Path: programPath}, source},
	}
}

func writeVerifiedProgramFixture(
	t *testing.T,
	root string,
	images ...bundle.ComputerSeed,
) (string, []byte, artifact.ProgramMetadata) {
	return writeVerifiedProgramFixtureWithMetadata(t, root, nil, images...)
}

func writeVerifiedProgramFixtureWithMetadata(t *testing.T, root string, edit func(*artifact.ProgramMetadata), images ...bundle.ComputerSeed) (string, []byte, artifact.ProgramMetadata) {
	t.Helper()
	encoder := os.Getenv("HELMR_SQUASHFS_ENCODER")
	if encoder == "" {
		t.Skip("HELMR_SQUASHFS_ENCODER is not set")
	}
	configRaw := []byte(`{"assets":[],"dirs":["tasks"],"external":[],"ignorePatterns":[]}`)
	sourcePath := "helmr/app/entry-0.mjs"
	sourceRaw := []byte("export const build = {}\n")
	runtimeDigest := "sha256:" + strings.Repeat("f", 64)
	if len(images) == 0 {
		image, _ := writeSeedFixture(t, root)
		images = []bundle.ComputerSeed{image}
	}
	index := fixtureProgramMetadata(images, sha256sum.DigestBytes(configRaw), runtimeDigest)
	if edit != nil {
		edit(&index)
	}
	runtimeIndex := artifact.DefinitionIndex{APIVersion: "helmr.definition-index.v1", Agents: []artifact.AgentBundleEntry{{ID: "hello", ComputerDefinitionID: images[0].DeclaredID, ModulePath: sourcePath, ExportName: "build"}}, Computers: []artifact.ComputerBundleEntry{}}
	for _, image := range images {
		runtimeIndex.Computers = append(runtimeIndex.Computers, artifact.ComputerBundleEntry{ID: image.DeclaredID, ModulePath: sourcePath, ExportName: image.DeclaredID})
	}
	definitionIndexRaw, err := artifact.CanonicalDefinitionIndex(runtimeIndex)
	if err != nil {
		t.Fatal(err)
	}

	indexRaw, err := artifact.CanonicalProgramMetadata(index)
	if err != nil {
		t.Fatal(err)
	}
	manifest := artifact.ProgramManifest{
		FormatVersion: artifact.ProgramManifestFormatVersion,
		Config: artifact.ProgramPathDigest{
			Digest: sha256sum.DigestBytes(configRaw), Path: "helmr/config.json",
		},
		ProgramMetadataDigest: sha256sum.DigestBytes(indexRaw),
	}
	files := map[string][]byte{
		"bun.lock":                    []byte("lockfileVersion = 1\n"),
		"helmr.config.ts":             []byte("export default { dirs: [\"tasks\"] };\n"),
		"helmr/config.json":           configRaw,
		"helmr/program-metadata.json": indexRaw,
		"helmr/definition-index.json": definitionIndexRaw,
		"package.json":                []byte(`{"packageManager":"yarn@4.9.2"}`),
		sourcePath:                    sourceRaw,
	}
	directories := []string{"helmr", "helmr/app", "node_modules", "tasks"}
	inputRoot := t.TempDir()
	for _, name := range directories {
		if err := os.MkdirAll(filepath.Join(inputRoot, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(inputRoot, name), body, 0644); err != nil {
			t.Fatal(err)
		}
	}
	inputDigest, err := programPayloadDigest(t.Context(), inputRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest.PayloadDigest = inputDigest
	files["helmr/program-manifest.json"] = canonicalJSON(t, manifest)
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

func fixtureProgramMetadata(images []bundle.ComputerSeed, configDigest, runtimeDigest string) artifact.ProgramMetadata {
	index := artifact.ProgramMetadata{Architecture: definition.ArchitectureX8664, ConfigResultDigest: configDigest, RuntimeContract: definition.RuntimeContract, RuntimeDigest: runtimeDigest,
		Definitions: []artifact.ProgramDefinition{{Kind: definition.KindAgent, DeclaredID: "hello", Agent: &definition.AgentManifest{ComputerDefinitionID: images[0].DeclaredID, Triggers: map[string]definition.CronTrigger{}}}}}
	for _, image := range images {
		index.Definitions = append(index.Definitions, artifact.ProgramDefinition{Kind: definition.KindComputer, DeclaredID: image.DeclaredID, Computer: &definition.ComputerManifest{
			Seed: definition.ComputerSeedManifest{ArtifactDigest: image.Artifact.Digest, MediaType: image.Artifact.MediaType, Profile: image.Artifact.Profile, Config: image.Artifact.Config}, Resources: definition.ResourcesManifest{MilliCPU: 1000, MemoryMiB: 1024}, Secrets: []definition.SecretBinding{}, BuildSecrets: []definition.SecretBinding{},
		}})
	}
	sort.Slice(index.Definitions, func(i, j int) bool {
		return artifact.CompareProgramDefinitions(index.Definitions[i], index.Definitions[j]) < 0
	})
	return index
}

// The finalizer checks the seed container, not bootability. A sparse zero disk
// exercises that contract without a filesystem builder on the test host.
func writeSeedFixture(t *testing.T, root string) (bundle.ComputerSeed, ObjectSource) {
	t.Helper()
	var encoded bytes.Buffer
	encoded.WriteString("helmr-firecracker-filepack-v0\n")
	header := []byte(fmt.Sprintf(`{"version":0,"role":"computer-seed","logical_size":%d,"chunk_size":4194304,"codec":"zstd"}`, disk.SeedCapacity))
	if err := binary.Write(&encoded, binary.BigEndian, uint32(len(header))); err != nil {
		t.Fatal(err)
	}
	encoded.Write(header)
	encoded.WriteByte(255)
	raw := encoded.Bytes()
	digest := sha256sum.DigestBytes(raw)
	path := filepath.Join(root, "seed.filepack")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return bundle.ComputerSeed{DeclaredID: "computer", Artifact: bundle.ComputerSeedArtifact{Profile: definition.ComputerSeedProfile, Architecture: definition.ArchitectureX8664, Digest: digest, SizeBytes: int64(len(raw)), MediaType: definition.ComputerSeedMediaType}}, ObjectSource{Digest: digest, Path: path}
}
