//go:build linux && computerproof

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This private input travels directly between pinned host transports, never in
// the public proof collector. It configures an ordinary, fresh Worker process.
type nativeCrossHostBootstrap struct {
	CheckpointID    uuid.UUID `json:"checkpointId"`
	SourceHostID    string    `json:"sourceHostId"`
	TargetHostID    string    `json:"targetHostId"`
	ControlPlaneURL string    `json:"controlPlaneUrl"`
	EnrollmentToken string    `json:"enrollmentToken"`
	CheckpointKey   string    `json:"checkpointKey"`
	CASURI          string    `json:"casUri"`
	PlatformURI     string    `json:"platformUri"`
}

func writeNativeHandoffFile(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path+".new", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(raw)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	return os.Rename(path+".new", path)
}

func handoffNativeCheckpoint(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, input nativeExecutionConfig, observations string, worker *nativeWorkerProcess, expectedStop *atomic.Bool, bootstrap nativeCrossHostBootstrap) error {
	var request struct {
		CheckpointID uuid.UUID `json:"checkpointId"`
	}
	if err := awaitLossCondition(ctx, func() (bool, error) {
		raw, err := os.ReadFile(observations + ".handoff-request")
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, json.Unmarshal(raw, &request)
	}); err != nil {
		return err
	}
	handoffStarted := time.Now()
	ctx, stopHandoff := context.WithTimeout(ctx, 5*time.Minute)
	defer stopHandoff()
	var source uuid.UUID
	var platform, cpuDigest string
	var vcpus int64
	err := pool.QueryRow(ctx, `SELECT l.worker_host_id,p.vm_platform_id,l.cpu_config_digest,l.vm_vcpu_count
 FROM computer_checkpoints p JOIN computer_leases l ON(l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.source_lease_epoch)
 JOIN worker_hosts h ON h.id=l.worker_host_id
 WHERE p.environment_id=$1 AND p.id=$2 AND p.status='ready' AND l.fenced_at>=p.ready_at
 AND h.resource_id=$3`, env, request.CheckpointID, input.SourceHostID).Scan(&source, &platform, &cpuDigest, &vcpus)
	if err != nil {
		return fmt.Errorf("read physically released source: %w", err)
	}
	// No host is enrolled from these inputs until its source process has joined.
	expectedStop.Store(true)
	if err := worker.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	stopped, cancel := context.WithTimeout(ctx, 40*time.Second)
	err = worker.wait(stopped)
	cancel()
	if err != nil {
		return fmt.Errorf("join source worker: %w", err)
	}
	bootstrap.CheckpointID = request.CheckpointID
	if err := writeNativeHandoffFile(filepath.Join(input.Evidence, "cross-host-bootstrap-private.json"), bootstrap); err != nil {
		return err
	}
	if err := writeNativeHandoffFile(filepath.Join(input.Evidence, "cross-host-ready.json"), map[string]any{"checkpointId": request.CheckpointID, "sourceResourceId": input.SourceHostID, "targetResourceId": input.TargetHostID, "controlPlaneUrl": bootstrap.ControlPlaneURL, "vmPlatformId": platform, "cpuConfigDigest": cpuDigest, "vcpus": vcpus, "sourceWorkerJoined": true, "handoffStartedAt": handoffStarted.UTC(), "sourceJoinedAt": time.Now().UTC()}); err != nil {
		return err
	}
	var target uuid.UUID
	var receipt []byte
	err = awaitLossCondition(ctx, func() (bool, error) {
		var ready, sourceStale bool
		err := pool.QueryRow(ctx, `SELECT target.id,
 COALESCE(target.status='active' AND target.current_epoch IS NOT NULL AND target.run_paused_reason IS NULL AND target.vm_paused_reason IS NULL
 AND target.observed_at>=clock_timestamp()-$6*interval '1 second'
 AND target.vm_platform_id=$3 AND EXISTS(SELECT 1 FROM worker_pool_cpu_shapes shape WHERE shape.worker_pool_id=target.worker_pool_id AND shape.vcpu_count=$4 AND shape.cpu_config_digest=$5),false),
 source.observed_at<clock_timestamp()-$6*interval '1 second',
 jsonb_build_object('checkpointId',$7::uuid,'sourceHostId',source.id,'targetHostId',target.id,
 'sourceResourceId',source.resource_id,'targetResourceId',target.resource_id,'vmPlatformId',target.vm_platform_id,'targetStatus',target.status,
 'cpuConfigDigest',$5::text,'sourceObservedAt',source.observed_at,'targetObservedAt',target.observed_at,
 'sourceWorkerJoined',true,'sourceObservationExpired',source.observed_at<clock_timestamp()-$6*interval '1 second')
 FROM worker_hosts source JOIN worker_hosts target ON target.worker_group_id=source.worker_group_id
 WHERE source.id=$1 AND target.resource_id=$2 AND target.id<>source.id AND target.status IN ('registering','active','draining')
`, source, input.TargetHostID, platform, vcpus, cpuDigest, workergroup.ObservationFreshnessSeconds, request.CheckpointID).Scan(&target, &ready, &sourceStale, &receipt)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return ready && sourceStale, nil
	})
	if err != nil {
		return fmt.Errorf("target qualification or source observation expiry did not complete: %w; last observation: %s; inspect target worker log", err, receipt)
	}
	var stillReady bool
	if err := pool.QueryRow(ctx, `SELECT status='ready' AND target_lease_epoch IS NULL FROM computer_checkpoints WHERE environment_id=$1 AND id=$2`, env, request.CheckpointID).Scan(&stillReady); err != nil {
		return err
	}
	if !stillReady {
		return errors.New("checkpoint changed before cross-host handoff")
	}

	var timed map[string]any
	if err := json.Unmarshal(receipt, &timed); err != nil {
		return err
	}
	timed["handoffStartedAt"], timed["readyAt"], timed["elapsedMs"] = handoffStarted.UTC(), time.Now().UTC(), time.Since(handoffStarted).Milliseconds()
	return writeNativeHandoffFile(observations+".handoff-receipt", timed)
}

