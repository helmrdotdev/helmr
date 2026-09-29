package controlplane

import (
	"bytes"
	"encoding/json"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func initializingComputerSourceRow(t *testing.T) db.ListComputerInstanceReconcileTargetsRow {
	t.Helper()
	config, err := json.Marshal(definition.ComputerConfig{
		Architecture: definition.ArchitectureX8664, RuntimeContract: definition.RuntimeContract,
		Profile:   computer.SeedProfile,
		Resources: definition.ResourcesManifest{MilliCPU: 1000, MemoryMiB: 1024},
		Image:     oci.RuntimeConfig{User: "1000", Env: []string{"HELLO=world"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := definition.ParseComputerSpec(config, cas.Descriptor{Digest: dbtest.Digest("seed"), SizeBytes: 1024, MediaType: computer.SeedMediaType})
	if err != nil {
		t.Fatal(err)
	}
	return db.ListComputerInstanceReconcileTargetsRow{
		PreparationDiskVersionID:  pgvalue.UUID(uuid.NewV7()),
		ComputerDiskVersionStatus: pgvalue.Text("initializing"),
		ComputerLogicalSizeBytes:  pgtype.Int8{Valid: true},
		ComputerArchitecture:      "x86_64", ReservedGuestEphemeralDiskBytes: computer.SeedCapacity,
		ComputerImageDigest: dbtest.Digest("seed"), ComputerImageSizeBytes: 1024, ComputerImageMediaType: computer.SeedMediaType,
		ComputerConfig: spec.Config, ComputerSpecDigest: spec.Digest[:],
	}
}

func committedComputerSourceRow(t *testing.T) db.ListComputerInstanceReconcileTargetsRow {
	r := initializingComputerSourceRow(t)
	r.ComputerDiskVersionStatus = pgvalue.Text("committed")
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := uuid.NewV7().String()
	writer := blockformat.Writer{Source: store, Sink: store, Scope: "fixture", ActiveKey: key, Keys: map[string][]byte{key: bytes.Repeat([]byte{1}, 32)}, PackLimit: blockformat.MinPackLimit}
	locator, err := writer.Empty(t.Context(), computer.SeedCapacity, 64)
	if err != nil {
		t.Fatal(err)
	}
	root, err := computer.NewGenerationRoot(locator, computer.SeedCapacity)
	if err != nil {
		t.Fatal(err)
	}
	r.ComputerGenerationLocator, err = json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	r.ComputerContentDigest = pgvalue.Text(root.Pack.Digest)
	r.ComputerLogicalSizeBytes.Int64 = computer.SeedCapacity
	r.ComputerInitialConfig = []byte(`{"User":"original","Env":["ORIGINAL=yes"]}`)
	return r
}

func TestRuntimeComputerSourceSeparatesInitializationAndContinuation(t *testing.T) {
	initial := initializingComputerSourceRow(t)
	source, err := projectRuntimeComputerSource(initial)
	if err != nil {
		t.Fatal(err)
	}
	if source.Seed == nil || source.Root != nil || source.Config.User != "1000" || source.VersionID != pgvalue.UUIDString(initial.PreparationDiskVersionID) {
		t.Fatalf("initial source: %+v", source)
	}
	for _, status := range []string{"committed", "private"} {
		r := committedComputerSourceRow(t)
		r.ComputerDiskVersionStatus = pgvalue.Text(status)
		// Deployment changes cannot reseed a Computer or replace its initial config.
		r.ComputerConfig = []byte(`{"invalid":"unused"}`)
		r.ComputerImageDigest = ""
		source, err := projectRuntimeComputerSource(r)
		if err != nil {
			t.Fatal(err)
		}
		if source.Seed != nil || source.Root == nil || source.Config.User != "original" {
			t.Fatalf("continuation source: %+v", source)
		}
	}
}

func TestRuntimeComputerSourceRejectsMissingOrConflictingAuthority(t *testing.T) {
	for _, test := range []struct {
		name      string
		committed bool
		change    func(*db.ListComputerInstanceReconcileTargetsRow)
	}{
		{"missing-version", false, func(r *db.ListComputerInstanceReconcileTargetsRow) { r.PreparationDiskVersionID.Valid = false }},
		{"source-mismatch", true, func(r *db.ListComputerInstanceReconcileTargetsRow) {
			r.SourceDiskVersionID = pgvalue.UUID(uuid.NewV7())
		}},
		{"restore-without-source", true, func(r *db.ListComputerInstanceReconcileTargetsRow) { r.SourceCheckpointID = pgvalue.UUID(uuid.NewV7()) }},
		{"discarded", true, func(r *db.ListComputerInstanceReconcileTargetsRow) {
			r.ComputerDiskVersionStatus = pgvalue.Text("discarded")
		}},
		{"capacity", false, func(r *db.ListComputerInstanceReconcileTargetsRow) { r.ReservedGuestEphemeralDiskBytes /= 2 }},
		{"architecture", false, func(r *db.ListComputerInstanceReconcileTargetsRow) { r.ComputerArchitecture = "arm64" }},
		{"seed-conflict", false, func(r *db.ListComputerInstanceReconcileTargetsRow) { r.ComputerImageDigest = dbtest.Digest("other") }},
		{"seed-format", false, func(r *db.ListComputerInstanceReconcileTargetsRow) { r.ComputerImageMediaType = "application/oci" }},
		{"seed-profile", false, func(r *db.ListComputerInstanceReconcileTargetsRow) {
			r.ComputerConfig = []byte(`{"image":{"profile":"other"}}`)
		}},
		{"initial-restore", false, func(r *db.ListComputerInstanceReconcileTargetsRow) { r.SourceCheckpointID = pgvalue.UUID(uuid.NewV7()) }},
		{"initial-config", false, func(r *db.ListComputerInstanceReconcileTargetsRow) { r.ComputerInitialConfig = []byte(`{}`) }},
		{"initial-disk", false, func(r *db.ListComputerInstanceReconcileTargetsRow) { r.ComputerGenerationLocator = []byte(`{}`) }},
		{"missing-generation", true, func(r *db.ListComputerInstanceReconcileTargetsRow) { r.ComputerGenerationLocator = nil }},
		{"disk-conflict", true, func(r *db.ListComputerInstanceReconcileTargetsRow) {
			r.ComputerContentDigest = pgvalue.Text(dbtest.Digest("other"))
		}},
		{"generation-format", true, func(r *db.ListComputerInstanceReconcileTargetsRow) { r.ComputerGenerationLocator = []byte(`{}`) }},
		{"disk-capacity", true, func(r *db.ListComputerInstanceReconcileTargetsRow) { r.ComputerLogicalSizeBytes.Int64 /= 2 }},
		{"missing-config", true, func(r *db.ListComputerInstanceReconcileTargetsRow) { r.ComputerInitialConfig = nil }},
		{"null-config", true, func(r *db.ListComputerInstanceReconcileTargetsRow) { r.ComputerInitialConfig = []byte(`null`) }},
		{"unknown-config", true, func(r *db.ListComputerInstanceReconcileTargetsRow) {
			r.ComputerInitialConfig = []byte(`{"unexpected":true}`)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := initializingComputerSourceRow(t)
			if test.committed {
				r = committedComputerSourceRow(t)
			}
			test.change(&r)
			if _, err := projectRuntimeComputerSource(r); err == nil {
				t.Fatal("invalid authority accepted")
			}
		})
	}
}
