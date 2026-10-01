package computer

import (
	"context"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// CaptureAbortPlan is an exact decision under the source fence. Adopted means
// publication won and the source must be excluded. Otherwise the unchanged
// source may install these grants and dispositions before acknowledging thaw.
// Acknowledged receipts carry no execution grants.
type CaptureAbortPlan struct {
	Instance        db.ComputerInstance
	Checkpoint      db.ComputerCheckpoint
	Adopted         bool
	Acknowledged    bool
	WriteCapability string
	Members         []CaptureAbortMember
}

type CaptureAbortMember struct {
	RunID                     string
	AttemptNumber             int32
	RunWaitID                 string
	LeaseID                   string
	LeaseSequence             int64
	BaseComputerDiskVersionID string
	ExpiresAt                 time.Time
	Cancelled                 bool
}

// AbortCapture makes a creating candidate ineligible for publication, without
// releasing the source writer or logical member holds. Replays of the same
// receipt revalidate current ownership and read current grants. Neither a lost
// reply nor storage failure is proof of source loss.
func AbortCapture(ctx context.Context, txb db.TxBeginner, key disk.FencingKey, ref CheckpointRef) (CaptureAbortPlan, error) {
	var plan CaptureAbortPlan
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		source, err := lockCheckpointSource(ctx, tx, ref)
		if err != nil {
			return err
		}
		i, cp := source.instance, source.checkpoint
		if cp.Status == "ready" {
			raw, err := jsoncanon.Transform(cp.Manifest)
			if err != nil {
				return err
			}
			if checkpointReadyFingerprint(ref, raw) != cp.ReadyRequestFingerprint.String {
				return pgx.ErrNoRows
			}
			plan = CaptureAbortPlan{Instance: i, Checkpoint: cp, Adopted: true}
			return nil
		}
		if cp.Status == "aborted" && cp.AbortDesiredVersion.Int64 == ref.DesiredVersion+1 && cp.AbortAcknowledgedAt.Valid {
			plan = CaptureAbortPlan{Instance: i, Checkpoint: cp, Acknowledged: true}
			return nil
		}
		expectedVersion, expectedAdmission := ref.DesiredVersion, "checkpointing"
		if cp.Status == "aborted" {
			if cp.AbortDesiredVersion.Int64 != ref.DesiredVersion+1 {
				return pgx.ErrNoRows
			}
			expectedVersion, expectedAdmission = cp.AbortDesiredVersion.Int64, "resuming_capture"
		} else if cp.Status != "creating" {
			return pgx.ErrNoRows
		}
		if err := source.checkAbortSource(ctx, expectedVersion, expectedAdmission); err != nil {
			return err
		}
		members, err := source.abortMembers(ctx)
		if err != nil {
			return err
		}
		if cp.Status == "creating" {
			_, err = tx.Exec(ctx, `UPDATE computer_checkpoints SET status='aborted',abort_desired_version=$2,invalidated_at=clock_timestamp(),invalidation_reason_code='capture_aborted' WHERE id=$1`, cp.ID, ref.DesiredVersion+1)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE computer_instances SET admission_state='resuming_capture',desired_version=desired_version+1,desired_reason='capture_aborted',desired_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, i.ID)
			if err != nil {
				return err
			}
			i.DesiredVersion++
			i.AdmissionState = "resuming_capture"
			cp, err = db.New(tx).LockComputerCheckpoint(ctx, db.LockComputerCheckpointParams{EnvironmentID: i.EnvironmentID, ComputerID: i.ComputerID, CheckpointID: cp.ID})
			if err != nil {
				return err
			}
		}
		capability, err := WriteCapability(key, i)
		if err != nil {
			return err
		}
		plan = CaptureAbortPlan{Instance: i, Checkpoint: cp, WriteCapability: capability.Token, Members: members}
		return nil
	})
	return plan, authorityChanged(err)
}

