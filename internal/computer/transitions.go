package computer

import (
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/db"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
)

// Focused row transitions for Session and dispatch operations. Each runs in
// the caller's transaction after the caller has taken the locks its operation
// needs in the global order; none of them takes a lock on its own beyond the
// rows it writes.

// AssignImageBuild places a queued image build on an allocation and reports
// whether the build was still assignable.
func AssignImageBuild(ctx context.Context, tx pgx.Tx, a Allocation, spec uuid.UUID) (bool, error) {
	changed, err := tx.Exec(ctx, `UPDATE image_builds SET status='running',executor_epoch=1,worker_host_id=$3,worker_epoch=$4,allocation_id=$5,channel_credential_digest=$6,
 reserved_cpu_millis=$7,reserved_memory_bytes=$8,reserved_scratch_bytes=$9,vm_platform_id=$10,vm_vcpu_count=$11,cpu_config_digest=$12
 WHERE environment_id=$1 AND id=$2 AND status='queued' AND deadline_at>clock_timestamp() AND NOT EXISTS(
 SELECT 1 FROM secret_bindings binding JOIN secrets s ON s.environment_id=binding.environment_id AND s.id=binding.secret_id
 WHERE binding.environment_id=$1 AND binding.image_spec_id=$13 AND s.status='revoked')`, a.EnvironmentID, a.OwnerID, a.HostID, a.HostEpoch, a.InstanceID, a.CredentialDigest, a.Shape.CPUMillis, a.Shape.MemoryBytes, a.Shape.ScratchBytes, a.Shape.VMPlatformID, a.Shape.VCPUCount, a.Shape.CPUConfigDigest, spec)
	if err != nil {
		return false, err
	}
	if changed.RowsAffected() == 0 {
		return false, nil
	}
	return true, notifyImageBuildChanged(ctx, tx, a.EnvironmentID, a.OwnerID)
}

// RecordImageBuildProxyTrust stores the attempt-scoped proxy CA of a build.
func RecordImageBuildProxyTrust(ctx context.Context, tx pgx.Tx, env, build uuid.UUID, trust secret.ProxyTrust) error {
	_, err := tx.Exec(ctx, `UPDATE image_builds SET proxy_ca_certificate=$3,proxy_ca_private_key_nonce=$4,proxy_ca_private_key_ciphertext=$5,proxy_ca_not_after=$6 WHERE environment_id=$1 AND id=$2`, env, build, trust.Certificate, trust.PrivateKeyNonce, trust.PrivateKeyCiphertext, trust.NotAfter)
	return err
}

// DeliverImageBuild records the first delivery of a running build to its
// executor. It returns pgx.ErrNoRows when the build no longer admits
// execution.
func DeliverImageBuild(ctx context.Context, tx pgx.Tx, env, build, spec uuid.UUID) (delivered, expires *time.Time, err error) {
	err = tx.QueryRow(ctx, `UPDATE image_builds p SET delivered_at=COALESCE(delivered_at,clock_timestamp()),
 executor_expires_at=COALESCE(executor_expires_at,LEAST(deadline_at,clock_timestamp()+interval '1 minute'))
 WHERE environment_id=$1 AND id=$2 AND status='running' AND fenced_at IS NULL AND deadline_at>clock_timestamp()
 AND (delivered_at IS NULL OR executor_expires_at>clock_timestamp())
 AND NOT EXISTS(SELECT 1 FROM secret_bindings binding JOIN secrets s ON s.environment_id=binding.environment_id AND s.id=binding.secret_id
 WHERE binding.environment_id=$1 AND binding.image_spec_id=$3 AND s.status='revoked')
 RETURNING delivered_at,executor_expires_at`, env, build, spec).Scan(&delivered, &expires)
	return delivered, expires, err
}

// TerminateImageBuild ends a build with an error code and wipes its proxy CA.
func TerminateImageBuild(ctx context.Context, tx pgx.Tx, env, build uuid.UUID, code string) error {
	_, err := tx.Exec(ctx, `UPDATE image_builds SET proxy_ca_certificate=NULL,proxy_ca_private_key_nonce=NULL,proxy_ca_private_key_ciphertext=NULL,proxy_ca_not_after=NULL,status='failed',error_code=$3 WHERE environment_id=$1 AND id=$2`, env, build, code)
	if err != nil {
		return err
	}
	return notifyImageBuildChanged(ctx, tx, env, build)
}

