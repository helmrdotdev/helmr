//go:build linux && computerproof

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/jackc/pgx/v5/pgxpool"
)

type nativeCheckpointObject struct {
	Role      string `json:"role"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
}

func writeNativeCheckpointObservations(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, path string) error {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		var raw []byte
		err := pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(observed),'[]'::jsonb) FROM (
 SELECT jsonb_build_object('computerId',p.computer_id,'checkpointId',p.id,'sourceEpoch',p.source_lease_epoch,'sourceFenced',l.fenced_at IS NOT NULL,'status',p.status,'observedAt',clock_timestamp(),
  'unfencedLeases',(SELECT count(*) FROM computer_leases live WHERE live.environment_id=p.environment_id AND live.computer_id=p.computer_id AND live.fenced_at IS NULL),
  'runtimeObjects',(SELECT jsonb_agg(jsonb_build_object('role',o.role,'digest',o.digest,'sizeBytes',o.size_bytes)) FROM computer_checkpoint_objects o WHERE o.environment_id=p.environment_id AND o.checkpoint_id=p.id)) observed
 FROM computer_checkpoints p JOIN computer_leases l ON(l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.source_lease_epoch)
 WHERE p.environment_id=$1 AND p.ready_at IS NOT NULL ORDER BY p.created_at DESC LIMIT 10) recent`, env).Scan(&raw)
		if err != nil {
			return err
		}
		if err = os.WriteFile(path+".new", raw, 0600); err != nil {
			return err
		}
		if err = os.Rename(path+".new", path); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-tick.C:
		}
	}
}