func (s checkpointSource) checkAbortSource(ctx context.Context, version int64, admission string) error {
	i, cp, c := s.instance, s.checkpoint, s.computer
	if c.Status != "active" || c.DesiredState != "active" || c.WriterGeneration != i.WriterGeneration || i.DesiredState != "ready" || i.DesiredVersion != version || i.AdmissionState != admission || i.ReclaimedAt.Valid || i.CaptureCheckpointID != cp.ID || i.MembershipRevision != cp.MembershipRevision || i.ProgramDeploymentID != cp.ProgramDeploymentID || i.ComputerSpecID != cp.ComputerSpecID {
		return pgx.ErrNoRows
	}
	var fresh bool
	err := s.tx.QueryRow(ctx, `SELECT i.writer_expires_at>clock_timestamp() AND h.status IN ('active','draining') AND coalesce(h.observed_at,'-infinity'::timestamptz)>clock_timestamp()-$2*interval '1 second' FROM computer_instances i JOIN worker_hosts h ON h.id=i.worker_host_id AND h.current_epoch=i.worker_epoch WHERE i.id=$1`, i.ID, workergroup.ObservationFreshnessSeconds).Scan(&fresh)
	if err != nil {
		return err
	}
	if !fresh {
		return pgx.ErrNoRows
	}
	return nil
}