// FailRevokedImageBuild ends a live build that has exposed a revoked Secret.
func FailRevokedImageBuild(ctx context.Context, tx pgx.Tx, env, build uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE image_builds p SET proxy_ca_certificate=NULL,proxy_ca_private_key_nonce=NULL,proxy_ca_private_key_ciphertext=NULL,proxy_ca_not_after=NULL,status='failed',error_code='secret_revoked'
   WHERE p.environment_id=$1 AND p.id=$2 AND p.status IN ('queued','running') AND EXISTS(
    SELECT 1 FROM secret_exposures x JOIN secrets s ON (s.environment_id,s.id)=(x.environment_id,x.secret_id)
    WHERE x.environment_id=p.environment_id AND x.image_build_id=p.id AND s.status='revoked')`, env, build)
	if err != nil {
		return err
	}
	return notifyImageBuildChanged(ctx, tx, env, build)
}

// InsertLease records a new lease allocation in the acquiring state.
func InsertLease(ctx context.Context, tx pgx.Tx, r ComputerAllocation) error {
	var expired bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(execution_deadline_at<=clock_timestamp(),false) FROM computers WHERE environment_id=$1 AND id=$2`, r.EnvironmentID, r.OwnerID).Scan(&expired); err != nil {
		return err
	}
	if expired {
		return ErrNotReady
	}

	_, err := tx.Exec(ctx, `INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,allocation_id,channel_credential_digest,status,
 reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,restored_from_save_id,disk_only_recovery)
 VALUES($1,$2,$3,$4,$5,$6,$7,'acquiring',$8,$9,$10,$11,$12,$13,$14,$15)`, r.EnvironmentID, r.OwnerID, r.Epoch, r.HostID, r.HostEpoch, r.InstanceID, r.CredentialDigest, r.Shape.CPUMillis, r.Shape.MemoryBytes, r.Shape.ScratchBytes, r.Shape.VMPlatformID, r.Shape.VCPUCount, r.Shape.CPUConfigDigest, r.RestoredFrom, r.DiskOnlyRecovery)
	if err != nil {
		return err
	}
	return notifyComputerChanged(ctx, tx, r.EnvironmentID, r.OwnerID)
}

// DeliverLease records the first delivery of a lease to its host. It returns
// pgx.ErrNoRows when the lease no longer admits execution.
func DeliverLease(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID, epoch int64) (delivered, expires *time.Time, err error) {
	err = tx.QueryRow(ctx, `UPDATE computer_leases SET delivered_at=COALESCE(delivered_at,clock_timestamp()),expires_at=COALESCE(expires_at,clock_timestamp()+interval '1 minute')
 WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND status IN ('acquiring','active') AND fenced_at IS NULL
 AND (delivered_at IS NULL OR expires_at>clock_timestamp())
 AND NOT EXISTS(SELECT 1 FROM computers c WHERE c.environment_id=$1 AND c.id=$2 AND c.execution_deadline_at<=clock_timestamp()) RETURNING delivered_at,expires_at`, env, computer, epoch).Scan(&delivered, &expires)
	return delivered, expires, err
}

// InitializeLease records that the guest of a delivered, live lease reported
// ready, and reports whether the lease was still live.
func InitializeLease(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID, epoch int64) (bool, error) {
	changed, err := tx.Exec(ctx, `UPDATE computer_leases SET status='active',initialized_at=COALESCE(initialized_at,clock_timestamp())
 WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND status IN ('acquiring','active') AND delivered_at IS NOT NULL AND expires_at>clock_timestamp() AND fenced_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM computers c WHERE c.environment_id=$1 AND c.id=$2 AND c.execution_deadline_at<=clock_timestamp())`, env, computer, epoch)
	if err != nil {
		return false, err
	}
	if changed.RowsAffected() == 0 {
		return false, nil
	}
	if err := completeExecutionAdmission(ctx, tx, env, computer); err != nil {
		return false, err
	}
	return true, notifyComputerChanged(ctx, tx, env, computer)
}

