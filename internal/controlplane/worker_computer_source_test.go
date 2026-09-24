package controlplane

import (
	"bytes"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func initializingComputerSourceRow(t *testing.T) db.ListRuntimeReconcileTargetsRow {
	t.Helper()
	manifest, err := json.Marshal(deployment.SandboxManifest{Resources: deployment.ResourcesManifest{MilliCPU: 1000, MemoryMiB: 1024}, Image: deployment.SandboxImageManifest{
		Profile: computer.SeedProfile, ArtifactDigest: validDigest('a'), MediaType: computer.SeedMediaType,
		Config: oci.RuntimeConfig{User: "1000", Env: []string{"HELLO=world"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return db.ListRuntimeReconcileTargetsRow{
		BaseWorkspaceVersionID:    pgvalue.UUID(uuid.NewV7()),
		ComputerVersionStatus:     pgvalue.Text("initializing"),
		WorkspaceLogicalSizeBytes: pgtype.Int8{Valid: true},
		WorkspaceArchitecture:     "x86_64", ReservedGuestEphemeralDiskBytes: computer.SeedCapacity,
		WorkspaceImageDigest: validDigest('a'), WorkspaceImageSizeBytes: 1024, WorkspaceImageMediaType: computer.SeedMediaType,
		SandboxManifestVersion: deployment.DeploymentPlanFormatVersion, SandboxManifest: manifest,
	}
}

func committedComputerSourceRow(t *testing.T) db.ListRuntimeReconcileTargetsRow {
	r := initializingComputerSourceRow(t)
	r.ComputerVersionStatus = pgvalue.Text("committed")
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
	r.WorkspaceContentDigest = pgvalue.Text(root.Pack.Digest)
	r.WorkspaceLogicalSizeBytes.Int64 = computer.SeedCapacity
	r.ComputerInitialConfig = []byte(`{"User":"original","Env":["ORIGINAL=yes"]}`)
	return r
}

func TestRuntimeComputerSourceSeparatesInitializationAndContinuation(t *testing.T) {
	initial := initializingComputerSourceRow(t)
	source, err := projectRuntimeComputerSource(initial)
	if err != nil {
		t.Fatal(err)
	}
	if source.Seed == nil || source.Disk != nil || source.Config.User != "1000" || source.VersionID != pgvalue.UUIDString(initial.BaseWorkspaceVersionID) {
		t.Fatalf("initial source: %+v", source)
	}
	for _, status := range []string{"committed", "private"} {
		r := committedComputerSourceRow(t)
		r.ComputerVersionStatus = pgvalue.Text(status)
		// Deployment changes cannot reseed a Computer or replace its initial config.
		r.SandboxManifest = []byte(`{"invalid":"unused"}`)
		r.WorkspaceImageDigest = ""
		source, err := projectRuntimeComputerSource(r)
		if err != nil {
			t.Fatal(err)
		}
		if source.Seed != nil || source.Disk != nil || source.Config.User != "original" {
			t.Fatalf("continuation source: %+v", source)
		}
	}
}

func TestRuntimeComputerSourceRejectsMissingOrConflictingAuthority(t *testing.T) {
	for _, test := range []struct {
		name      string
		committed bool
		change    func(*db.ListRuntimeReconcileTargetsRow)
	}{
		{"missing-version", false, func(r *db.ListRuntimeReconcileTargetsRow) { r.BaseWorkspaceVersionID.Valid = false }},
		{"discarded", true, func(r *db.ListRuntimeReconcileTargetsRow) { r.ComputerVersionStatus = pgvalue.Text("discarded") }},
		{"capacity", false, func(r *db.ListRuntimeReconcileTargetsRow) { r.ReservedGuestEphemeralDiskBytes /= 2 }},
		{"architecture", false, func(r *db.ListRuntimeReconcileTargetsRow) { r.WorkspaceArchitecture = "arm64" }},
		{"seed-conflict", false, func(r *db.ListRuntimeReconcileTargetsRow) { r.WorkspaceImageDigest = validDigest('c') }},
		{"seed-format", false, func(r *db.ListRuntimeReconcileTargetsRow) { r.WorkspaceImageMediaType = "application/oci" }},
		{"seed-profile", false, func(r *db.ListRuntimeReconcileTargetsRow) {
			r.SandboxManifest = []byte(`{"image":{"profile":"other"}}`)
		}},
		{"initial-restore", false, func(r *db.ListRuntimeReconcileTargetsRow) { r.RestoreCheckpointID = pgvalue.UUID(uuid.NewV7()) }},
		{"initial-config", false, func(r *db.ListRuntimeReconcileTargetsRow) { r.ComputerInitialConfig = []byte(`{}`) }},
		{"initial-disk", false, func(r *db.ListRuntimeReconcileTargetsRow) { r.WorkspaceArtifactDigest = validDigest('b') }},
		{"missing-generation", true, func(r *db.ListRuntimeReconcileTargetsRow) { r.ComputerGenerationLocator = nil }},
		{"disk-conflict", true, func(r *db.ListRuntimeReconcileTargetsRow) { r.WorkspaceContentDigest = pgvalue.Text(validDigest('c')) }},
		{"generation-format", true, func(r *db.ListRuntimeReconcileTargetsRow) { r.ComputerGenerationLocator = []byte(`{}`) }},
		{"disk-capacity", true, func(r *db.ListRuntimeReconcileTargetsRow) { r.WorkspaceLogicalSizeBytes.Int64 /= 2 }},
		{"missing-config", true, func(r *db.ListRuntimeReconcileTargetsRow) { r.ComputerInitialConfig = nil }},
		{"null-config", true, func(r *db.ListRuntimeReconcileTargetsRow) { r.ComputerInitialConfig = []byte(`null`) }},
		{"unknown-config", true, func(r *db.ListRuntimeReconcileTargetsRow) { r.ComputerInitialConfig = []byte(`{"unexpected":true}`) }},
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
