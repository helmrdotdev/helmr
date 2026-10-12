package agent_test

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/dispatch"

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
			dbtest.MustExec(t, t.Context(), p.f.pool, `UPDATE image_builds SET executor_expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, p.f.env, p.ref.ImageBuildID)
			if err := dispatch.ExpireImageBuild(t.Context(), p.f.pool, p.f.env, p.ref.ImageBuildID); err != nil {
				t.Fatal(err)
			}
			reconcile := func() {
				t.Helper()
				if err := computer.ReconcileDiskRetention(t.Context(), p.f.pool); err != nil {
					t.Fatal(err)
				}
			}
			reconcile()
			var pinned bool
			if err := p.f.pool.QueryRow(t.Context(), `SELECT disk_released_at IS NULL AND EXISTS(SELECT 1 FROM disk_object_pins x WHERE x.environment_id=p.environment_id AND x.image_build_id=p.id) FROM image_builds p WHERE environment_id=$1 AND id=$2`, p.f.env, p.ref.ImageBuildID).Scan(&pinned); err != nil || !pinned {
				t.Fatalf("unfenced preparation lost pins: %v %v", pinned, err)
			}
			if _, err := p.f.pool.Exec(t.Context(), `UPDATE disk_encryption_keys SET retired_at=clock_timestamp(),wrapped_key=NULL WHERE id=$1`, key.ID); err == nil {
				t.Fatal("unfenced preparation key retired")
			}
			if err := computer.ObserveImageBuildStopped(t.Context(), p.f.pool, *p.f.host(), preparationIdentity(p.ref)); err != nil {
				t.Fatal(err)
			}
			for range 8 {
				reconcile()
			}
			var released, identityRetained, available bool
			if err := p.f.pool.QueryRow(t.Context(), `SELECT p.disk_released_at IS NOT NULL,p.write_key_id=$3 AND p.capture_root IS NOT NULL AND p.capture_evidence IS NOT NULL,k.available
 FROM image_builds p JOIN disk_encryption_keys k ON k.id=p.write_key_id WHERE p.environment_id=$1 AND p.id=$2`, p.f.env, p.ref.ImageBuildID, key.ID).Scan(&released, &identityRetained, &available); err != nil || !released || !identityRetained || available != published {
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
