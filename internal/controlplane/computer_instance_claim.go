package controlplane

import (
	"context"
	"crypto/sha256"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Claim the channel of an already prepared physical Instance. Allocation and
// Program/Run admission are independent operations.
func claimComputerInstanceChannel(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, instanceID, environmentID pgtype.UUID, token string) (db.ComputerInstance, error) {
	q := db.New(tx)
	target, err := q.GetComputerInstance(ctx, db.GetComputerInstanceParams{ID: instanceID, EnvironmentID: environmentID})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	bindings, err := q.LockComputerSecretsForAdmission(ctx, target.ComputerID)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	locked, err := workergroup.LockHost(ctx, q, worker)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if !locked.Continues() {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	c, err := q.LockComputer(ctx, db.LockComputerParams{ID: target.ComputerID, EnvironmentID: target.EnvironmentID})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	i, err := q.LockWorkerComputerInstance(ctx, db.LockWorkerComputerInstanceParams{ID: target.ID, OrgID: target.OrgID, WorkerHostID: locked.Host.ID, WorkerGroupID: locked.Group.ID, WorkerEpoch: worker.Epoch})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if c.DirtyState == "dirty_state_lost" || c.DirtyState == "capture_failed" || c.Status != "active" || c.DesiredState != "active" || c.WriterGeneration != i.WriterGeneration || i.ComputerID != c.ID || i.EnvironmentID != c.EnvironmentID || !i.SourceDiskVersionID.Valid || token == "" {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	for _, b := range bindings {
		if b.SecretStatus != "active" || !b.CurrentVersionID.Valid {
			return db.ComputerInstance{}, pgx.ErrNoRows
		}
	}
	hash := sha256.Sum256([]byte(token))
	return q.ClaimComputerInstanceChannel(ctx, db.ClaimComputerInstanceChannelParams{ID: i.ID, WorkerHostID: locked.Host.ID, WorkerEpoch: worker.Epoch, WriterGeneration: i.WriterGeneration, TokenHash: hash[:], WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds})
}
