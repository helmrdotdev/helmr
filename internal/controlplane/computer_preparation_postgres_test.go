package controlplane

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

type initialPublicationFixture struct {
	runtest.Fixture
	server       *Server
	worker       workerActor
	logicalBytes int64
	runtime      pgtype.UUID
}

func newInitialPublicationFixture(t *testing.T) initialPublicationFixture {
	t.Helper()
	f := runtest.New(t)
	q := db.New(f.Pool)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.EnvironmentID, f.DeploymentID)
	c, err := q.CreateComputerFromCurrentDeployment(t.Context(), db.CreateComputerFromCurrentDeploymentParams{
		ID: pgvalue.NewUUIDv7(), InitialVersionID: pgvalue.NewUUIDv7(),
		OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID),
		DeploymentDefinitionID: pgvalue.UUID(f.ComputerDefinitionID), SandboxDeclaredID: "test-computer",
	})
	if err != nil {
		t.Fatal(err)
	}
	diskBytes := int64(compute.ComputerGuestEphemeralDiskMiB) * 1048576
	instance, err := q.AllocateComputerInstance(t.Context(), db.AllocateComputerInstanceParams{
		ID: pgvalue.NewUUIDv7(), EnvironmentID: c.EnvironmentID, ComputerID: c.ID, ComputerSpecID: c.ComputerSpecID,
		WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1,
		VMPlatformID: f.VMPlatformID, VMVCPUCount: 1, CPUConfigDigest: f.CPUConfigDigest,
		ReservedCPUMillis: 1000, ReservedMemoryBytes: 1073741824, ReservedGuestEphemeralDiskBytes: diskBytes, ReservedExecutionSlots: 1,
		PreparationSeconds: 300, WriterTtlSeconds: 600, WriterGeneration: 1, WriterTokenHash: make([]byte, 32), Reason: "computer_preparation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.ChargeComputerPreparation(t.Context(), db.ChargeComputerPreparationParams{ComputerID: c.ID, InstanceID: instance.ID}); err != nil {
		t.Fatal(err)
	}
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return initialPublicationFixture{Fixture: f, runtime: instance.ID, server: &Server{db: db.New(f.Pool), tx: f.Pool, cas: store},
		worker: workerActor{WorkerHostID: f.WorkerID, WorkerGroupID: runtest.WorkerGroupID, WorkerEpoch: 1}, logicalBytes: diskBytes}
}

func TestComputerPreparationSourceTracksPublishedRoot(t *testing.T) {
	f, fence, input := generationPublicationFixture(t)
	seed := initializingComputerSourceRow(t)
	// Bind a valid admitted deployment to this reserved runtime. The existing
	// publication fixture's opaque candidate isolates the database protocol;
	// disk encoding/authentication is exercised by the computer package.
	dbtest.MustExec(t, t.Context(), f.Pool, `WITH lifetime AS (INSERT INTO cas_blobs (digest, size_bytes) VALUES ($2, $3) ON CONFLICT DO NOTHING) INSERT INTO cas_objects (org_id,digest,size_bytes,media_type) VALUES ($1,$2,$3,$4)`,
		f.OrgID, seed.ComputerImageDigest, seed.ComputerImageSizeBytes, seed.ComputerImageMediaType)
	seedID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO artifacts(id,org_id,project_id,environment_id,kind,digest,size_bytes,media_type)
 VALUES($1,$2,$3,$4,'computer_image',$5,$6,$7)`, seedID, f.OrgID, f.ProjectID, f.EnvironmentID, seed.ComputerImageDigest, seed.ComputerImageSizeBytes, seed.ComputerImageMediaType)
	spec, err := f.server.db.RegisterComputerSpec(t.Context(), db.RegisterComputerSpecParams{
		ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(f.EnvironmentID),
		Config: seed.ComputerConfig, Digest: seed.ComputerSpecDigest, SeedArtifactID: pgvalue.UUID(seedID),
		SeedDigest: seed.ComputerImageDigest, SeedSizeBytes: seed.ComputerImageSizeBytes, SeedMediaType: seed.ComputerImageMediaType,
	})
	if err != nil {
		t.Fatal(err)
	}
	specID := spec.ID
	dbtest.MustExec(t, t.Context(), f.Pool, `WITH target AS (SELECT computer_id FROM computer_instances WHERE id=$1), updated AS (UPDATE computers SET computer_spec_id=$2 WHERE id=(SELECT computer_id FROM target)) UPDATE computer_instances SET computer_spec_id=$2 WHERE computer_id=(SELECT computer_id FROM target)`, f.runtime, specID)
	read := func() workerapi.RuntimeComputerSource {
		t.Helper()
		rows, err := f.server.db.ListComputerInstanceReconcileTargets(t.Context(), db.ListComputerInstanceReconcileTargetsParams{
			WorkerGroupID: pgvalue.UUID(f.worker.WorkerGroupID), WorkerHostID: pgvalue.UUID(f.worker.WorkerHostID),
			WorkerEpoch: f.worker.WorkerEpoch, RowLimit: 64,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("preparation rows: %d", len(rows))
		}
		source, err := projectRuntimeComputerSource(rows[0])
		if err != nil {
			t.Fatal(err)
		}
		return source
	}
	initial := read()
	if initial.Seed == nil || initial.Root != nil || initial.Config.User != "1000" {
		t.Fatalf("initial: %+v", initial)
	}
	published, err := f.server.publishInitialComputerGeneration(t.Context(), fence, input)
	if err != nil {
		t.Fatal(err)
	}
	continued := read()
	if continued.Seed != nil || continued.Root == nil || continued.Config.User != "root" || continued.VersionID != initial.VersionID || continued.VersionID != pgvalue.UUIDString(published.VersionID) {
		t.Fatalf("published: %+v", continued)
	}
	// A later deployment cannot replace the Computer's initial configuration.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE deployment_definitions SET manifest=jsonb_set(manifest,'{image,config}', '{"User":"changed"}') WHERE id=$1`, f.ComputerDefinitionID)
	if source := read(); source.Config.User != "root" || source.Config.WorkingDir != "/workspace" || source.Root == nil {
		t.Fatalf("continuation depended on receipt or new deployment: %+v", source)
	}
}
