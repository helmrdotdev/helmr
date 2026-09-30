package computer

import (
	"bytes"
	"context"
	"errors"
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

// RunInstance is the Computer and the Instance of a Run lease, locked in the
// owning transaction in that order by LockInstanceForRun. It is valid only
// inside that transaction.
type RunInstance struct {
	tx       pgx.Tx
	computer db.LockRunLeaseClaimComputerRow
	instance db.ComputerInstance
}

// LockInstanceForRun locks the Computer, then the Instance, of a Run lease.
// The worker host and any Computers the Run's lineage reaches must already be
// locked. The Computer must be active and clean, and the Instance the ready,
// mounted, unreclaimed incarnation of the lease's writer generation with the
// admission the access requires.
func LockInstanceForRun(ctx context.Context, tx pgx.Tx, ref RunInstanceRef, access RunAccess) (RunInstance, error) {
	q := db.New(tx)
	org, project, environment, computerID := pgvalue.UUID(ref.OrgID), pgvalue.UUID(ref.ProjectID), pgvalue.UUID(ref.EnvironmentID), pgvalue.UUID(ref.ComputerID)
	c, err := q.LockRunLeaseClaimComputer(ctx, db.LockRunLeaseClaimComputerParams{ID: computerID, OrgID: org, ProjectID: project, EnvironmentID: environment, RegionID: ref.RegionID})
	if err != nil {
		return RunInstance{}, err
	}
	i, err := q.LockRunLeaseClaimInstance(ctx, db.LockRunLeaseClaimInstanceParams{ID: pgvalue.UUID(ref.InstanceID), OrgID: org, ProjectID: project, EnvironmentID: environment, RegionID: ref.RegionID, WorkerGroupID: pgvalue.UUID(ref.Host.GroupID), WorkerHostID: pgvalue.UUID(ref.Host.HostID), WorkerEpoch: ref.Host.Epoch, ComputerID: computerID})
	if err != nil {
		return RunInstance{}, err
	}
	if c.Status != "active" || c.DesiredState != "active" || c.DeletedAt.Valid || c.DirtyState == "dirty_state_lost" || c.DirtyState == "capture_failed" || i.WriterGeneration != c.WriterGeneration || i.WriterGeneration != ref.WriterGeneration || (i.AdmissionState != "open" && !(access == RunLive && (i.AdmissionState == "draining" || i.AdmissionState == "checkpointing")) && !(access == RunResume && (i.AdmissionState == "restoring" || i.AdmissionState == "draining"))) || i.DesiredState != "ready" || i.ObservedState != "ready" || i.ObservedDesiredVersion != i.DesiredVersion || i.MountState != "mounted" || i.ReclaimedAt.Valid || i.TerminalAt.Valid {
		return RunInstance{}, pgx.ErrNoRows
	}
	return RunInstance{tx: tx, computer: c, instance: i}, nil
}

// Computer is the locked Computer.
func (r RunInstance) Computer() db.LockRunLeaseClaimComputerRow {
	return r.computer
}

// Instance is the locked Instance.
func (r RunInstance) Instance() db.ComputerInstance {
	return cloneInstance(r.instance)
}

// cloneInstance copies every slice of an Instance row, so a caller cannot
// change what an accessor returns next through a returned row.
func cloneInstance(i db.ComputerInstance) db.ComputerInstance {
	i.ReclaimEvidence = bytes.Clone(i.ReclaimEvidence)
	i.TerminalError = bytes.Clone(i.TerminalError)
	i.WriterTokenHash = bytes.Clone(i.WriterTokenHash)
	i.GuestChannelTokenHash = bytes.Clone(i.GuestChannelTokenHash)
	i.FinalizationError = bytes.Clone(i.FinalizationError)
	return i
}

// RecordStart records a Run start as activity on the locked Computer. It
// applies no status or writer generation predicate: the caller's Run lease
// fence has already validated both under the locks.
func (r RunInstance) RecordStart(ctx context.Context) error {
	_, err := r.tx.Exec(ctx, `UPDATE computers SET last_activity_at=greatest(last_activity_at,clock_timestamp()),updated_at=clock_timestamp() WHERE id=$1`, r.computer.ID)
	return err
}

// CommandRef addresses a Computer Command in its Environment.
type CommandRef struct {
	EnvironmentID uuid.UUID
	ComputerID    uuid.UUID
	CommandID     uuid.UUID
}

// CommandInstance is the Computer and the Instance a Command was bound to,
// locked in the owning transaction in that order. It grants no capability:
// the caller's Command operation checks the Command and decides which of
// the predicates it requires. A Command that was never bound, or whose
// Instance incarnation no longer matches its writer generation, locks only
// the Computer and is not Bound. A CommandInstance is valid only inside the
// transaction that locked it.
type CommandInstance struct {
	tx       pgx.Tx
	computer db.Computer
	instance db.ComputerInstance
	bound    bool
}

// LockCommandInstance locks the Computer, then the exact Instance incarnation
// the Command was bound to; a newer Instance is never substituted. The caller
// takes any Secret and worker host locks first and locks the Command after.
// A missing Computer returns pgx.ErrNoRows; a missing bound Instance is not
// an error.
func LockCommandInstance(ctx context.Context, tx pgx.Tx, ref CommandRef) (CommandInstance, error) {
	q := db.New(tx)
	environment, computerID := pgvalue.UUID(ref.EnvironmentID), pgvalue.UUID(ref.ComputerID)
	c, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: environment, ID: computerID})
	if err != nil {
		return CommandInstance{}, err
	}
	i, err := q.LockComputerCommandInstance(ctx, db.LockComputerCommandInstanceParams{EnvironmentID: environment, ComputerID: computerID, CommandID: pgvalue.UUID(ref.CommandID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return CommandInstance{tx: tx, computer: c}, nil
	}
	if err != nil {
		return CommandInstance{}, err
	}
	return CommandInstance{tx: tx, computer: c, instance: i, bound: true}, nil
}

// Computer is the locked Computer.
func (c CommandInstance) Computer() db.Computer {
	return c.computer
}

// Instance is the locked bound Instance; it is the zero value when the
// Command is not Bound.
func (c CommandInstance) Instance() db.ComputerInstance {
	return c.instance
}

// Bound reports whether the Command's Instance incarnation was locked.
func (c CommandInstance) Bound() bool {
	return c.bound
}

// On reports whether the bound Instance is the addressed incarnation on the
// worker host epoch at the writer generation the host acts for.
func (c CommandInstance) On(host Host, instanceID uuid.UUID, writerGeneration int64) bool {
	i := c.instance
	return c.bound && i.ID == pgvalue.UUID(instanceID) && i.WorkerHostID == pgvalue.UUID(host.HostID) && i.WorkerGroupID == pgvalue.UUID(host.GroupID) && i.WorkerEpoch == host.Epoch && i.WriterGeneration == writerGeneration
}

// Serving reports whether the bound Instance is unreclaimed, desired and
// observed ready at its desired version, mounted, and the Computer's current
// writer. Admission and Computer status checks stay with the caller.
func (c CommandInstance) Serving() bool {
	i := c.instance
	return c.bound && !i.ReclaimedAt.Valid && i.DesiredState == "ready" && i.ObservedState == "ready" && i.ObservedDesiredVersion == i.DesiredVersion && i.MountState == "mounted" && i.WriterGeneration == c.computer.WriterGeneration
}

// TouchActivity records member activity on the locked Computer while it is
// active and the bound Instance's writer generation is still its current
// one; otherwise, including when the Command is not Bound, it records
// nothing. orgID and projectID scope the Command's Environment.
func (c CommandInstance) TouchActivity(ctx context.Context, orgID, projectID uuid.UUID) error {
	if !c.bound {
		return nil
	}
	_, err := db.New(c.tx).TouchRunComputerActivity(ctx, db.TouchRunComputerActivityParams{
		ID: c.computer.ID, EnvironmentID: c.computer.EnvironmentID,
		OrgID: pgvalue.UUID(orgID), ProjectID: pgvalue.UUID(projectID), WriterGeneration: c.instance.WriterGeneration,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
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

// LockInstanceForCommand locks the Command's Instance through
// LockCommandInstance for an operation that grants a worker host authority
// over the Command. The worker host must already be locked. The Computer must
// be active and its current writer the Instance, which must be the addressed
// ready, mounted and unreclaimed incarnation on the host epoch. Admission
// checks stay with the caller's Command operation.
func LockInstanceForCommand(ctx context.Context, tx pgx.Tx, ref CommandInstanceRef) (db.ComputerInstance, error) {
	locked, err := LockCommandInstance(ctx, tx, CommandRef{EnvironmentID: ref.EnvironmentID, ComputerID: ref.ComputerID, CommandID: ref.CommandID})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if !locked.On(ref.Host, ref.InstanceID, ref.WriterGeneration) {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	if c := locked.Computer(); c.Status != "active" || c.DesiredState != "active" || !locked.Serving() {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	return locked.Instance(), nil
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