// ActivateRestoredLease records the restore target lease as active once the
// restore has committed.
func ActivateRestoredLease(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID, epoch int64) error {
	tag, err := tx.Exec(ctx, `UPDATE computer_leases SET status='active',initialized_at=COALESCE(initialized_at,clock_timestamp()) WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3
 AND NOT EXISTS(SELECT 1 FROM computers c WHERE c.environment_id=$1 AND c.id=$2 AND c.execution_deadline_at<=clock_timestamp())`, env, computer, epoch)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotReady
	}
	if err = completeExecutionAdmission(ctx, tx, env, computer); err != nil {
		return err
	}
	return notifyComputerChanged(ctx, tx, env, computer)
}

func completeExecutionAdmission(ctx context.Context, tx pgx.Tx, env, id uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE computers SET execution_pending_since=NULL,execution_deadline_at=NULL WHERE environment_id=$1 AND id=$2`, env, id)
	return err
}

// MarkLeaseLost ends an unfenced lease whose physical owner is gone; only
// physical proof may fence it later.
func MarkLeaseLost(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID, epoch int64) error {
	_, err := tx.Exec(ctx, `UPDATE computer_leases SET status='lost' WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, env, computer, epoch)
	if err != nil {
		return err
	}
	return notifyComputerChanged(ctx, tx, env, computer)
}

// FenceLease records physical proof that the lease's VM has stopped.
func FenceLease(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID, epoch int64, evidence string) error {
	var host uuid.UUID
	err := tx.QueryRow(ctx, `SELECT worker_host_id FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, env, computer, epoch).Scan(&host)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence=$4 WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, env, computer, epoch, evidence)
	if err != nil {
		return err
	}
	// Recovery of already accepted work starts its own bounded pending interval.
	if _, err = tx.Exec(ctx, `UPDATE computers c SET execution_pending_since=clock_timestamp(),execution_deadline_at=clock_timestamp()+(SELECT preparation_timeout_ms FROM environments WHERE id=$1)*interval '1 millisecond'
 WHERE c.environment_id=$1 AND c.id=$2 AND c.execution_pending_since IS NULL
 AND NOT EXISTS(SELECT 1 FROM checkpoints cp WHERE cp.environment_id=c.environment_id AND cp.computer_id=c.id AND cp.status='ready')
 AND (EXISTS(SELECT 1 FROM turns t JOIN sessions s ON (s.environment_id,s.id)=(t.environment_id,t.session_id) WHERE t.environment_id=c.environment_id AND t.computer_id=c.id AND t.status='queued' AND `+db.NoSessionHoldsSQL+`) OR EXISTS(SELECT 1 FROM commands cmd WHERE cmd.environment_id=c.environment_id AND cmd.computer_id=c.id AND cmd.status='pending'))`, env, computer); err != nil {
		return err
	}
	if err := RetireUnusedExecutionAdmission(ctx, tx, env, computer); err != nil {
		return err
	}
	return dbnotify.Notify(ctx, tx, "dispatch", "capacity", "worker:"+host.String())
}

// LoseLeaseCheckpoints settles the unfinished checkpoints owned by a lease
// whose physical owner closed without an eligible restore.
func LoseLeaseCheckpoints(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID, epoch int64) error {
	_, err := tx.Exec(ctx, `UPDATE checkpoints SET status='lost',terminal_evidence='physical owner unavailable without an eligible restore',capture_request=NULL
 WHERE environment_id=$1 AND computer_id=$2 AND COALESCE(target_lease_epoch,source_lease_epoch)=$3
 AND status IN ('capturing','sealed','aborting','consumed') AND controls_reconciled_at IS NULL`, env, computer, epoch)
	return err
}

// LoseInvalidCheckpoint records that the owning worker proved the
// checkpoint's contents invalid after physical cleanup.
func LoseInvalidCheckpoint(ctx context.Context, tx pgx.Tx, env, checkpoint uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE checkpoints SET status='lost',terminal_evidence='owning worker proved checkpoint contents invalid after physical cleanup',capture_request=NULL WHERE environment_id=$1 AND id=$2`, env, checkpoint)
	return err
}

// ReserveCapture advances the Computer's control version and save sequence
// for a new capture and returns the values it reserved.
func ReserveCapture(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID) (version, saveSeq int64, err error) {
	err = tx.QueryRow(ctx, `UPDATE computers SET next_control_version=next_control_version+1,next_save_seq=next_save_seq+1
   WHERE environment_id=$1 AND id=$2 AND integrity_fault_at IS NULL AND deleted_at IS NULL
   RETURNING next_control_version-1,next_save_seq-1`, env, computer).Scan(&version, &saveSeq)
	return version, saveSeq, err
}

