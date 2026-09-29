package controlplane

import (
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestComputerPreparationWithoutProgram(t *testing.T) {
	row := initializingComputerSourceRow(t)
	row.WriterGeneration = 7
	row.ComputerID = pgvalue.UUID(uuid.NewV7())
	row.ComputerSpecID = pgvalue.UUID(uuid.NewV7())
	source, err := projectComputerInstancePreparation(t.Context(), nil, row)
	if err != nil {
		t.Fatal(err)
	}
	if source.WriterGeneration != 7 || source.Program != nil || source.Computer == nil || source.Computer.Seed == nil || source.ComputerID != pgvalue.UUIDString(row.ComputerID) {
		t.Fatalf("standalone preparation: %+v", source)
	}
}

func TestComputerPreparationPinsProgram(t *testing.T) {
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := store.Put(t.Context(), artifact.RuntimeArtifactMediaType, strings.NewReader("runtime"))
	if err != nil {
		t.Fatal(err)
	}
	row := committedComputerSourceRow(t)
	row.SourceDiskVersionID = row.PreparationDiskVersionID
	row.ProgramDeploymentID = pgvalue.UUID(uuid.NewV7())
	row.ProgramRuntimeDigest = pgvalue.Text(runtime.Digest)
	row.ProgramArtifactDigest = pgvalue.Text(dbtest.Digest("program"))
	row.ProgramArtifactSizeBytes = pgtype.Int8{Int64: 42, Valid: true}
	row.ProgramArtifactMediaType = pgvalue.Text("application/vnd.helmr.deployment-program.v0+squashfs")
	row.ProgramIndexDigest = dbtest.Hash("index")
	source, err := projectComputerInstancePreparation(t.Context(), store, row)
	if err != nil {
		t.Fatal(err)
	}
	if source.Program == nil || source.Program.DeploymentID != pgvalue.UUIDString(row.ProgramDeploymentID) || source.Program.Runtime.Digest != runtime.Digest || source.Computer.Config.User != "original" {
		t.Fatalf("wrong pinned Program/config: %+v", source)
	}
	for _, change := range []func(*db.ListComputerInstanceReconcileTargetsRow){
		func(r *db.ListComputerInstanceReconcileTargetsRow) { r.ProgramArtifactDigest.Valid = false },
		func(r *db.ListComputerInstanceReconcileTargetsRow) { r.ProgramIndexDigest = nil },
		func(r *db.ListComputerInstanceReconcileTargetsRow) {
			r.ProgramRuntimeDigest = pgvalue.Text(dbtest.Digest("absent"))
		},
	} {
		invalid := row
		change(&invalid)
		if _, err := projectComputerInstancePreparation(t.Context(), store, invalid); err == nil {
			t.Fatal("incomplete Program preparation accepted")
		}
	}
}

func TestComputerCleanupMetadataDoesNotReadDiskOrProgram(t *testing.T) {
	row := initializingComputerSourceRow(t)
	row.ComputerID = pgvalue.UUID(uuid.NewV7())
	row.ComputerSpecID = pgvalue.UUID(uuid.NewV7())
	row.ComputerGenerationLocator = []byte(`invalid`)
	row.ProgramDeploymentID = pgvalue.UUID(uuid.NewV7())
	row.VMPlatformID = "platform"
	row.VMVCPUCount = 3
	row.CPUConfigDigest = "cpu-shape"
	row.VmContract = "contract"
	source := computerInstanceSourceMetadata(row)
	if source.Computer != nil || source.Program != nil || source.VMPlatformID != row.VMPlatformID || source.VMVCPUCount != 3 || source.VMRuntimeContract != row.VmContract {
		t.Fatalf("cleanup lost physical identity: %+v", source)
	}
}

func TestComputerPreparationRestoresWithoutProgram(t *testing.T) {
	row := committedComputerSourceRow(t)
	row.ComputerDiskVersionStatus = pgvalue.Text("private")
	row.SourceDiskVersionID = row.PreparationDiskVersionID
	row.SourceCheckpointID = pgvalue.UUID(uuid.NewV7())
	source, err := projectComputerInstancePreparation(t.Context(), nil, row)
	if err != nil || source.Program != nil || source.Computer == nil || source.Computer.Root == nil {
		t.Fatalf("idle preparation checkpoint: %+v %v", source, err)
	}
}
