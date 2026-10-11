package deployment

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestFinalizeAdmitsPhysicalProgramBeforeRegisteringDefinitions(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	seed := finalizeDiskFixture(t)
	metadata := artifacttest.ProgramMetadata(t)
	metadata.Definitions[2].Computer.Seed.ArtifactDigest = sha256sum.DigestBytes(seed)
	metadata.ConfigResultDigest = sha256sum.DigestBytes([]byte(`{"assets":[],"dirs":["tasks"],"external":[],"ignorePatterns":[]}`))
	program := physicalProgram(t, metadata)
	output := artifact.ProgramOutput{Artifact: artifact.ProgramDescriptor{Digest: sha256sum.DigestBytes(program), SizeBytes: int64(len(program)), MediaType: artifact.ProgramArtifactMediaType}, Metadata: metadata}
	runtime := artifact.RuntimeDescriptor{Architecture: metadata.Architecture, Digest: metadata.RuntimeDigest, FormatVersion: artifact.RuntimeDescriptorFormatVersion, MediaType: artifact.RuntimeArtifactMediaType, RuntimeContract: metadata.RuntimeContract, SizeBytes: 4096}
	store := &physicalBundleStore{body: map[string][]byte{}, objects: map[string]cas.Object{}}
	store.add(output.Artifact.Digest, output.Artifact.MediaType, program)
	seedDigest := sha256sum.DigestBytes(seed)
	store.add(seedDigest, definition.ComputerSeedMediaType, seed)
	store.objects[runtime.Digest] = cas.Object{Digest: runtime.Digest, SizeBytes: runtime.SizeBytes, MediaType: runtime.MediaType}
	manifest := bundle.Manifest{Contract: bundle.Contract, Platform: bundle.Platform{Architecture: metadata.Architecture, OS: bundle.TargetOS}, Runtime: bundle.Runtime{Contract: runtime.RuntimeContract, Artifact: bundle.Object{Digest: runtime.Digest, SizeBytes: runtime.SizeBytes, MediaType: runtime.MediaType}}, Program: output, ComputerSeeds: []bundle.ComputerSeed{{DeclaredID: "repo", Artifact: bundle.ComputerSeedArtifact{Profile: definition.ComputerSeedProfile, Config: metadata.Definitions[2].Computer.Seed.Config, Architecture: metadata.Architecture, Digest: seedDigest, SizeBytes: int64(len(seed)), MediaType: definition.ComputerSeedMediaType}}}, Objects: []bundle.Object{bundle.Object(output.Artifact), {Digest: seedDigest, SizeBytes: int64(len(seed)), MediaType: definition.ComputerSeedMediaType}}}
	bundle.SortObjects(manifest.Objects)
	finalize := NewFinalizer(store, store, bundle.Admission{Runtime: runtime}, discardLogger())
	root := func(m bundle.Manifest) string {
		t.Helper()
		var err error
		m.Plan, err = bundle.PlanFromProgramMetadata(m.Program.Metadata)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := bundle.Canonical(m)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256sum.DigestBytes(raw)
		store.add(digest, bundle.MediaType, raw)
		return digest
	}
	// An admitted producer projection is still untrusted until the actual Program
	// is compared with it. Alter only metadata while keeping exact artifact bytes.
	bad := manifest
	bad.Program.Metadata = metadata.Clone()
	duration := int64(9876)
	bad.Program.Metadata.Definitions[0].Agent.MaxTurnDurationMs = &duration
	request, err := finalize.Prepare(t.Context(), f.principal(), f.scope(), root(bad), "mismatched-metadata")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = finalize.Finalize(t.Context(), f.pool, request, func(string) error { return nil }); err == nil {
		t.Fatal("unbound producer metadata registered")
	}
	var count int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM deployments`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid Program left a deployment: %d %v", count, err)
	}
	request, err = finalize.Prepare(t.Context(), f.principal(), f.scope(), root(manifest), "verified")
	if err != nil {
		t.Fatal(err)
	}
	verified := 0
	result, err := finalize.Finalize(t.Context(), f.pool, request, func(string) error { verified++; return nil })
	if err != nil || verified != 2 {
		t.Fatalf("finalize: %v verified=%d", err, verified)
	}
	var stored []byte
	if err = f.pool.QueryRow(t.Context(), `SELECT s.spec FROM computer_preparation_specs s JOIN computer_definitions d ON (d.environment_id,d.preparation_spec_id)=(s.environment_id,s.id) WHERE d.deployment_id=$1`, result.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	spec, err := artifact.ParseComputerPreparationSpec(stored)
	if err != nil || spec.Program != output.Artifact || spec.ComputerDefinitionID != "repo" {
		t.Fatalf("registered identity: %+v %v", spec, err)
	}
}

type physicalBundleStore struct {
	cas.UploadStore
	body    map[string][]byte
	objects map[string]cas.Object
}

func (s *physicalBundleStore) add(digest, media string, body []byte) {
	s.body[digest] = body
	s.objects[digest] = cas.Object{Digest: digest, SizeBytes: int64(len(body)), MediaType: media}
}
func (s *physicalBundleStore) Stat(_ context.Context, digest string) (cas.Object, error) {
	o, ok := s.objects[digest]
	if !ok {
		return cas.Object{}, errors.New("missing test object")
	}
	return o, nil
}
func (s *physicalBundleStore) Get(_ context.Context, digest string) (io.ReadCloser, error) {
	b, ok := s.body[digest]
	if !ok {
		return nil, errors.New("missing test bytes")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (s *physicalBundleStore) PromoteQuarantine(ctx context.Context, _ string, d cas.Descriptor) (cas.Object, error) {
	return s.Stat(ctx, d.Digest)
}

func physicalProgram(t *testing.T, metadata artifact.ProgramMetadata) []byte {
	t.Helper()
	encoder := os.Getenv("HELMR_SQUASHFS_ENCODER")
	if encoder == "" {
		t.Skip("HELMR_SQUASHFS_ENCODER is not set")
	}
	canonical := func(value any) []byte {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		raw, err = jsoncanon.Transform(raw)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	rawMetadata, err := artifact.CanonicalProgramMetadata(metadata)
	if err != nil {
		t.Fatal(err)
	}
	index, err := artifact.CanonicalDefinitionIndex(artifacttest.DefinitionIndex())
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"bun.lock": []byte("lockfileVersion = 1\n"), "helmr.config.ts": []byte("export default { dirs: ['tasks'] };\n"),
		"helmr/config.json":           []byte(`{"assets":[],"dirs":["tasks"],"external":[],"ignorePatterns":[]}`),
		"helmr/program-metadata.json": rawMetadata, "helmr/definition-index.json": index,
		"helmr/app/entry-0.mjs": []byte("export const build = {};\n"), "helmr/app/entry-1.mjs": []byte("export const chat = {};\n"),
		"package.json": []byte(`{"packageManager":"yarn@4.9.2"}`),
	}
	directories := []string{"helmr", "helmr/app", "node_modules", "tasks"}
	entries := []artifact.Entry{{Path: ".", Kind: artifact.EntryDirectory, Mode: 0755}}
	for _, dir := range directories {
		entries = append(entries, artifact.Entry{Path: dir, Kind: artifact.EntryDirectory, Mode: 0755})
	}
	for path, body := range files {
		entries = append(entries, artifact.Entry{Path: path, Kind: artifact.EntryRegular, Mode: 0644, SizeBytes: int64(len(body))})
	}
	payload, err := artifact.PayloadDigest(t.Context(), entries, func(_ context.Context, path string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(files[path])), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	files["helmr/program-manifest.json"] = canonical(artifact.ProgramManifest{FormatVersion: artifact.ProgramManifestFormatVersion, Config: artifact.ProgramPathDigest{Path: "helmr/config.json", Digest: metadata.ConfigResultDigest}, ProgramMetadataDigest: sha256sum.DigestBytes(rawMetadata), PayloadDigest: payload})
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	names := slices.Clone(directories)
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		body, regular := files[name]
		h := &tar.Header{Name: name, ModTime: time.Unix(0, 0), Format: tar.FormatPAX, Mode: 0755, Typeflag: tar.TypeDir}
		if regular {
			h.Mode = 0644
			h.Typeflag = tar.TypeReg
			h.Size = int64(len(body))
		}
		if err := writer.WriteHeader(h); err != nil {
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
	path := filepath.Join(t.TempDir(), "program.squashfs")
	cmd := exec.CommandContext(t.Context(), encoder, "-", path, "-tar", "-noappend", "-all-root", "-no-xattrs", "-no-exports", "-no-fragments", "-no-tailends", "-no-duplicates", "-no-hardlinks", "-no-progress", "-exit-on-error", "-processors", "2", "-mem", "256M", "-comp", "zstd", "-b", "131072", "-root-mode", "0755", "-mkfs-time", "0", "-all-time", "0")
	cmd.Env = []string{"LC_ALL=C", "TZ=UTC"}
	cmd.Stdin = bytes.NewReader(archive.Bytes())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("encode: %v %s", err, output)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
