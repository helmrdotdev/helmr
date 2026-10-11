package agent

import (
	"fmt"
	"testing"
	"uuid"
)

func TestRevokedImagePayloadRetirementPreservesLineageAndComputerRoot(t *testing.T) {
	for _, attached := range []bool{false, true} {
		t.Run(fmt.Sprint(attached), func(t *testing.T) {
			p, key := newPreparationPublicationTest(t)
			root := p.capture(t, key)
			if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "published before revocation"); err != nil {
				t.Fatal(err)
			}
			var computer, image uuid.UUID
			if err := p.f.pool.QueryRow(t.Context(), `SELECT id FROM computers WHERE environment_id=$1 AND preparation_id=$2`, p.f.env, p.ref.PreparationID).Scan(&computer); err != nil {
				t.Fatal(err)
			}
			if err := p.f.pool.QueryRow(t.Context(), `SELECT id FROM computer_images WHERE environment_id=$1 AND preparation_id=$2`, p.f.env, p.ref.PreparationID).Scan(&image); err != nil {
				t.Fatal(err)
			}
			if attached {
				if _, err := PinComputerImage(t.Context(), p.f.pool, p.f.env, computer); err != nil {
					t.Fatal(err)
				}
			}
			reconcile := func() {
				t.Helper()
				if err := ReconcileComputerDiskRetention(t.Context(), p.f.pool); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := p.f.secrets.Revoke(t.Context(), p.f.env, p.f.secretID, "revoke-image"); err != nil {
				t.Fatal(err)
			}
			reconcile()
			var retained bool
			if err := p.f.pool.QueryRow(t.Context(), `SELECT root_id IS NOT NULL FROM computer_images WHERE environment_id=$1 AND id=$2`, p.f.env, image).Scan(&retained); err != nil || !retained {
				t.Fatalf("unfenced image collected: %v %v", retained, err)
			}
			if err := ObservePreparationStopped(t.Context(), p.f.pool, *p.f.host(), p.ref.Identity()); err != nil {
				t.Fatal(err)
			}
			for range 8 {
				reconcile()
			}
			var retired, evidence, keyAvailable bool
			if err := p.f.pool.QueryRow(t.Context(), `SELECT i.root_id IS NULL AND i.payload_retired_at IS NOT NULL,
 p.capture_root IS NOT NULL AND i.publication_evidence IS NOT NULL AND p.write_key_id=$3,k.available
 FROM computer_images i JOIN computer_preparations p ON p.environment_id=i.environment_id AND p.id=i.preparation_id
 JOIN computer_data_keys k ON k.id=p.write_key_id WHERE i.environment_id=$1 AND i.id=$2`, p.f.env, image, key.ID).Scan(&retired, &evidence, &keyAvailable); err != nil || !retired || !evidence || keyAvailable != attached {
				t.Fatalf("retired=%v evidence=%v key=%v attached=%v error=%v", retired, evidence, keyAvailable, attached, err)
			}
			if attached {
				var lineage bool
				if err := p.f.pool.QueryRow(t.Context(), `SELECT c.image_id=$3 AND c.initial_root_id IS NOT NULL AND EXISTS(SELECT 1 FROM computer_secret_revocations r WHERE r.environment_id=c.environment_id AND r.computer_id=c.id) FROM computers c WHERE environment_id=$1 AND id=$2`, p.f.env, computer, image).Scan(&lineage); err != nil || !lineage {
					t.Fatalf("historical revocation or Computer root lost: %v %v", lineage, err)
				}
			}
			if _, err := PinComputerImage(t.Context(), p.f.pool, p.f.env, computer); err == nil {
				t.Fatal("revoked image admitted after payload retirement")
			}
		})
	}
}