func verifyNativeCrossHostContinuation(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, result []byte, input nativeExecutionConfig) error {
	var receipt struct {
		ComputerID    string `json:"computerId"`
		Continuations []struct {
			Checkpoint struct {
				CheckpointID string `json:"checkpointId"`
			} `json:"checkpoint"`
		} `json:"continuations"`
	}
	if err := json.Unmarshal(result, &receipt); err != nil {
		return err
	}
	if len(receipt.Continuations) != 2 {
		return errors.New("cross-host fixture requires two continuations")
	}
	records := make([]json.RawMessage, 0, 2)
	for index, cycle := range receipt.Continuations {
		expectedSource := input.TargetHostID
		if index == 0 {
			expectedSource = input.SourceHostID
		}
		var valid bool
		var raw json.RawMessage
		err := pool.QueryRow(ctx, `SELECT source.resource_id=$4 AND target.resource_id=$5
 AND ((source.id<>target.id)=$6) AND a.vm_platform_id=b.vm_platform_id AND a.cpu_config_digest=b.cpu_config_digest,
 jsonb_build_object('checkpointId',p.id,'sourceHostId',source.id,'targetHostId',target.id,
 'sourceResourceId',source.resource_id,'targetResourceId',target.resource_id,'vmPlatformId',a.vm_platform_id,'cpuConfigDigest',a.cpu_config_digest)
 FROM computer_checkpoints p
 JOIN computer_leases a ON(a.environment_id,a.computer_id,a.epoch)=(p.environment_id,p.computer_id,p.source_lease_epoch)
 JOIN computer_leases b ON(b.environment_id,b.computer_id,b.epoch)=(p.environment_id,p.computer_id,p.target_lease_epoch)
 JOIN worker_hosts source ON source.id=a.worker_host_id JOIN worker_hosts target ON target.id=b.worker_host_id
 WHERE p.environment_id=$1 AND p.computer_id=$2 AND p.id=$3`, env, receipt.ComputerID, cycle.Checkpoint.CheckpointID, expectedSource, input.TargetHostID, index == 0).Scan(&valid, &raw)
		if err != nil {
			return err
		}
		if !valid {
			return fmt.Errorf("cross-host physical resource or runtime mismatch: %s", raw)
		}
		records = append(records, raw)
	}
	return writeNativeHandoffFile(filepath.Join(input.Evidence, "cross-host-continuation.json"), records)
}
