package dispatch

import (
	"context"
	"encoding/json"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

// FailComputerCheckpoint invalidates the whole candidate and requests source
// exclusion. The caller owns commit/rollback. It does not certify that processes
// stopped, settle individual Runs, release the writer, or reopen admission.
func FailComputerCheckpoint(ctx context.Context, tx pgx.Tx, worker ComputerCaptureWorker, request workerapi.CheckpointFailedRequest) (db.ComputerCheckpoint, error) {
	request.Error = strings.TrimSpace(request.Error)
	if request.Error == "" || len(request.Error) > 1024 {
		return db.ComputerCheckpoint{}, ErrCheckpointCandidate
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	fingerprint := sha256sum.DigestBytes(append([]byte("computer.checkpoint.failed\x00"), encoded...))
	source, err := lockComputerCheckpointSource(ctx, tx, worker, computerCheckpointFence{request.ComputerInstanceID, request.WorkerEpoch, request.DesiredVersion, request.CheckpointID})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	instance, checkpoint := source.Instance(), source.Checkpoint()
	if checkpoint.Status == "invalid" && checkpoint.InvalidationReasonCode.String == "checkpoint_failed" && checkpoint.FailedRequestFingerprint.Valid && checkpoint.FailedRequestFingerprint.String == fingerprint {
		return checkpoint, nil
	}
	if checkpoint.Status != "creating" || checkpoint.SourceComputerInstanceID != instance.ID || checkpoint.WriterGeneration != instance.WriterGeneration || checkpoint.MembershipRevision != instance.MembershipRevision || checkpoint.ProgramDeploymentID != instance.ProgramDeploymentID || checkpoint.ComputerSpecID != instance.ComputerSpecID || source.Computer().WriterGeneration != instance.WriterGeneration || instance.DesiredState != "ready" || instance.DesiredVersion != request.DesiredVersion || instance.AdmissionState != "checkpointing" || instance.ReclaimedAt.Valid {
		return db.ComputerCheckpoint{}, pgx.ErrNoRows
	}
	// Expired writer/member deadlines must not prevent the current host from
	// reporting failure. Physical exclusion still precedes any replacement writer.
	q := db.New(tx)
	checkpoint, err = q.InvalidateFailedComputerCheckpoint(ctx, db.InvalidateFailedComputerCheckpointParams{EnvironmentID: instance.EnvironmentID, CheckpointID: checkpoint.ID, FailedRequestFingerprint: pgvalue.Text(fingerprint)})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	failure, err := json.Marshal(struct {
		Message string `json:"message"`
	}{request.Error})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	// The last committed head may predate accepted work on this source. Once
	// capture fails, exclude it without admitting work against that older head.
	_, err = tx.Exec(ctx, `UPDATE computers SET desired_state='stopped',dirty_state='capture_failed',
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