// CheckpointMember is one Session process captured by a checkpoint.
type CheckpointMember struct {
	Session uuid.UUID
	Epoch   int64
}

// CheckpointStart is a capture's checkpoint, its disk save and its members.
type CheckpointStart struct {
	EnvironmentID, ComputerID, CheckpointID, SaveID uuid.UUID
	LeaseEpoch, ControlVersion, SaveSeq             int64
	Request, RequestDigest                          []byte
	Expires                                         time.Time
	Members                                         []CheckpointMember
}

// CreateCheckpoint records a capturing checkpoint with its requested disk
// save and captured members.
func CreateCheckpoint(ctx context.Context, tx pgx.Tx, c CheckpointStart) error {
	if _, err := tx.Exec(ctx, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq) VALUES($1,$2,$3,$4,$5)`, c.EnvironmentID, c.SaveID, c.ComputerID, c.LeaseEpoch, c.SaveSeq); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO checkpoints(environment_id,id,computer_id,source_lease_epoch,control_version,disk_save_id,status,capture_request,capture_expires_at,capture_digest)
   VALUES($1,$2,$3,$4,$5,$6,'capturing',$7,$8,$9)`, c.EnvironmentID, c.CheckpointID, c.ComputerID, c.LeaseEpoch, c.ControlVersion, c.SaveID, c.Request, c.Expires, c.RequestDigest); err != nil {
		return err
	}
	for _, m := range c.Members {
		if _, err := tx.Exec(ctx, `INSERT INTO checkpoint_processes(environment_id,computer_id,checkpoint_id,session_id,process_epoch) VALUES($1,$2,$3,$4,$5)`, c.EnvironmentID, c.ComputerID, c.CheckpointID, m.Session, m.Epoch); err != nil {
			return err
		}
	}
	if err := BindHibernateCapture(ctx, tx, c.EnvironmentID, c.ComputerID, c.CheckpointID); err != nil {
		return err
	}
	return notifyComputerChanged(ctx, tx, c.EnvironmentID, c.ComputerID)
}

// SealCheckpoint records that every captured member is frozen.
func SealCheckpoint(ctx context.Context, tx pgx.Tx, env, checkpoint uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE checkpoints SET status='sealed' WHERE environment_id=$1 AND id=$2`, env, checkpoint)
	return err
}

// CancelUnsealedCheckpoint cancels a capture that never sealed, together with
// its still-requested disk save. It returns ErrConflict when the save already
// has a recorded cut.
func CancelUnsealedCheckpoint(ctx context.Context, tx pgx.Tx, env, checkpoint, save uuid.UUID, reason string) error {
	tag, err := tx.Exec(ctx, `UPDATE computer_saves SET status='failed',failure_evidence=$3 WHERE environment_id=$1 AND id=$2 AND status='requested'`, env, save, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	_, err = tx.Exec(ctx, `UPDATE checkpoints SET status='cancelled',terminal_evidence=$3,capture_request=NULL WHERE environment_id=$1 AND id=$2`, env, checkpoint, reason)
	if err != nil {
		return err
	}
	if err := settleHibernateCapture(ctx, tx, env, checkpoint, "capture_refused"); err != nil {
		return err
	}
	return notifySaveChanged(ctx, tx, env, save)
}

// BeginRestore advances the Computer's control version and records the
// checkpoint as restoring onto the target lease epoch. It returns the
// restore's control version.
func BeginRestore(ctx context.Context, tx pgx.Tx, env, computer, checkpoint uuid.UUID, epoch int64) (int64, error) {
	var next int64
	if err := tx.QueryRow(ctx, `UPDATE computers SET next_control_version=next_control_version+1 WHERE environment_id=$1 AND id=$2 RETURNING next_control_version-1`, env, computer).Scan(&next); err != nil {
		return 0, err
	}
	_, err := tx.Exec(ctx, `UPDATE checkpoints SET status='restoring',target_lease_epoch=$3,restore_control_version=$4,restore_identity=NULL WHERE environment_id=$1 AND id=$2`, env, checkpoint, epoch, next)
	return next, err
}

// RecordRestoreIdentity stores the validated identity of a restore.
func RecordRestoreIdentity(ctx context.Context, tx pgx.Tx, env, checkpoint uuid.UUID, identity []byte) error {
	_, err := tx.Exec(ctx, `UPDATE checkpoints SET restore_identity=$3 WHERE environment_id=$1 AND id=$2`, env, checkpoint, identity)
	return err
}

// BeginSourceAbort advances the Computer's control version from the expected
// value and records the checkpoint as aborting. It returns ErrConflict when
// the version moved.
func BeginSourceAbort(ctx context.Context, tx pgx.Tx, env, computer, checkpoint uuid.UUID, expectedVersion int64, identity []byte) error {
	tag, err := tx.Exec(ctx, `UPDATE computers SET next_control_version=next_control_version+1 WHERE environment_id=$1 AND id=$2 AND next_control_version=$3 AND integrity_fault_at IS NULL AND deleted_at IS NULL`, env, computer, expectedVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	_, err = tx.Exec(ctx, `UPDATE checkpoints SET status='aborting',abort_identity=$3 WHERE environment_id=$1 AND id=$2`, env, checkpoint, identity)
	return err
}

// RecordAbortIdentity stores the validated identity of a source abort.
func RecordAbortIdentity(ctx context.Context, tx pgx.Tx, env, checkpoint uuid.UUID, identity []byte) error {
	_, err := tx.Exec(ctx, `UPDATE checkpoints SET abort_identity=$3 WHERE environment_id=$1 AND id=$2`, env, checkpoint, identity)
	return err
}

// ConsumeCheckpoint records that a restore or source abort used the
// checkpoint.
func ConsumeCheckpoint(ctx context.Context, tx pgx.Tx, env, checkpoint uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE checkpoints SET status='consumed' WHERE environment_id=$1 AND id=$2`, env, checkpoint)
	return err
}

