package agent

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestPreparationDiskRetentionRequiresPhysicalClosure(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "unpublished", true: "published"}[published], func(t *testing.T) {
			p, key := newPreparationPublicationTest(t)
			root := p.capture(t, key)
			if published {
				if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "retained publication"); err != nil {
					t.Fatal(err)
				}
			}
			dbtest.MustExec(t, t.Context(), p.f.pool, `UPDATE computer_preparations SET executor_expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, p.f.env, p.ref.PreparationID)
			if err := expirePreparation(t.Context(), p.f.pool, p.f.env, p.ref.PreparationID); err != nil {
				t.Fatal(err)
			}
			reconcile := func() {
				t.Helper()
				if err := ReconcileComputerDiskRetention(t.Context(), p.f.pool); err != nil {
					t.Fatal(err)
				}
			}
			reconcile()
			var pinned bool
			if err := p.f.pool.QueryRow(t.Context(), `SELECT disk_released_at IS NULL AND EXISTS(SELECT 1 FROM computer_object_pins x WHERE x.environment_id=p.environment_id AND x.preparation_id=p.id) FROM computer_preparations p WHERE environment_id=$1 AND id=$2`, p.f.env, p.ref.PreparationID).Scan(&pinned); err != nil || !pinned {
				t.Fatalf("unfenced preparation lost pins: %v %v", pinned, err)
			}
			if _, err := p.f.pool.Exec(t.Context(), `UPDATE computer_data_keys SET retired_at=clock_timestamp(),wrapped_key=NULL WHERE id=$1`, key.ID); err == nil {
				t.Fatal("unfenced preparation key retired")
			}
			if err := ObservePreparationStopped(t.Context(), p.f.pool, *p.f.host(), p.ref.Identity()); err != nil {
				t.Fatal(err)
			}
			for range 8 {
				reconcile()
			}
			var released, identityRetained, available bool
			if err := p.f.pool.QueryRow(t.Context(), `SELECT p.disk_released_at IS NOT NULL,p.write_key_id=$3 AND p.capture_root IS NOT NULL AND p.capture_evidence IS NOT NULL,k.available
 FROM computer_preparations p JOIN computer_data_keys k ON k.id=p.write_key_id WHERE p.environment_id=$1 AND p.id=$2`, p.f.env, p.ref.PreparationID, key.ID).Scan(&released, &identityRetained, &available); err != nil || !released || !identityRetained || available != published {
				t.Fatalf("release=%v identity=%v key available=%v published=%v error=%v", released, identityRetained, available, published, err)
			}
			if published {
				if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "same publication after attachment release"); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
