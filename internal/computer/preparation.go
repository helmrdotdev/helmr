package computer

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// PreparationRef addresses the preparation of one allocated Instance at the
// desired version the worker host acts on.
type PreparationRef struct {
	InstanceID     uuid.UUID
	DesiredVersion int64
}

// preparationFence is a preparation whose supply, Computer, Instance and disk
// version locks hold in the owning transaction, before the principal's claim
// versions are compared. Only its claim method yields an authority.
type preparationFence struct {
	tx                                          pgx.Tx
	orgID, projectID, environmentID, computerID pgtype.UUID
	versionID                                   pgtype.UUID
	writerGeneration, logicalBytes              int64
	instance                                    db.ComputerInstance
}

// initialFence fences the preparation of an Instance that initializes its
// Computer's head version.
type initialFence struct{ preparationFence }

// sourceFence fences the preparation of an Instance that restores its
// committed or private source version.
type sourceFence struct{ preparationFence }

// initialPreparation is the authority of an initial preparation: its fence
// holds and the principal's claims were current after the locks. It records
// initial objects, pins the initial key and publishes the initial version.
type initialPreparation struct{ preparationFence }

// sourcePreparation is the authority of a source preparation: its fence holds
// and the principal's claims were current after the locks. It pins the
// Instance write key and delivers the retained source keys.
type sourcePreparation struct{ preparationFence }

func lockInitialFence(ctx context.Context, tx pgx.Tx, principal workergroup.HostPrincipal, ref PreparationRef) (initialFence, error) {
	f, err := lockPreparationFence(ctx, tx, principal, ref, true)
	return initialFence{f}, err
}

func lockSourceFence(ctx context.Context, tx pgx.Tx, principal workergroup.HostPrincipal, ref PreparationRef) (sourceFence, error) {
	f, err := lockPreparationFence(ctx, tx, principal, ref, false)
	return sourceFence{f}, err
}

// claim compares the principal's claim versions with the Host and Group rows
// the fence locked.
func (f initialFence) claim(ctx context.Context, principal workergroup.HostPrincipal) (initialPreparation, error) {
	if err := workergroup.CheckClaims(ctx, f.tx, principal); err != nil {
		return initialPreparation{}, err
	}
	return initialPreparation(f), nil
}

func (f sourceFence) claim(ctx context.Context, principal workergroup.HostPrincipal) (sourcePreparation, error) {
	if err := workergroup.CheckClaims(ctx, f.tx, principal); err != nil {
		return sourcePreparation{}, err
	}
	return sourcePreparation(f), nil
}

// lockInitialPreparation fences an initial preparation and compares the
// principal's claims.
func lockInitialPreparation(ctx context.Context, tx pgx.Tx, principal workergroup.HostPrincipal, ref PreparationRef) (initialPreparation, error) {
	f, err := lockInitialFence(ctx, tx, principal, ref)
	if err != nil {
		return initialPreparation{}, err
	}
	return f.claim(ctx, principal)
}

// lockPreparationFence fences the preparation of an allocated Instance as
// admission: active supply without Run or VM pauses. It locks supply (Group
// and Pool shared, Host updated), the Computer and the Instance, then reads
// the disk version: the Computer's initializing head for an initial
// preparation, or the Instance's committed or private source otherwise. The
// preparation deadlines are evaluated after the last lock. A fence that no
// longer holds returns pgx.ErrNoRows.
func lockPreparationFence(ctx context.Context, tx pgx.Tx, principal workergroup.HostPrincipal, ref PreparationRef, initial bool) (preparationFence, error) {
	runtimeID, workerID, groupID := pgvalue.UUID(ref.InstanceID), pgvalue.UUID(principal.HostID), pgvalue.UUID(principal.GroupID)
	var environmentID, computerID pgtype.UUID
	var region, observed string
	err := tx.QueryRow(ctx, `SELECT environment_id,computer_id,region_id,observed_state FROM computer_instances
 WHERE id=$1 AND worker_host_id=$2 AND worker_group_id=$3 AND worker_epoch=$4`, runtimeID, workerID, groupID, principal.Epoch).Scan(&environmentID, &computerID, &region, &observed)
	if err != nil {
		return preparationFence{}, err
	}
	admitting, err := workergroup.LockDispatchSupply(ctx, tx, workergroup.DispatchSupply{GroupID: groupID, RegionID: region, HostID: workerID, Epoch: principal.Epoch, RunArchitecture: string(definition.ArchitectureX8664)})
	if err != nil {
		return preparationFence{}, err
	}
	if !admitting {
		return preparationFence{}, pgx.ErrNoRows
	}
	q := db.New(tx)
	c, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: environmentID, ID: computerID})
	if err != nil {
		return preparationFence{}, err
	}
	i, err := q.LockComputerInstance(ctx, db.LockComputerInstanceParams{EnvironmentID: environmentID, ComputerID: computerID})
	if err != nil {
		return preparationFence{}, err
	}
	if c.Status != "active" || c.DesiredState != "active" || c.DeletedAt.Valid || len(c.RecoveryFailure) > 0 || len(c.PreparationFailure) > 0 || c.DirtyState == "dirty_state_lost" || c.DirtyState == "capture_failed" ||
		i.ID != runtimeID || i.WorkerHostID != workerID || i.WorkerGroupID != groupID || i.WorkerEpoch != principal.Epoch ||
		i.WriterGeneration != c.WriterGeneration || i.DesiredState != "ready" || i.DesiredVersion != ref.DesiredVersion ||
		i.ObservedState != "allocated" || (i.AdmissionState != "open" && i.AdmissionState != "restoring") {
		return preparationFence{}, pgx.ErrNoRows
	}
	version := i.SourceDiskVersionID
	if initial {
		if version.Valid || i.SourceCheckpointID.Valid {
			return preparationFence{}, pgx.ErrNoRows
		}
		version = c.HeadDiskVersionID
	}
	var root pgtype.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM computer_disk_versions WHERE environment_id=$1 AND computer_id=$2 AND id=$3
 AND (($4::boolean AND parent_version_id IS NULL AND status='initializing') OR (NOT $4::boolean AND status IN ('committed','private')))
 AND payload_not_retired`, environmentID, computerID, version, initial).Scan(&root)
	if err != nil {
		return preparationFence{}, err
	}
	f := preparationFence{tx: tx, orgID: i.OrgID, projectID: i.ProjectID, environmentID: environmentID, computerID: computerID, versionID: root, writerGeneration: i.WriterGeneration, logicalBytes: i.ReservedGuestEphemeralDiskBytes, instance: i}
	if err = f.checkDeadlines(ctx); err != nil {
		return preparationFence{}, err
	}
	return f, nil
}

// checkDeadlines evaluates the preparation, writer and worker observation
// deadlines. Publication rechecks them after its object writes, before
// commit.
func (f preparationFence) checkDeadlines(ctx context.Context) error {
	return checkPreparationDeadlines(ctx, f.tx, f.instance)
}

// encryptionScope is the key-wrapping scope of the prepared Computer.
func (f preparationFence) encryptionScope() (string, error) {
	return encryptionScope(pgvalue.UUIDString(f.orgID), pgvalue.UUIDString(f.environmentID), pgvalue.UUIDString(f.computerID))
}
