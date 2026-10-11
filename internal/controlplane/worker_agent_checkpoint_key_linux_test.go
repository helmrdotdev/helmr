//go:build linux && computerproof

package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgxpool"
)

type nativeCheckpointKeyFault struct {
	CheckpointID     uuid.UUID `json:"checkpointId"`
	ComputerID       uuid.UUID `json:"computerId"`
	WorkerHostID     uuid.UUID `json:"workerHostId"`
	WorkerEpoch      int64     `json:"workerEpoch"`
	FailedLeaseCount int64     `json:"failedLeaseCount"`
	LastFailureEpoch int64     `json:"lastFailureEpoch"`
	Ready            bool      `json:"ready"`
	PausedMS         float64   `json:"pausedMs,omitempty"`
	Repaired         *struct {
		CheckpointID      uuid.UUID `json:"checkpointId"`
		WorkerEpoch       int64     `json:"workerEpoch"`
		WrongWorkerJoined bool      `json:"wrongWorkerJoined"`
	} `json:"repaired,omitempty"`
}

// Replace only the fixture's owned Worker processes. The ordinary source fence,
// admission latch, allocation cleanup and restoration remain production behavior.
func qualifyNativeCheckpointKeyMismatch(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, input nativeExecutionConfig, observations string, source *nativeWorkerProcess, expectedSourceStop *atomic.Bool, environment map[string]string, stopDriver context.CancelCauseFunc) (resultErr error) {
	var checkpoint uuid.UUID
	var active *nativeWorkerProcess
	defer func() {
		if errors.Is(resultErr, context.Canceled) && ctx.Err() != nil {
			resultErr = nil
		}
		if resultErr != nil {
			stopDriver(resultErr)
		}
		if active != nil {
			// Keep the owned Worker available until outer fixture retirement finishes.
			<-ctx.Done()
			resultErr = errors.Join(resultErr, stopNativeKeyWorker(active, false))
		}
	}()
	awaitRequest := func(phase string) error {
		return awaitLossCondition(ctx, func() (bool, error) {
			if active != nil {
				select {
				case <-active.done:
					return false, fmt.Errorf("key qualification Worker exited: %v", active.err)
				default:
				}
			}
			raw, err := os.ReadFile(observations + ".key-" + phase)
			if errors.Is(err, os.ErrNotExist) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			var request struct {
				CheckpointID uuid.UUID `json:"checkpointId"`
			}
			if err := json.Unmarshal(raw, &request); err != nil {
				return false, err
			}
			if request.CheckpointID == uuid.Nil() || checkpoint != uuid.Nil() && request.CheckpointID != checkpoint {
				return false, errors.New("key qualification checkpoint changed")
			}
			checkpoint = request.CheckpointID
			return true, nil
		})
	}
	if err := awaitRequest("request"); err != nil {
		return err
	}
	var host, computer uuid.UUID
	var sourceWorkerEpoch int64
	err := pool.QueryRow(ctx, `SELECT l.worker_host_id,l.computer_id,l.worker_epoch
 FROM computer_checkpoints p JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.source_lease_epoch)
 JOIN worker_hosts h ON h.id=l.worker_host_id
 WHERE p.environment_id=$1 AND p.id=$2 AND p.status='ready' AND p.target_lease_epoch IS NULL
 AND l.fenced_at>=p.ready_at AND h.resource_id=$3`, env, checkpoint, environment["WORKER_RESOURCE_ID"]).Scan(&host, &computer, &sourceWorkerEpoch)
	if err != nil {
		return err
	}
	expectedSourceStop.Store(true)
	if err := stopNativeKeyWorker(source, true); err != nil {
		return err
	}
	wrong := maps.Clone(environment)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	wrong["CHECKPOINT_ENCRYPTION_KEY"] = base64.StdEncoding.EncodeToString(key)
	if wrong["CHECKPOINT_ENCRYPTION_KEY"] == environment["CHECKPOINT_ENCRYPTION_KEY"] {
		return errors.New("key qualification did not change the key")
	}
	active, err = startNativeWorker(input, wrong, "worker-wrong-key.log")
	if err != nil {
		return err
	}
	awaitActive := func(after int64) (int64, error) {
		var epoch int64
		err := awaitLossCondition(ctx, func() (bool, error) {
			select {
			case <-active.done:
				return false, fmt.Errorf("key qualification Worker exited before activation: %v", active.err)
			default:
			}
			var ready bool
			err := pool.QueryRow(ctx, `SELECT COALESCE(current_epoch,0),COALESCE(status='active' AND current_epoch>$2
 AND run_paused_reason IS NULL AND vm_paused_reason IS NULL AND observed_at>=clock_timestamp()-interval '5 seconds',false)
 FROM worker_hosts WHERE id=$1`, host, after).Scan(&epoch, &ready)
			return ready, err
		})
		return epoch, err
	}
	wrongEpoch, err := awaitActive(sourceWorkerEpoch)
	if err != nil {
		return err
	}
	if err := writeNativeHandoffFile(observations+".key-wrong-ready", map[string]any{"checkpointId": checkpoint, "workerEpoch": wrongEpoch, "sourceWorkerJoined": true}); err != nil {
		return err
	}

	readFault := func() (nativeCheckpointKeyFault, error) {
		var state nativeCheckpointKeyFault
		var raw []byte
		err := pool.QueryRow(ctx, `SELECT jsonb_build_object('checkpointId',p.id,'computerId',p.computer_id,'workerHostId',h.id,'workerEpoch',h.current_epoch,
 'failedLeaseCount',count(bad.epoch),'lastFailureEpoch',COALESCE(max(bad.epoch),0),
 'ready',p.status='ready' AND p.target_lease_epoch IS NULL AND count(bad.epoch)>0
 AND bool_and(bad.fenced_at IS NOT NULL AND bad.initialized_at IS NULL AND bad.fence_evidence='owning worker joined physical VM closure')
 AND h.observed_at>=clock_timestamp()-$4::bigint*interval '1 second' AND h.status='active' AND h.current_epoch=$3 AND h.run_paused_reason='checkpoint_key_unavailable' AND h.vm_paused_reason='checkpoint_key_unavailable'
 AND NOT EXISTS(SELECT 1 FROM computer_leases live WHERE live.environment_id=p.environment_id AND live.computer_id=p.computer_id AND live.fenced_at IS NULL)
 AND (SELECT count(*)=4 FROM computer_checkpoint_objects o WHERE o.environment_id=p.environment_id AND o.checkpoint_id=p.id))
 FROM computer_checkpoints p JOIN computer_leases source ON(source.environment_id,source.computer_id,source.epoch)=(p.environment_id,p.computer_id,p.source_lease_epoch)
 JOIN worker_hosts h ON h.id=source.worker_host_id
 LEFT JOIN computer_leases bad ON bad.environment_id=p.environment_id AND bad.computer_id=p.computer_id AND bad.epoch>source.epoch AND bad.worker_host_id=h.id AND bad.worker_epoch=$3
 WHERE p.environment_id=$1 AND p.id=$2 GROUP BY p.environment_id,p.id,h.id`, env, checkpoint, wrongEpoch, workergroup.ObservationFreshnessSeconds).Scan(&raw)
		if err != nil {
			return state, err
		}
		return state, json.Unmarshal(raw, &state)
	}
	var fault nativeCheckpointKeyFault
	if err := awaitLossCondition(ctx, func() (bool, error) {
		select {
		case <-active.done:
			return false, fmt.Errorf("wrong-key Worker exited instead of staying paused: %v", active.err)
		default:
		}
		var err error
		fault, err = readFault()
		return fault.Ready, err
	}); err != nil {
		return err
	}
	pausedAt := time.Now()
	if err := writeNativeHandoffFile(observations+".key-fault", fault); err != nil {
		return err
	}
	if err := awaitRequest("repair-request"); err != nil {
		return err
	}
	after, err := readFault()
	if err != nil {
		return err
	}
	if after != fault || time.Since(pausedAt) < 20*time.Second {
		return errors.New("key mismatch did not preserve a stable paused checkpoint for twenty seconds")
	}
	if err := stopNativeKeyWorker(active, true); err != nil {
		return err
	}
	active = nil
	raw, err := os.ReadFile(filepath.Join(input.Evidence, "worker-wrong-key.log"))
	if err != nil {
		return err
	}
	if strings.Count(string(raw), "Worker checkpoint key differs from retained Computer") != 1 {
		return errors.New("wrong-key Worker did not log exactly one positively identified mismatch")
	}
	active, err = startNativeWorker(input, environment, "worker-correct-key.log")
	if err != nil {
		return err
	}
	correctedEpoch, err := awaitActive(wrongEpoch)
	if err != nil {
		return err
	}
	repaired := map[string]any{"checkpointId": checkpoint, "workerEpoch": correctedEpoch, "wrongWorkerJoined": true, "pausedMs": time.Since(pausedAt).Milliseconds(), "failedLeaseCount": fault.FailedLeaseCount, "lastFailureEpoch": fault.LastFailureEpoch}
	if err := writeNativeHandoffFile(observations+".key-repaired", repaired); err != nil {
		return err
	}
	if err := writeNativeHandoffFile(filepath.Join(input.Evidence, "checkpoint-key-mismatch.json"), map[string]any{"fault": fault, "repaired": repaired}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return nil
	case <-active.done:
		return fmt.Errorf("correct-key Worker exited before fixture retirement: %v", active.err)
	}
}

