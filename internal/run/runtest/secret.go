package runtest

import (
	"context"
	"fmt"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

// AddSecret inserts an active Secret with one version at revocation
// generation zero and returns its id.
func (fixture Fixture) AddSecret(t *testing.T, name string) uuid.UUID {
	t.Helper()
	ctx := t.Context()
	secretID, versionID := uuid.NewV7(), uuid.NewV7()
	tx, err := fixture.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	dbtest.MustExec(t, ctx, tx, `INSERT INTO secrets(id,environment_id,name,current_version_id) VALUES($1,$2,$3,$4)`, secretID, fixture.EnvironmentID, name, versionID)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO secret_versions(id,secret_id,version,nonce,ciphertext) VALUES($1,$2,1,decode(repeat('01',12),'hex'),decode(repeat('02',16),'hex'))`, versionID, secretID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return secretID
}

// PlaceSecret places the Secret on the Run lease's Computer as raw
// environment variables SECRET_0 through SECRET_<placements-1>.
func (fixture Fixture) PlaceSecret(t *testing.T, leaseID, secretID uuid.UUID, placements int) {
	t.Helper()
	for n := range placements {
		dbtest.MustExec(t, t.Context(), fixture.Pool, `INSERT INTO computer_secrets(mode,computer_id,environment_id,placement_kind,placement_target,secret_id)
 SELECT 'raw',computer_id,environment_id,'env',$2,$3 FROM run_leases WHERE id=$1`, leaseID, fmt.Sprintf("SECRET_%d", n), secretID)
	}
}

// ResolveRunSecret records that the Run lease's attempt resolved the
// Secret's current version, placed as SECRET_0, at its current revocation
// generation.
func (fixture Fixture) ResolveRunSecret(t *testing.T, work RunLease, secretID uuid.UUID) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), fixture.Pool, `INSERT INTO secret_resolutions(id,computer_id,run_id,attempt_number,placement_kind,placement_target,secret_id,secret_version_id,revocation_generation)
 SELECT $2,l.computer_id,l.run_id,l.attempt_number,'env','SECRET_0',s.id,s.current_version_id,s.revocation_generation FROM run_leases l, secrets s WHERE l.id=$1 AND s.id=$3`, work.LeaseID, uuid.NewV7(), secretID)
}

// RevokeSecret revokes the Secret at the revocation generation.
func (fixture Fixture) RevokeSecret(t *testing.T, secretID uuid.UUID, generation int64) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE secrets SET status='revoked',current_version_id=NULL,revoked_at=now(),revocation_generation=$2,revision=revision+1 WHERE id=$1`, secretID, generation)
}
