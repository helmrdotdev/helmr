package controlplane

import (
	"fmt"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
	"testing"
)

type initialPublicationFixture struct {
	runtest.Fixture
	server       *Server
	worker       workerActor
	logicalBytes int64
	runtime      pgtype.UUID
	run          runtest.RunLease
}

func newInitialPublicationFixture(t *testing.T) initialPublicationFixture {
	t.Helper()
	f, run, runtime := prepareReservedRuntimeFailure(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE deployment_definitions SET manifest=$2::jsonb WHERE id=$1`, f.WorkspaceDefinitionID, fmt.Sprintf(`{"image":{"artifactDigest":%q,"mediaType":"application/octet-stream"},"resources":{"milliCpu":1000,"memoryMiB":1024}}`, dbtest.Digest("run-lease-image")))
	diskBytes := int64(compute.WorkspaceGuestEphemeralDiskMiB) * 1048576
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET reserved_guest_ephemeral_disk_bytes=$2 WHERE id=$1`, runtime, diskBytes)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_versions SET status='initializing',artifact_id=NULL,content_digest=NULL,size_bytes=0,published_at=NULL WHERE workspace_id=(SELECT workspace_id FROM runtime_instances WHERE id=$1)`, runtime)
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return initialPublicationFixture{Fixture: f, run: run, runtime: runtime, server: &Server{db: db.New(f.Pool), tx: f.Pool, cas: store},
		worker: workerActor{WorkerInstanceID: f.WorkerID, WorkerGroupID: runtest.WorkerGroupID, WorkerEpoch: 1}, logicalBytes: diskBytes}
}

func TestComputerPreparationSourceTracksPublishedRoot(t *testing.T) {
	f, fence, input := generationPublicationFixture(t)
	seed := initializingComputerSourceRow(t)
	// Bind a valid admitted deployment to this reserved runtime. The existing
	// publication fixture's opaque candidate isolates the database protocol;
	// disk encoding/authentication is exercised by the computer package.
	dbtest.MustExec(t, t.Context(), f.Pool, `WITH lifetime AS (INSERT INTO cas_blobs (digest, size_bytes) VALUES ($2, $3) ON CONFLICT DO NOTHING) INSERT INTO cas_objects (org_id,digest,size_bytes,media_type) VALUES ($1,$2,$3,$4)`,
		f.OrgID, seed.WorkspaceImageDigest, seed.WorkspaceImageSizeBytes, seed.WorkspaceImageMediaType)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE artifacts SET digest=$2,size_bytes=$3,media_type=$4
 WHERE id=(SELECT artifact_id FROM deployment_definitions WHERE id=$1)`,
		f.WorkspaceDefinitionID, seed.WorkspaceImageDigest, seed.WorkspaceImageSizeBytes, seed.WorkspaceImageMediaType)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE deployment_definitions SET manifest=$2 WHERE id=$1`, f.WorkspaceDefinitionID, seed.SandboxManifest)
	read := func() workerapi.RuntimeComputerSource {
		t.Helper()
		rows, err := f.server.db.ListRuntimeReconcileTargets(t.Context(), db.ListRuntimeReconcileTargetsParams{
			WorkerGroupID: pgvalue.UUID(f.worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(f.worker.WorkerInstanceID),
			WorkerEpoch: f.worker.WorkerEpoch, ObservationFreshnessSeconds: workerapi.WorkerObservationFreshnessSeconds, RowLimit: 64,
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
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE deployment_definitions SET manifest=jsonb_set(manifest,'{image,config}', '{"User":"changed"}') WHERE id=$1`, f.WorkspaceDefinitionID)
	if source := read(); source.Config.User != "root" || source.Config.WorkingDir != "/workspace" || source.Root == nil {
		t.Fatalf("continuation depended on receipt or new deployment: %+v", source)
	}
}