// ReconcileCheckpointControls records that the members' controls match the
// consumed checkpoint and drops its capture request.
func ReconcileCheckpointControls(ctx context.Context, tx pgx.Tx, env, checkpoint uuid.UUID) error {
	tag, err := tx.Exec(ctx, `UPDATE computers SET execution_pending_since=NULL,execution_deadline_at=NULL WHERE environment_id=$1 AND id=(SELECT computer_id FROM checkpoints WHERE environment_id=$1 AND id=$2) AND (execution_deadline_at IS NULL OR execution_deadline_at>clock_timestamp())`, env, checkpoint)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotReady
	}

	if _, err := tx.Exec(ctx, `UPDATE checkpoints SET controls_reconciled_at=clock_timestamp(),capture_request=NULL WHERE environment_id=$1 AND id=$2`, env, checkpoint); err != nil {
		return err
	}
	if err := settleHibernateCapture(ctx, tx, env, checkpoint, "capture_resumed"); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE computer_controls SET status='completed',settled_at=clock_timestamp() WHERE environment_id=$1 AND computer_id=(SELECT computer_id FROM checkpoints WHERE environment_id=$1 AND id=$2) AND kind='wake' AND status='accepted'`, env, checkpoint)
	return err
}

// FailSave records why a save can no longer publish.
func FailSave(ctx context.Context, tx pgx.Tx, env, save uuid.UUID, evidence string) error {
	_, err := tx.Exec(ctx, `UPDATE computer_saves SET status='failed',failure_evidence=$3 WHERE environment_id=$1 AND id=$2`, env, save, evidence)
	if err != nil {
		return err
	}
	return notifySaveChanged(ctx, tx, env, save)
}

// RequestSave reserves the next save sequence of the Computer and records a
// save requested by a Turn's finalization.
func RequestSave(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID, epoch int64, turn uuid.UUID) (SaveRequest, error) {
	save := SaveRequest{ID: uuid.NewV7(), ComputerID: computer, LeaseEpoch: epoch}
	if err := tx.QueryRow(ctx, `UPDATE computers SET next_save_seq=next_save_seq+1 WHERE environment_id=$1 AND id=$2 RETURNING next_save_seq-1`, env, computer).Scan(&save.Sequence); err != nil {
		return SaveRequest{}, err
	}
	err := tx.QueryRow(ctx, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq,turn_id) VALUES($1,$2,$3,$4,$5,$6) RETURNING requested_at`, env, save.ID, computer, epoch, save.Sequence, turn).Scan(&save.RequestedAt)
	if err != nil {
		return save, err
	}
	return save, notifyComputerReaders(ctx, tx, env, computer)
}

// MarkPreparationFailed records that a Computer's image never became ready
// before its deadline.
func MarkPreparationFailed(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE computers SET preparation_failed_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, env, computer)
	return err
}

