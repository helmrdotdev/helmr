package computer

import (
	"context"
	"encoding/json"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/jackc/pgx/v5"
)

// FailCheckpoint invalidates the whole capture candidate and requests the
// source Instance's exclusion, in one transaction under the checkpoint source
// fence. It does not certify that processes stopped, settle individual Runs,
// release the writer or reopen admission. The message is trimmed and must
// hold 1 to 1024 bytes, otherwise it reports ErrCheckpointCandidate before
// any database access. An exact committed failure replays. A fence that no
// longer holds, or a failure that differs from the committed one, reports
// ErrAuthorityChanged.
func FailCheckpoint(ctx context.Context, txb db.TxBeginner, ref CheckpointRef, message string) (db.ComputerCheckpoint, error) {
	message = strings.TrimSpace(message)
	if message == "" || len(message) > 1024 {
		return db.ComputerCheckpoint{}, ErrCheckpointCandidate
	}
	fingerprint, err := failedCheckpointFingerprint(ref, message)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	var checkpoint db.ComputerCheckpoint
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		source, err := lockCheckpointSource(ctx, tx, ref)
		if err != nil {
			return err
		}
		checkpoint, err = source.fail(ctx, ref, message, fingerprint)
		return err
	})
	if err != nil {
		return db.ComputerCheckpoint{}, authorityChanged(err)
	}
	return checkpoint, nil
}

// failedCheckpointReceipt is the persisted identity of a failure report. Its
// field names, tags and order are the stored receipt fingerprint's format.
type failedCheckpointReceipt struct {
	ComputerInstanceID string `json:"computer_instance_id"`
	WorkerEpoch        int64  `json:"worker_epoch"`
	DesiredVersion     int64  `json:"desired_version"`
	CheckpointID       string `json:"checkpoint_id"`
	Error              string `json:"error"`
}

// failedCheckpointFingerprint is the receipt fingerprint of a failure report
// with a trimmed message.
func failedCheckpointFingerprint(ref CheckpointRef, message string) (string, error) {
	encoded, err := json.Marshal(failedCheckpointReceipt{ComputerInstanceID: ref.InstanceID.String(), WorkerEpoch: ref.WorkerEpoch, DesiredVersion: ref.DesiredVersion, CheckpointID: ref.CheckpointID.String(), Error: message})
	if err != nil {
		return "", err
	}
	return sha256sum.DigestBytes(append([]byte("computer.checkpoint.failed\x00"), encoded...)), nil
}

// fail invalidates the creating checkpoint of the current source incarnation,
// marks the Computer's state lost to capture and requests the Instance's
// close with discard.
func (s checkpointSource) fail(ctx context.Context, ref CheckpointRef, message, fingerprint string) (db.ComputerCheckpoint, error) {
	instance, checkpoint := s.instance, s.checkpoint
	if checkpoint.Status == "invalid" && checkpoint.InvalidationReasonCode.String == "checkpoint_failed" && checkpoint.FailedRequestFingerprint.Valid && checkpoint.FailedRequestFingerprint.String == fingerprint {
		return checkpoint, nil
	}
	if checkpoint.Status != "creating" || checkpoint.SourceComputerInstanceID != instance.ID || checkpoint.WriterGeneration != instance.WriterGeneration || checkpoint.MembershipRevision != instance.MembershipRevision || checkpoint.ProgramDeploymentID != instance.ProgramDeploymentID || checkpoint.ComputerSpecID != instance.ComputerSpecID || s.computer.WriterGeneration != instance.WriterGeneration || instance.DesiredState != "ready" || instance.DesiredVersion != ref.DesiredVersion || instance.AdmissionState != "checkpointing" || instance.ReclaimedAt.Valid {
		return db.ComputerCheckpoint{}, pgx.ErrNoRows
	}
	// Expired writer/member deadlines must not prevent the current host from
	// reporting failure. Physical exclusion still precedes any replacement writer.
	q := db.New(s.tx)
	checkpoint, err := q.InvalidateFailedComputerCheckpoint(ctx, db.InvalidateFailedComputerCheckpointParams{EnvironmentID: instance.EnvironmentID, CheckpointID: checkpoint.ID, FailedRequestFingerprint: pgvalue.Text(fingerprint)})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	failure, err := json.Marshal(struct {
		Message string `json:"message"`
	}{message})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	// The last committed head may predate accepted work on this source. Once
	// capture fails, exclude it without admitting work against that older head.
	_, err = s.tx.Exec(ctx, `UPDATE computers SET desired_state='stopped',dirty_state='capture_failed',
 recovery_id=CASE WHEN recovery_completed_at IS NULL THEN coalesce(recovery_id,$2) ELSE $2 END,
 recovery_disk_version_id=head_disk_version_id,recovery_reason='computer_capture_failed',
 recovery_started_at=CASE WHEN recovery_completed_at IS NULL THEN coalesce(recovery_started_at,clock_timestamp()) ELSE clock_timestamp() END,
 recovery_completed_at=NULL,recovery_failure=coalesce(recovery_failure,jsonb_build_object('code','computer_capture_failed','message','Computer state could not be preserved','details',$3::jsonb)),
 revision=revision+1,updated_at=clock_timestamp()
 WHERE id=$1 AND status NOT IN ('deleting','deleted') AND desired_state<>'deleted'`, instance.ComputerID, pgvalue.UUID(uuid.NewV7()), failure)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	_, err = q.RequestComputerInstanceClose(ctx, db.RequestComputerInstanceCloseParams{ID: instance.ID, WriterGeneration: instance.WriterGeneration, Reason: "checkpoint_failed", FinalizationAction: pgvalue.Text("discard"), Error: failure})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	return checkpoint, nil
}
