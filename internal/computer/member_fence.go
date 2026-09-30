package computer

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Member fences lock a Computer and its Instance for an operation of one of
// their members, in the caller's transaction and before the caller locks the
// member rows (pglock documents the order). They check the Computer and
// Instance authority the member operation needs; the caller keeps the member
// checks and the final deadline recheck after its last lock. A fence that no
// longer holds returns pgx.ErrNoRows.

// RunAccess is the Instance admission a Run lease operation requires.
type RunAccess uint8

const (
	// RunAdmission claims or starts a lease: the Instance admission is open.
	RunAdmission RunAccess = iota
	// RunLive continues a started Run: the Instance may also be draining or
	// checkpointing.
	RunLive
	// RunResume resumes a restored Run: the Instance may also be restoring or
	// draining.
	RunResume
)

// RunInstanceRef addresses the Instance a Run lease was assigned: the lease's
// Computer and Instance on the worker host epoch at the lease's writer
// generation.
type RunInstanceRef struct {
	OrgID            uuid.UUID
	ProjectID        uuid.UUID
	EnvironmentID    uuid.UUID
	RegionID         string
	ComputerID       uuid.UUID
	InstanceID       uuid.UUID
	Host             Host
	WriterGeneration int64
}

// LockInstanceForRun locks the Computer, then the Instance, of a Run lease.
// The worker host and any Computers the Run's lineage reaches must already be
// locked. The Computer must be active and clean, and the Instance the ready,
// mounted, unreclaimed incarnation of the lease's writer generation with the
// admission the access requires.
func LockInstanceForRun(ctx context.Context, tx pgx.Tx, ref RunInstanceRef, access RunAccess) (db.LockRunLeaseClaimComputerRow, db.ComputerInstance, error) {
	q := db.New(tx)
	org, project, environment, computerID := pgvalue.UUID(ref.OrgID), pgvalue.UUID(ref.ProjectID), pgvalue.UUID(ref.EnvironmentID), pgvalue.UUID(ref.ComputerID)
	c, err := q.LockRunLeaseClaimComputer(ctx, db.LockRunLeaseClaimComputerParams{ID: computerID, OrgID: org, ProjectID: project, EnvironmentID: environment, RegionID: ref.RegionID})
	if err != nil {
		return db.LockRunLeaseClaimComputerRow{}, db.ComputerInstance{}, err
	}
	i, err := q.LockRunLeaseClaimInstance(ctx, db.LockRunLeaseClaimInstanceParams{ID: pgvalue.UUID(ref.InstanceID), OrgID: org, ProjectID: project, EnvironmentID: environment, RegionID: ref.RegionID, WorkerGroupID: pgvalue.UUID(ref.Host.GroupID), WorkerHostID: pgvalue.UUID(ref.Host.HostID), WorkerEpoch: ref.Host.Epoch, ComputerID: computerID})
	if err != nil {
		return db.LockRunLeaseClaimComputerRow{}, db.ComputerInstance{}, err
	}
	if c.Status != "active" || c.DesiredState != "active" || c.DeletedAt.Valid || c.DirtyState == "dirty_state_lost" || c.DirtyState == "capture_failed" || i.WriterGeneration != c.WriterGeneration || i.WriterGeneration != ref.WriterGeneration || (i.AdmissionState != "open" && !(access == RunLive && (i.AdmissionState == "draining" || i.AdmissionState == "checkpointing")) && !(access == RunResume && (i.AdmissionState == "restoring" || i.AdmissionState == "draining"))) || i.DesiredState != "ready" || i.ObservedState != "ready" || i.ObservedDesiredVersion != i.DesiredVersion || i.MountState != "mounted" || i.ReclaimedAt.Valid || i.TerminalAt.Valid {
		return db.LockRunLeaseClaimComputerRow{}, db.ComputerInstance{}, pgx.ErrNoRows
	}
	return c, i, nil
}

// CommandInstanceRef addresses the Instance a Computer Command was bound to:
// the Instance incarnation on the worker host epoch at the writer generation
// the host acts for.
type CommandInstanceRef struct {
	EnvironmentID    uuid.UUID
	ComputerID       uuid.UUID
	CommandID        uuid.UUID
	InstanceID       uuid.UUID
	Host             Host
	WriterGeneration int64
}

// LockInstanceForCommand locks the Computer, then the Instance bound to the
// Command. The worker host must already be locked. The Computer must be
// active and its current writer the Instance, which must be the addressed
// ready, mounted and unreclaimed incarnation on the host epoch. Admission
// checks stay with the caller's Command operation.
func LockInstanceForCommand(ctx context.Context, tx pgx.Tx, ref CommandInstanceRef) (db.ComputerInstance, error) {
	q := db.New(tx)
	environment, computerID := pgvalue.UUID(ref.EnvironmentID), pgvalue.UUID(ref.ComputerID)
	c, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: environment, ID: computerID})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	i, err := q.LockComputerCommandInstance(ctx, db.LockComputerCommandInstanceParams{EnvironmentID: environment, ComputerID: computerID, CommandID: pgvalue.UUID(ref.CommandID)})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if i.ID != pgvalue.UUID(ref.InstanceID) || i.WorkerHostID != pgvalue.UUID(ref.Host.HostID) || i.WorkerGroupID != pgvalue.UUID(ref.Host.GroupID) || i.WorkerEpoch != ref.Host.Epoch || i.WriterGeneration != ref.WriterGeneration {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	if c.Status != "active" || c.DesiredState != "active" || i.ReclaimedAt.Valid || i.DesiredState != "ready" || i.ObservedState != "ready" || i.ObservedDesiredVersion != i.DesiredVersion || i.MountState != "mounted" || i.WriterGeneration != c.WriterGeneration {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	return i, nil
}

// LockRunComputers update-locks, in id order, the Computers of the Runs,
// then, in id order, the unreclaimed Instances of those Computers. Callers
// that lock several Runs' members take it before any member lock so that
// concurrent graphs acquire Computers in one order.
func LockRunComputers(ctx context.Context, tx pgx.Tx, runIDs []uuid.UUID) error {
	q := db.New(tx)
	ids := pgUUIDs(runIDs)
	if _, err := q.LockCancellationComputers(ctx, ids); err != nil {
		return err
	}
	_, err := q.LockCancellationInstances(ctx, ids)
	return err
}

// LockRunComputersWithTarget is LockRunComputers that also locks the target
// Computer in the same id-ordered statement, for a Run operation that
// addresses another Computer.
func LockRunComputersWithTarget(ctx context.Context, tx pgx.Tx, runIDs []uuid.UUID, target pgtype.UUID) error {
	ids := pgUUIDs(runIDs)
	rows, err := tx.Query(ctx, `SELECT id FROM computers WHERE id=$2 OR id IN
 (SELECT computer_id FROM runs WHERE id=ANY($1::uuid[])) ORDER BY id FOR UPDATE`, ids, target)
	if err != nil {
		return err
	}
	for rows.Next() {
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	_, err = db.New(tx).LockCancellationInstances(ctx, ids)
	return err
}

func pgUUIDs(values []uuid.UUID) []pgtype.UUID {
	result := make([]pgtype.UUID, len(values))
	for n, value := range values {
		result[n] = pgvalue.UUID(value)
	}
	return result
}