// MarkUsed clears the Computer's unused clock when work is admitted to it.
func MarkUsed(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE computers SET unused_since=NULL WHERE environment_id=$1 AND id=$2`, env, computer)
	return err
}

// RecordSessionExposures records the current versions of the active Secrets
// exposed to a Session process.
func RecordSessionExposures(ctx context.Context, tx pgx.Tx, env, session uuid.UUID, epoch int64, secrets []uuid.UUID) error {
	_, err := tx.Exec(ctx, `INSERT INTO secret_exposures(id,environment_id,session_id,process_epoch,secret_id,version_id,revocation_generation)
   SELECT uuidv7(),$1,$2,$3,s.id,s.current_version_id,s.revocation_generation FROM secrets s WHERE s.environment_id=$1 AND s.id=ANY($4::uuid[]) AND s.status='active'
   ON CONFLICT(environment_id,session_id,process_epoch,secret_id) WHERE session_id IS NOT NULL DO NOTHING`, env, session, epoch, secrets)
	return err
}

// RecordCommandExposures records the current versions of the active Secrets
// exposed to a Command.
func RecordCommandExposures(ctx context.Context, tx pgx.Tx, env, command uuid.UUID, secrets []uuid.UUID) error {
	_, err := tx.Exec(ctx, `INSERT INTO secret_exposures(id,environment_id,command_id,secret_id,version_id,revocation_generation)
 SELECT uuidv7(),$1,$2,s.id,s.current_version_id,s.revocation_generation FROM secrets s WHERE s.environment_id=$1 AND s.id=ANY($3::uuid[]) AND s.status='active'
 ON CONFLICT(environment_id,command_id,secret_id) WHERE command_id IS NOT NULL DO NOTHING`, env, command, secrets)
	return err
}

// ImageSpecRecord is one image spec registered by a Deployment.
type ImageSpecRecord struct {
	ID     uuid.UUID       `json:"id"`
	Digest string          `json:"digest"`
	Spec   json.RawMessage `json:"spec"`
	Seed   json.RawMessage `json:"seed"`
}

// RecordImageSpecs records image specs by digest; a spec already registered
// under the same digest keeps its identity.
func RecordImageSpecs(ctx context.Context, tx pgx.Tx, env uuid.UUID, specs []ImageSpecRecord) error {
	raw, err := json.Marshal(specs)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO image_specs(environment_id,id,spec_digest,spec,seed)
 SELECT $1,v.id,v.digest,v.spec,v.seed FROM jsonb_to_recordset($2::jsonb) AS v(id uuid,digest text,spec jsonb,seed jsonb)
 ON CONFLICT(environment_id,spec_digest) DO NOTHING`, env, raw)
	return err
}

// ApplyImageRefreshPolicy sets each image spec's refresh interval to the
// shortest one the promoted Deployment's templates request, clearing it for
// specs no template refreshes. A spec whose interval is unchanged keeps its
// next refresh; a changed one refreshes no later than one interval after the
// cutover.
func ApplyImageRefreshPolicy(ctx context.Context, tx pgx.Tx, env, deployment uuid.UUID, cutover time.Time) error {
	_, err := tx.Exec(ctx, `WITH requested AS (
 SELECT image_spec_id,min(refresh_every_ms) AS every_ms FROM template_definitions
 WHERE environment_id=$1 AND deployment_id=$2 AND refresh_every_ms IS NOT NULL GROUP BY image_spec_id
 ) , affected AS (
 SELECT id FROM image_specs WHERE environment_id=$1 AND refresh_every_ms IS NOT NULL
 UNION SELECT image_spec_id FROM requested
 ), policy AS (
 SELECT a.id,r.every_ms FROM affected a LEFT JOIN requested r ON r.image_spec_id=a.id
 ) UPDATE image_specs s SET refresh_every_ms=p.every_ms,
 next_refresh_at=CASE WHEN p.every_ms IS NULL THEN NULL
 WHEN s.refresh_every_ms=p.every_ms THEN s.next_refresh_at
 ELSE LEAST(s.next_refresh_at,$3::timestamptz+p.every_ms*interval '1 millisecond') END
 FROM policy p WHERE s.environment_id=$1 AND s.id=p.id AND s.refresh_every_ms IS DISTINCT FROM p.every_ms`, env, deployment, cutover)
	return err
}