func verifyNativeHealthyContinuation(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, result []byte, evidence string, longIdle, keyMismatch bool) error {
	var receipt struct {
		ComputerID    string `json:"computerId"`
		Continuations []struct {
			KeyFault        *nativeCheckpointKeyFault `json:"keyFault"`
			RequestedIdleMS int64                     `json:"requestedIdleMs"`
			IdleMS          float64                   `json:"idleMs"`
			IdleSamples     int                       `json:"idleSamples"`
			IdleStartedAt   time.Time                 `json:"idleStartedAt"`
			IdleFinishedAt  time.Time                 `json:"idleFinishedAt"`
			Checkpoint      struct {
				CheckpointID   string                   `json:"checkpointId"`
				SourceEpoch    int64                    `json:"sourceEpoch"`
				RuntimeObjects []nativeCheckpointObject `json:"runtimeObjects"`
			} `json:"checkpoint"`
		} `json:"continuations"`
	}
	if err := json.Unmarshal(result, &receipt); err != nil {
		return err
	}
	if len(receipt.Continuations) != 2 {
		return errors.New("healthy fixture requires two complete continuations")
	}
	seen := map[string]bool{}
	var records []json.RawMessage
	for index, cycle := range receipt.Continuations {
		if longIdle && index == 0 && (cycle.RequestedIdleMS != 7200000 || cycle.IdleMS < 7200000 || cycle.IdleSamples < 2 || cycle.IdleStartedAt.IsZero() || cycle.IdleFinishedAt.Sub(cycle.IdleStartedAt) < 2*time.Hour) {
			return errors.New("long-idle continuation did not observe two hours of released compute")
		}
		if keyMismatch && index == 0 {
			if err := verifyNativeCheckpointKeyRecovery(ctx, pool, env, receipt.ComputerID, cycle.Checkpoint.CheckpointID, cycle.Checkpoint.SourceEpoch, cycle.KeyFault); err != nil {
				return err
			}
		}
		id := cycle.Checkpoint.CheckpointID
		if seen[id] {
			return errors.New("healthy continuation reused a checkpoint")
		}
		seen[id] = true
		var valid bool
		var raw json.RawMessage
		var manifestBytes []byte
		err := pool.QueryRow(ctx, `SELECT p.status='consumed' AND p.controls_reconciled_at IS NOT NULL
   AND source.fenced_at>=p.ready_at AND target.delivered_at>=source.fenced_at
   AND source.fence_evidence='owning worker joined physical VM closure'
   AND source.computer_instance_id<>target.computer_instance_id AND target.epoch>source.epoch
   AND source.channel_credential_digest<>target.channel_credential_digest
   AND p.source_lease_epoch=$4 AND s.status='published'
   AND (NOT $5 OR (target.epoch=source.epoch+1
    AND target.delivered_at-source.fenced_at>=interval '2 hours'
    AND $6::timestamptz>=source.fenced_at AND target.delivered_at>=$7::timestamptz))
   AND (SELECT count(*)=4 AND bool_and(m.process_epoch=1) FROM computer_checkpoint_members m WHERE m.environment_id=p.environment_id AND m.checkpoint_id=p.id),
   jsonb_build_object('checkpointId',p.id,'sourceInstance',source.computer_instance_id,'targetInstance',target.computer_instance_id,
    'sourceHost',source.worker_host_id,'targetHost',target.worker_host_id,'sourceEpoch',source.epoch,'targetEpoch',target.epoch,
    'readyAt',p.ready_at,'sourceFencedAt',source.fenced_at,'sourceFenceEvidence',source.fence_evidence,'targetDeliveredAt',target.delivered_at,'controlsReconciledAt',p.controls_reconciled_at,
    'status',p.status,'diskSaveStatus',s.status,'credentialsRotated',source.channel_credential_digest<>target.channel_credential_digest,
    'memberCount',(SELECT count(*) FROM computer_checkpoint_members m WHERE m.environment_id=p.environment_id AND m.checkpoint_id=p.id),
    'memberProcessEpochs',(SELECT jsonb_agg(m.process_epoch) FROM computer_checkpoint_members m WHERE m.environment_id=p.environment_id AND m.checkpoint_id=p.id)),p.manifest
 FROM computer_checkpoints p JOIN computer_leases source ON(source.environment_id,source.computer_id,source.epoch)=(p.environment_id,p.computer_id,p.source_lease_epoch)
 JOIN computer_leases target ON(target.environment_id,target.computer_id,target.epoch)=(p.environment_id,p.computer_id,p.target_lease_epoch)
 JOIN computer_saves s ON(s.environment_id,s.id)=(p.environment_id,p.disk_save_id)
 WHERE p.environment_id=$1 AND p.computer_id=$2 AND p.id=$3`, env, receipt.ComputerID, id, cycle.Checkpoint.SourceEpoch, longIdle && index == 0, cycle.IdleStartedAt, cycle.IdleFinishedAt).Scan(&valid, &raw, &manifestBytes)
		if err != nil {
			return err
		}
		if !valid {
			return fmt.Errorf("healthy continuation %s lost coherent ownership or source fencing: %s", id, raw)
		}
		// Retention may release object pins after both physical instances close.
		// Compare the objects observed while ready with the retained manifest.
		var manifest computercheckpoint.Manifest
		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			return err
		}
		if _, err := manifest.Encode(); err != nil {
			return err
		}
		if manifest.CheckpointID.String() != id || manifest.ComputerID.String() != receipt.ComputerID || manifest.LeaseEpoch != cycle.Checkpoint.SourceEpoch {
			return errors.New("healthy continuation manifest identity changed")
		}
		objects := manifest.Objects()
		if len(cycle.Checkpoint.RuntimeObjects) != len(objects) {
			return errors.New("healthy checkpoint did not observe all runtime objects while ready")
		}
		observed := make(map[string]nativeCheckpointObject)
		for _, object := range cycle.Checkpoint.RuntimeObjects {
			if _, duplicate := observed[object.Role]; duplicate {
				return errors.New("healthy checkpoint observed duplicate runtime role")
			}
			observed[object.Role] = object
		}
		for _, object := range objects {
			if observed[object.Role] != (nativeCheckpointObject{Role: object.Role, Digest: object.Digest, SizeBytes: object.SizeBytes}) {
				return errors.New("healthy checkpoint published object changed")
			}
		}
		var record map[string]json.RawMessage
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		record["runtimeObjects"], err = json.Marshal(cycle.Checkpoint.RuntimeObjects)
		if err != nil {
			return err
		}
		raw, err = json.Marshal(record)
		if err != nil {
			return err
		}
		records = append(records, raw)
	}
	raw, err := json.Marshal(records)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(evidence, "healthy-continuation.json"), raw, 0600)
}
