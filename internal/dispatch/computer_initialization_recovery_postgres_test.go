package dispatch

import (
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"testing"
)

func prepareInitialGeneration(t *testing.T, f runPlacementFixture) (pgtype.UUID, pgtype.UUID) {
	t.Helper()
	root := workspaceHeadVersion(t, f)
	dbtest.MustExec(t, f.ctx, f.pool, `UPDATE computer_versions SET status='initializing',publisher_runtime_instance_id=NULL,publisher_desired_version=NULL,publication_request_fingerprint=NULL,root_pack_digest=NULL,logical_bytes=0,published_at=NULL WHERE id=$1`, root)
	placement, err := f.authority.PlaceReadyRun(f.ctx, ReadyRunCandidate{OrgID: pgvalue.UUID(f.orgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	return placement.RuntimeInstanceID, root
}

func TestInitialGenerationRecoveryRetainsObjectsUntilPhysicalReclamation(t *testing.T) {
	f := newRunPlacementFixture(t)
	runtimeID, root := prepareInitialGeneration(t, f)
	// Reuse the fixture generation as a registered candidate. This proves DB
	// retention across revocation, not remote verification or physical shutdown.
	dbtest.MustExec(t, f.ctx, f.pool, `INSERT INTO runtime_computer_object_pins(runtime_instance_id,publication_key,runtime_desired_version,environment_id,computer_id,digest)
 SELECT r.id,decode(repeat('a1',32),'hex'),r.desired_version,r.environment_id,r.workspace_id,v.root_pack_digest FROM runtime_instances r JOIN computer_version_roots v ON v.version_id=$2 WHERE r.id=$1`, runtimeID, root)
	dbtest.MustExec(t, f.ctx, f.pool, `UPDATE runtime_instances SET preparation_expires_at=now()-interval '1 second' WHERE id=$1`, runtimeID)
	n, err := f.authority.RecoverExpiredRuntimeReservations(f.ctx, 10)
	if err != nil || n != 1 {
		t.Fatalf("expiry: %d %v", n, err)
	}
	runtime, err := runtimeDeadlineState(f, runtimeID)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.ReclaimedAt.Valid || runtime.DesiredState != "closed" {
		t.Fatalf("incorrect physical cleanup claim: %+v", runtime)
	}
	if n, err := db.New(f.pool).ReleaseReclaimedComputerObjects(f.ctx, 10); err != nil || n != 0 {
		t.Fatalf("revocation released pins: %d %v", n, err)
	}
	var pins int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM runtime_computer_object_pins WHERE runtime_instance_id=$1`, runtimeID).Scan(&pins); err != nil || pins != 1 {
		t.Fatalf("lost candidate pin: %d %v", pins, err)
	}
	if n, err := f.authority.RecoverExpiredRuntimeReservations(f.ctx, 10); err != nil || n != 0 {
		t.Fatalf("replay: %d %v", n, err)
	}
}
