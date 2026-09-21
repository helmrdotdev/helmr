package dispatch

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

// These tests exercise revocation and retained upload ownership. The fixture's
// tree version is not evidence of initial disk publication or writable VM boot.
func registerPreparationCandidate(t *testing.T, f runPlacementFixture) db.ComputerInitialization {
	t.Helper()
	placement, err := f.authority.PlaceReadyRun(f.ctx, ReadyRunCandidate{
		OrgID: pgvalue.UUID(f.orgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := db.RegisterComputerInitializationParams{
		ID: pgvalue.UUID(uuid.NewV7()), RuntimeInstanceID: placement.RuntimeInstanceID,
		Digest: dbtest.Digest(uuid.NewV7().String()), SizeBytes: 1024, LogicalBytes: 4096,
		MediaType: computer.DiskMediaType, InitialConfig: []byte(`{"User":"root"}`),
	}
	if err := f.pool.QueryRow(f.ctx, `
SELECT r.environment_id, r.workspace_id, r.reserved_workspace_version_id,
       r.desired_version, w.ownership_generation, w.writer_generation
  FROM runtime_instances r JOIN workspaces w ON w.id=r.workspace_id WHERE r.id=$1`,
		placement.RuntimeInstanceID).Scan(&p.EnvironmentID, &p.ComputerID, &p.VersionID,
		&p.RuntimeDesiredVersion, &p.OwnershipGeneration, &p.WriterGeneration); err != nil {
		t.Fatal(err)
	}
	row, err := db.New(f.pool).RegisterComputerInitialization(f.ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func readPreparationCandidate(t *testing.T, f runPlacementFixture, original db.ComputerInitialization) db.ComputerInitialization {
	t.Helper()
	row, err := db.New(f.pool).GetComputerInitialization(f.ctx, db.GetComputerInitializationParams{
		EnvironmentID: original.EnvironmentID, ComputerID: original.ComputerID, RuntimeInstanceID: original.RuntimeInstanceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestComputerInitializationRecoveryAfterRuntimeExpiry(t *testing.T) {
	f := newRunPlacementFixture(t)
	original := registerPreparationCandidate(t, f)
	dbtest.MustExec(t, f.ctx, f.pool, `UPDATE runtime_instances SET preparation_expires_at=now()-interval '1 second' WHERE id=$1`, original.RuntimeInstanceID)
	n, err := f.authority.RecoverExpiredRuntimeReservations(f.ctx, 10)
	if err != nil || n != 1 {
		t.Fatalf("expiry: n=%d, %v", n, err)
	}
	abandoned := readPreparationCandidate(t, f, original)
	if abandoned.Status != "abandoned" || !abandoned.AbandonedAt.Valid || abandoned.ArtifactID.Valid ||
		abandoned.Digest != original.Digest || abandoned.VersionID != original.VersionID {
		t.Fatalf("lost cleanup ownership: %+v", abandoned)
	}
	runtime, err := runtimeDeadlineState(f, original.RuntimeInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.ReclaimedAt.Valid || runtime.DesiredState != "closed" {
		t.Fatalf("incorrect physical cleanup claim: %+v", runtime)
	}
	n, err = f.authority.RecoverExpiredRuntimeReservations(f.ctx, 10)
	if err != nil || n != 0 {
		t.Fatalf("replay: n=%d, %v", n, err)
	}
	replay := readPreparationCandidate(t, f, original)
	if replay.AbandonedAt != abandoned.AbandonedAt {
		t.Fatal("abandonment receipt changed on replay")
	}
}

func TestComputerInitializationRecoveryWithoutExpiredReservation(t *testing.T) {
	for _, test := range []struct {
		name, update string
		abandoned    bool
	}{
		{"live", "", false},
		{"new desired version", "desired_version=desired_version+1", true},
		{"close requested", "desired_state='closed',desired_version=desired_version+1", true},
		{"cleared reservation", "reserved_run_id=NULL,reserved_attempt_number=NULL,reserved_workspace_version_id=NULL", true},
		{"failed", "observed_state='failed',terminal_at=clock_timestamp(),terminal_reason_code='prepare_failed',reserved_run_id=NULL,reserved_attempt_number=NULL,reserved_workspace_version_id=NULL", true},
		{"lost", "observed_state='lost',terminal_at=clock_timestamp(),terminal_reason_code='worker_lost',reserved_run_id=NULL,reserved_attempt_number=NULL,reserved_workspace_version_id=NULL", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRunPlacementFixture(t)
			candidate := registerPreparationCandidate(t, f)
			if test.update != "" {
				dbtest.MustExec(t, f.ctx, f.pool, "UPDATE runtime_instances SET "+test.update+" WHERE id=$1", candidate.RuntimeInstanceID)
			}
			n, err := f.authority.RecoverExpiredRuntimeReservations(f.ctx, 10)
			if err != nil || n != 0 {
				t.Fatalf("recovery with no expiry: n=%d, %v", n, err)
			}
			got := readPreparationCandidate(t, f, candidate)
			want := "registered"
			if test.abandoned {
				want = "abandoned"
			}
			if got.Status != want {
				t.Fatalf("status=%s want=%s", got.Status, want)
			}
		})
	}
}

func TestComputerInitializationRecoveryRespectsRuntimeLockAndConsumption(t *testing.T) {
	for _, consume := range []bool{false, true} {
		name := "rollback"
		if consume {
			name = "consumed"
		}
		t.Run(name, func(t *testing.T) {
			f := newRunPlacementFixture(t)
			candidate := registerPreparationCandidate(t, f)
			dbtest.MustExec(t, f.ctx, f.pool, `UPDATE runtime_instances SET preparation_expires_at=now()-interval '1 second' WHERE id=$1`, candidate.RuntimeInstanceID)
			tx, err := f.pool.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			// Match the publication owner's Runtime -> candidate order. A concurrent
			// recovery sweep must not block unrelated candidates on this lock.
			var id pgtype.UUID
			if err := tx.QueryRow(f.ctx, `SELECT id FROM runtime_instances WHERE id=$1 FOR UPDATE`, candidate.RuntimeInstanceID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			if consume {
				artifactID := pgvalue.UUID(uuid.NewV7())
				dbtest.MustExec(t, f.ctx, tx, `INSERT INTO cas_objects (org_id,digest,size_bytes,media_type) VALUES ($1,$2,$3,$4)`, f.orgID, candidate.Digest, candidate.SizeBytes, candidate.MediaType)
				dbtest.MustExec(t, f.ctx, tx, `INSERT INTO artifacts (id,org_id,project_id,environment_id,digest,kind,size_bytes,media_type)
                    SELECT $1,r.org_id,r.project_id,r.environment_id,$2,'workspace_version',$3,$4 FROM runtime_instances r WHERE r.id=$5`, artifactID, candidate.Digest, candidate.SizeBytes, candidate.MediaType, candidate.RuntimeInstanceID)
				if _, err := db.New(tx).ConsumeComputerInitialization(f.ctx, db.ConsumeComputerInitializationParams{
					ID: candidate.ID, EnvironmentID: candidate.EnvironmentID, ComputerID: candidate.ComputerID, ArtifactID: artifactID,
				}); err != nil {
					t.Fatal(err)
				}
			}
			// The expired Runtime is visible but locked. Consumption here tests
			// the low-level terminal transition only, not permission to publish
			// after expiry (which the future publication owner must reject).
			ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
			defer cancel()
			if n, err := db.New(f.pool).AbandonRevokedComputerInitializations(ctx, 10); err != nil || n != 0 {
				t.Fatalf("concurrent recovery: n=%d, %v", n, err)
			}
			if consume {
				err = tx.Commit(f.ctx)
			} else {
				err = tx.Rollback(f.ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, f.ctx, f.pool, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, candidate.RuntimeInstanceID)
			if _, err := f.authority.RecoverExpiredRuntimeReservations(f.ctx, 10); err != nil {
				t.Fatal(err)
			}
			got := readPreparationCandidate(t, f, candidate)
			want := "abandoned"
			if consume {
				want = "consumed"
			}
			if got.Status != want {
				t.Fatalf("terminal status=%s want=%s", got.Status, want)
			}
		})
	}
}