func stopNativeKeyWorker(worker *nativeWorkerProcess, requireRunning bool) error {
	select {
	case <-worker.done:
		if requireRunning {
			return fmt.Errorf("key qualification Worker exited before requested shutdown: %v", worker.err)
		}
		return worker.err
	default:
	}
	if err := worker.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	stopped, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if err := worker.wait(stopped); err != nil {
		_ = worker.cmd.Process.Kill()
		<-worker.done
		return fmt.Errorf("key qualification Worker did not join ordinary shutdown: %w", err)
	}
	return nil
}

func verifyNativeCheckpointKeyRecovery(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, computer, checkpoint string, sourceEpoch int64, fault *nativeCheckpointKeyFault) error {
	if fault == nil || fault.CheckpointID.String() != checkpoint || fault.ComputerID.String() != computer || fault.WorkerHostID == uuid.Nil() || !fault.Ready || fault.WorkerEpoch <= 0 || fault.FailedLeaseCount < 1 || fault.LastFailureEpoch <= sourceEpoch || fault.PausedMS < 20000 || fault.Repaired == nil || fault.Repaired.CheckpointID != fault.CheckpointID || !fault.Repaired.WrongWorkerJoined || fault.Repaired.WorkerEpoch <= fault.WorkerEpoch {
		return errors.New("key mismatch receipt did not establish pause and ordinary corrected supply")
	}
	var valid bool
	err := pool.QueryRow(ctx, `SELECT p.status='consumed' AND p.controls_reconciled_at IS NOT NULL
 AND p.source_lease_epoch=$8 AND target.epoch=$6+1 AND target.worker_host_id=$4 AND target.worker_epoch=$7
 AND (SELECT count(*)=$5 AND bool_and(bad.worker_host_id=$4 AND bad.worker_epoch=$9 AND bad.initialized_at IS NULL
 AND bad.fence_evidence='owning worker joined physical VM closure' AND bad.fenced_at IS NOT NULL AND bad.fenced_at<=target.delivered_at AND bad.restored_from_save_id=p.disk_save_id)
 FROM computer_leases bad WHERE bad.environment_id=p.environment_id AND bad.computer_id=p.computer_id AND bad.epoch>p.source_lease_epoch AND bad.epoch<target.epoch)
 FROM computer_checkpoints p JOIN computer_leases target ON(target.environment_id,target.computer_id,target.epoch)=(p.environment_id,p.computer_id,p.target_lease_epoch)
 WHERE p.environment_id=$1 AND p.computer_id=$2 AND p.id=$3`, env, computer, checkpoint, fault.WorkerHostID, fault.FailedLeaseCount, fault.LastFailureEpoch, fault.Repaired.WorkerEpoch, sourceEpoch, fault.WorkerEpoch).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return errors.New("key mismatch did not restore the retained cut after fencing every failed allocation")
	}
	return nil
}