// The checkpoint source locks already cover the resident graph. A terminal
// member is cancelled in the guest rather than receiving an execution grant.
// A nonterminal lease whose ownership or deadline lapsed cannot be resurrected.
func (s checkpointSource) abortMembers(ctx context.Context) ([]CaptureAbortMember, error) {
	rows, err := s.tx.Query(ctx, `SELECT m.run_id::text,m.attempt_number,m.run_wait_id::text,m.source_run_lease_id::text,l.lease_sequence,a.base_computer_disk_version_id::text,l.expires_at,
 (r.terminal_at IS NOT NULL OR a.terminal_at IS NOT NULL OR l.terminal_at IS NOT NULL),
 coalesce(l.computer_instance_id=$2 AND l.writer_generation=$3 AND l.status='checkpointing' AND l.expires_at>clock_timestamp() AND l.process_reconciled_at IS NULL AND r.status='waiting' AND r.terminal_at IS NULL AND r.current_run_lease_id=l.id AND r.current_attempt_number=m.attempt_number AND a.terminal_at IS NULL AND w.current_run_lease_id=l.id AND w.suspend_checkpoint_id=$1 AND w.suspension_status='checkpointing' AND w.expected_run_revision=r.revision,false)
 FROM computer_checkpoint_runs m JOIN runs r ON r.id=m.run_id JOIN run_attempts a ON a.run_id=m.run_id AND a.number=m.attempt_number JOIN run_leases l ON l.id=m.source_run_lease_id JOIN run_waits w ON w.id=m.run_wait_id WHERE m.checkpoint_id=$1 ORDER BY m.run_id`, s.checkpoint.ID, s.instance.ID, s.instance.WriterGeneration)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := []CaptureAbortMember{}
	for rows.Next() {
		var m CaptureAbortMember
		var live bool
		if err := rows.Scan(&m.RunID, &m.AttemptNumber, &m.RunWaitID, &m.LeaseID, &m.LeaseSequence, &m.BaseComputerDiskVersionID, &m.ExpiresAt, &m.Cancelled, &live); err != nil {
			return nil, err
		}
		if !m.Cancelled && !live {
			return nil, pgx.ErrNoRows
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return members, nil
}

// CompleteCaptureAbort records the physical acknowledgment and the existing
// desired-version observation together. It is the sole transition back to hot
// waits and ordinary admission; a generic Instance observation cannot replace it.
func CompleteCaptureAbort(ctx context.Context, txb db.TxBeginner, ref CheckpointRef, abortVersion int64, cancelledLeases []uuid.UUID) (db.ComputerCheckpoint, error) {
	if abortVersion != ref.DesiredVersion+1 {
		return db.ComputerCheckpoint{}, ErrCheckpointCandidate
	}
	cancelled := make(map[string]bool, len(cancelledLeases))
	for _, id := range cancelledLeases {
		if id == uuid.Nil() || cancelled[id.String()] {
			return db.ComputerCheckpoint{}, ErrCheckpointCandidate
		}
		cancelled[id.String()] = true
	}
	var checkpoint db.ComputerCheckpoint
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		source, err := lockCheckpointSource(ctx, tx, ref)
		if err != nil {
			return err
		}
		cp := source.checkpoint
		if cp.Status != "aborted" || cp.AbortDesiredVersion.Int64 != abortVersion {
			return pgx.ErrNoRows
		}
		if cp.AbortAcknowledgedAt.Valid {
			checkpoint = cp
			return nil
		}
		if err := source.checkAbortSource(ctx, abortVersion, "resuming_capture"); err != nil {
			return err
		}
		members, err := source.abortMembers(ctx)
		if err != nil {
			return err
		}
		// The guest acknowledgment must cover cancellations that committed
		// after grant issuance. A changed disposition requires reinstallation
		// and ordinary member cleanup before this barrier can open.
		remaining := len(cancelled)
		for _, m := range members {
			if m.Cancelled != cancelled[m.LeaseID] {
				return pgx.ErrNoRows
			}
			if m.Cancelled {
				remaining--
			}
		}
		if remaining != 0 {
			return pgx.ErrNoRows
		}
		for _, m := range members {
			if m.Cancelled {
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE run_leases SET status='running',updated_at=clock_timestamp() WHERE id=$1`, m.LeaseID); err != nil {
				return err
			}
			result, err := tx.Exec(ctx, `WITH resumed_run AS (
 UPDATE runs r SET status=CASE WHEN w.condition_status='pending' THEN 'waiting' ELSE 'running' END,
 revision=r.revision+CASE WHEN w.condition_status='pending' THEN 0 ELSE 1 END,updated_at=clock_timestamp()
 FROM run_waits w WHERE w.id=$1 AND r.id=w.run_id AND r.status='waiting'
 AND r.revision=w.expected_run_revision AND r.current_run_lease_id=w.current_run_lease_id
 RETURNING r.id,r.revision
)
UPDATE run_waits w SET suspension_status=CASE WHEN w.condition_status='pending' THEN 'hot' ELSE 'released' END,
 expected_run_revision=resumed_run.revision,suspend_checkpoint_id=NULL,
 suspension_terminal_at=CASE WHEN w.condition_status<>'pending' THEN clock_timestamp() END,updated_at=clock_timestamp()
FROM resumed_run WHERE w.id=$1 AND w.run_id=resumed_run.id`, m.RunWaitID)
			if err != nil {
				return err
			}
			if result.RowsAffected() != 1 {
				return pgx.ErrNoRows
			}
		}
		result, err := tx.Exec(ctx, `UPDATE computer_instances i SET admission_state=CASE WHEN h.status='draining' THEN 'draining' ELSE 'open' END,capture_checkpoint_id=NULL,observed_desired_version=i.desired_version,observed_at=clock_timestamp(),updated_at=clock_timestamp() FROM worker_hosts h WHERE i.id=$1 AND h.id=i.worker_host_id AND h.current_epoch=i.worker_epoch`, source.instance.ID)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return pgx.ErrNoRows
		}
		if _, err := tx.Exec(ctx, `UPDATE computer_checkpoints SET abort_acknowledged_at=clock_timestamp() WHERE id=$1`, cp.ID); err != nil {
			return err
		}
		checkpoint, err = db.New(tx).LockComputerCheckpoint(ctx, db.LockComputerCheckpointParams{EnvironmentID: cp.EnvironmentID, ComputerID: cp.ComputerID, CheckpointID: cp.ID})
		return err
	})
	return checkpoint, authorityChanged(err)
}
