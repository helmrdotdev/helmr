package computer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Assignment is a prepared Instance whose guest channel a worker host
// claimed: the Instance, its image and VM runtime source, and the channel
// credential whose hash the Instance recorded.
type Assignment struct {
	Instance          db.ComputerInstance
	Source            db.GetComputerInstanceAssignmentSourceRow
	ChannelCredential string
}

// ClaimInstance claims the guest channel of one prepared, unclaimed Instance
// on the principal's host epoch. Each candidate is tried in its own
// transaction with a fresh channel credential generated outside it; a candidate
// whose authority changed is skipped. It returns nil when no candidate can be
// claimed, and workergroup.ErrStaleClaims when the principal's claim
// versions changed. Allocation and Program or Run admission are independent
// operations.
func ClaimInstance(ctx context.Context, q db.Querier, txb db.TxBeginner, principal workergroup.HostPrincipal) (*Assignment, error) {
	targets, err := q.ListUnclaimedWorkerComputerInstances(ctx, db.ListUnclaimedWorkerComputerInstancesParams{WorkerGroupID: pgvalue.UUID(principal.GroupID), WorkerHostID: pgvalue.UUID(principal.HostID), WorkerEpoch: principal.Epoch})
	if err != nil {
		return nil, fmt.Errorf("list prepared computer instances: %w", err)
	}
	for _, target := range targets {
		credential, err := auth.GenerateOpaque(32)
		if err != nil {
			return nil, fmt.Errorf("generate computer instance channel: %w", err)
		}
		var assignment *Assignment
		err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
			i, err := claimChannel(ctx, tx, principal, target.ID, target.EnvironmentID, credential)
			if err != nil {
				return err
			}
			source, err := db.New(tx).GetComputerInstanceAssignmentSource(ctx, i.ID)
			if err != nil {
				return err
			}
			assignment = &Assignment{Instance: i, Source: source, ChannelCredential: credential}
			return nil
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return assignment, nil
	}
	return nil, nil
}

// claimChannel claims the channel of an already prepared physical Instance
// under secrets → group → host → Computer → Instance locks.
func claimChannel(ctx context.Context, tx pgx.Tx, principal workergroup.HostPrincipal, instanceID, environmentID pgtype.UUID, credential string) (db.ComputerInstance, error) {
	q := db.New(tx)
	target, err := q.GetComputerInstance(ctx, db.GetComputerInstanceParams{ID: instanceID, EnvironmentID: environmentID})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	bindings, err := q.LockComputerSecretsForAdmission(ctx, target.ComputerID)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	locked, err := workergroup.LockHost(ctx, q, principal)
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
	i, err := q.LockWorkerComputerInstance(ctx, db.LockWorkerComputerInstanceParams{ID: target.ID, OrgID: target.OrgID, WorkerHostID: locked.Host.ID, WorkerGroupID: locked.Group.ID, WorkerEpoch: principal.Epoch})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if c.DirtyState == "dirty_state_lost" || c.Status != "active" || c.DesiredState != "active" || c.WriterGeneration != i.WriterGeneration || i.ComputerID != c.ID || i.EnvironmentID != c.EnvironmentID || !i.SourceDiskVersionID.Valid || credential == "" {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	for _, b := range bindings {
		if b.SecretStatus != "active" || !b.CurrentVersionID.Valid {
			return db.ComputerInstance{}, pgx.ErrNoRows
		}
	}
	hash := sha256.Sum256([]byte(credential))
	return q.ClaimComputerInstanceChannel(ctx, db.ClaimComputerInstanceChannelParams{ID: i.ID, WorkerHostID: locked.Host.ID, WorkerEpoch: principal.Epoch, WriterGeneration: i.WriterGeneration, CredentialHash: hash[:], WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds})
}

// WriterRef addresses the writer generation of one Instance a worker host
// holds.
type WriterRef struct {
	EnvironmentID    uuid.UUID
	InstanceID       uuid.UUID
	WriterGeneration int64
}

// RenewInstance renews the Instance writer for WriterTTL in one transaction
// under secrets → group → host → Computer → Instance locks. A close intent is
// returned unchanged as an observation, never a renewed write grant. Renewal
// neither renews member executions nor reopens admission. It returns
// ErrAuthorityChanged when the writer is stale and
// workergroup.ErrStaleClaims when the principal's claim versions changed.
func RenewInstance(ctx context.Context, txb db.TxBeginner, principal workergroup.HostPrincipal, writer WriterRef) (db.ComputerInstance, error) {
	var instance db.ComputerInstance
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		var err error
		instance, err = renewWriter(ctx, tx, principal, writer)
		return err
	})
	return instance, authorityChanged(err)
}

func renewWriter(ctx context.Context, tx pgx.Tx, principal workergroup.HostPrincipal, writer WriterRef) (db.ComputerInstance, error) {
	if writer.WriterGeneration <= 0 {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	q := db.New(tx)
	target, err := q.GetComputerInstance(ctx, db.GetComputerInstanceParams{ID: pgvalue.UUID(writer.InstanceID), EnvironmentID: pgvalue.UUID(writer.EnvironmentID)})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	bindings, err := q.LockComputerSecretsForAdmission(ctx, target.ComputerID)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	locked, err := workergroup.LockHost(ctx, q, principal)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if !locked.Continues() {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	c, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: target.EnvironmentID, ID: target.ComputerID})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	i, err := q.LockWorkerComputerInstance(ctx, db.LockWorkerComputerInstanceParams{ID: target.ID, OrgID: target.OrgID, WorkerHostID: locked.Host.ID, WorkerGroupID: locked.Group.ID, WorkerEpoch: principal.Epoch})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if i.WriterGeneration != writer.WriterGeneration || i.ComputerID != c.ID || i.EnvironmentID != c.EnvironmentID {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	// A close intent is an observation, never a renewed write grant.
	if i.DesiredState == "closed" {
		return i, nil
	}
	for _, b := range bindings {
		if b.SecretStatus != "active" || !b.CurrentVersionID.Valid {
			return db.ComputerInstance{}, pgx.ErrNoRows
		}
	}
	if c.Status != "active" || c.DesiredState != "active" || c.WriterGeneration != i.WriterGeneration {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	return q.RenewComputerInstanceWriter(ctx, db.RenewComputerInstanceWriterParams{ID: i.ID, WorkerHostID: locked.Host.ID, WorkerEpoch: principal.Epoch, WriterGeneration: i.WriterGeneration, WriterTokenHash: i.WriterTokenHash, TtlSeconds: int64(WriterTTL / time.Second)})
}
