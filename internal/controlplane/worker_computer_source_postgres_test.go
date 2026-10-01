package controlplane

import (
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestInstanceSourceDiscoveryUsesExactDisk(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	var instance, computerID, version pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT i.id,i.computer_id,i.source_disk_version_id FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id WHERE l.id=$1`, work.LeaseID).Scan(&instance, &computerID, &version); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_version=desired_version+1,reserved_guest_ephemeral_disk_bytes=$2 WHERE id=$1`, instance, disk.SeedCapacity)
	read := func() db.ListComputerInstanceReconcileTargetsRow {
		t.Helper()
		rows, err := db.New(f.Pool).ListComputerInstanceReconcileTargets(t.Context(), db.ListComputerInstanceReconcileTargetsParams{WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerEpoch: 1, RowLimit: 64})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.ID == instance {
				return row
			}
		}
		t.Fatal("instance missing from discovery")
		return db.ListComputerInstanceReconcileTargetsRow{}
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET initial_config='{"User":"original"}' WHERE id=$1`, computerID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_disk_versions v SET root_pack_digest=root.locator->'pack'->>'digest',logical_bytes=$2 FROM computer_disk_version_roots root WHERE v.id=$1 AND root.version_id=v.id`, version, disk.SeedCapacity)
	platform, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := platform.Put(t.Context(), artifact.RuntimeArtifactMediaType, strings.NewReader("managed runtime"))
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE deployments SET runtime_artifact_digest=$2 WHERE id=$1`, f.DeploymentID, runtime.Digest)
	pinned := read()
	if !version.Valid || pinned.PreparationDiskVersionID != version || pinned.ComputerDiskVersionStatus.String != "committed" {
		t.Fatalf("pinned source not selected: %+v", pinned.PreparationDiskVersionID)
	}
	projected, err := projectInstanceComputerSource(pinned)
	if err != nil || projected.Root == nil || projected.Seed != nil {
		t.Fatalf("discovered continuation projection: %+v %v", projected, err)
	}
	preparation, err := projectComputerInstancePreparation(t.Context(), platform, pinned)
	if err != nil || preparation.Program == nil || preparation.Program.DeploymentID != f.DeploymentID.String() || preparation.Program.Runtime.Digest != runtime.Digest || preparation.Computer.Config.User != "original" {
		t.Fatalf("discovered Program preparation: %+v %v", preparation, err)
	}
	// The allocated source remains authoritative even if the Computer head changes.
	later := pgvalue.UUID(uuid.NewV7())
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_disk_versions (id,environment_id,computer_id,parent_version_id,root_pack_digest,logical_bytes,status,writer_generation,published_at,source_computer_instance_id) SELECT $2,environment_id,computer_id,id,root_pack_digest,logical_bytes,'committed',(SELECT writer_generation FROM computer_instances WHERE id=$3),clock_timestamp(),$3 FROM computer_disk_versions WHERE id=$1`, version, later, instance)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET head_disk_version_id=$2 WHERE id=$1`, computerID, later)
	if row := read(); row.PreparationDiskVersionID != version {
		t.Fatal("discovery replaced the pinned source")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET head_disk_version_id=$2 WHERE id=$1`, computerID, version)
	// Initial allocation has no retained source; its seed publication targets the
	// Computer's initializing head, not a reservation owned by a member Run.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET source_disk_version_id=NULL WHERE id=$1`, instance)
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM computer_disk_version_roots WHERE version_id=$1`, version)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_disk_versions SET status='initializing',publisher_computer_instance_id=NULL,publisher_desired_version=NULL,publication_request_fingerprint=NULL,root_pack_digest=NULL,logical_bytes=0,published_at=NULL WHERE id=$1`, version)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET initial_config=NULL WHERE id=$1`, computerID)
	initial := read()
	if initial.SourceDiskVersionID.Valid || initial.PreparationDiskVersionID != version || initial.ComputerDiskVersionStatus.String != "initializing" || len(initial.ComputerVersionLocator) != 0 {
		t.Fatal("initial source did not resolve the initializing head")
	}
	projected, err = projectInstanceComputerSource(initial)
	if err != nil || projected.Seed == nil || projected.Root != nil {
		t.Fatalf("discovered seed projection: %+v %v", projected, err)
	}
	preparation, err = projectComputerInstancePreparation(t.Context(), platform, initial)
	if err != nil || preparation.Program == nil || preparation.Computer == nil || preparation.Computer.Seed == nil {
		t.Fatalf("discovered initial preparation: %+v %v", preparation, err)
	}
}
