//go:build linux && computerproof

package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"uuid"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Mutate one checkpoint object only after the SDK driver observes normal source
// fencing. No database update, product fault hook or manufactured stop receipt
// participates in this case. Evidence retains S3 object/marker identities; normal
// reclamation may subsequently delete the retired object versions.
func injectNativeCheckpointFault(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, rawURI, observations string, mode nativeExecutionMode) error {
	uri, err := url.Parse(rawURI)
	if err != nil {
		return err
	}
	if uri.Scheme != "s3" || uri.Host == "" || !strings.HasPrefix(uri.Path, "/_verification/native-execution/") || !strings.HasSuffix(uri.Path, "/cas") || uri.RawQuery != "" || uri.Fragment != "" {
		return errors.New("checkpoint fault requires an isolated native execution CAS prefix")
	}
	var request struct {
		CheckpointID uuid.UUID `json:"checkpointId"`
	}
	for {
		raw, err := os.ReadFile(observations + ".fault-request")
		if err == nil {
			if err := json.Unmarshal(raw, &request); err != nil {
				return err
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(100 * time.Millisecond):
		}
	}
	role := "vm_config"
	if mode == nativeExecutionMissingMemory {
		role = "memory"
	} else if mode != nativeExecutionCorruptConfig {
		return errors.New("unsupported checkpoint fault")
	}
	var object nativeCheckpointObject
	err = pool.QueryRow(ctx, `SELECT o.role,o.digest,o.size_bytes
 FROM computer_checkpoint_objects o JOIN computer_checkpoints p ON (p.environment_id,p.id)=(o.environment_id,o.checkpoint_id)
 JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.source_lease_epoch)
 WHERE p.environment_id=$1 AND p.id=$2 AND p.status='ready' AND l.fenced_at>=p.ready_at
 AND o.role=$3`, env, request.CheckpointID, role).Scan(&object.Role, &object.Digest, &object.SizeBytes)
	if err != nil {
		return err
	}
	if object.SizeBytes <= 0 || (mode == nativeExecutionCorruptConfig && object.SizeBytes > 1<<20) {
		return errors.New("unexpected checkpoint object size")
	}
	key, err := cas.ObjectKey(uri.Path, object.Digest)
	if err != nil {
		return err
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	store := awss3.NewFromConfig(cfg)
	head, err := store.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(uri.Host), Key: aws.String(key)})
	if err != nil {
		return err
	}
	if aws.ToInt64(head.ContentLength) != object.SizeBytes || aws.ToString(head.VersionId) == "" || aws.ToString(head.VersionId) == "null" || aws.ToString(head.ETag) == "" {
		return errors.New("checkpoint fault requires the exact original versioned object")
	}
	evidence := map[string]any{"checkpointId": request.CheckpointID, "kind": mode, "role": object.Role, "objectKey": key, "sizeBytes": object.SizeBytes,
		"originalDigest": object.Digest, "originalVersionId": aws.ToString(head.VersionId)}
	if mode == nativeExecutionMissingMemory {
		deleted, err := store.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(uri.Host), Key: aws.String(key), IfMatch: head.ETag})
		if err != nil {
			return err
		}
		if !aws.ToBool(deleted.DeleteMarker) || aws.ToString(deleted.VersionId) == "" || aws.ToString(deleted.VersionId) == "null" || aws.ToString(deleted.VersionId) == aws.ToString(head.VersionId) {
			return errors.New("memory fault did not create a distinct delete marker")
		}
		_, err = store.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(uri.Host), Key: aws.String(key)})
		if err == nil {
			return errors.New("memory object remained readable after deletion")
		}
		var response smithy.APIError
		if !errors.As(err, &response) || response.ErrorCode() != "NotFound" {
			return fmt.Errorf("memory object absence not observed: %w", err)
		}
		evidence["deleteMarkerVersionId"], evidence["observedAbsent"] = aws.ToString(deleted.VersionId), true
	} else {
		get, err := store.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(uri.Host), Key: aws.String(key)})
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(io.LimitReader(get.Body, (1<<20)+1))
		closeErr := get.Body.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		digest := func(body []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(body)) }
		if int64(len(body)) != object.SizeBytes || digest(body) != object.Digest || aws.ToString(get.VersionId) != aws.ToString(head.VersionId) {
			return errors.New("checkpoint fault did not read the exact original configuration")
		}
		body[0] ^= 1
		put, err := store.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String(uri.Host), Key: aws.String(key), Body: bytes.NewReader(body), IfMatch: head.ETag, ContentType: head.ContentType, Metadata: head.Metadata, Tagging: aws.String(url.QueryEscape(cas.ExpirableTagKey) + "=" + url.QueryEscape(cas.ExpirableTagValue))})
		if err != nil {
			return err
		}
		if aws.ToString(put.VersionId) == "" || aws.ToString(put.VersionId) == "null" || aws.ToString(put.VersionId) == aws.ToString(head.VersionId) {
			return errors.New("checkpoint fault did not create a distinct object version")
		}
		check, err := store.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(uri.Host), Key: aws.String(key)})
		if err != nil {
			return err
		}
		observed, readErr := io.ReadAll(io.LimitReader(check.Body, (1<<20)+1))
		closeErr = check.Body.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if !bytes.Equal(observed, body) || aws.ToString(check.VersionId) != aws.ToString(put.VersionId) {
			return errors.New("checkpoint corruption was not observed at the ordinary read key")
		}
		evidence["corruptedDigest"], evidence["corruptedVersionId"] = digest(body), aws.ToString(put.VersionId)
	}
	receipt, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	if err := os.WriteFile(observations+".fault-receipt.new", receipt, 0600); err != nil {
		return err
	}
	return os.Rename(observations+".fault-receipt.new", observations+".fault-receipt")
}

func verifyNativeCheckpointLoss(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, result []byte, evidence string) error {
	var receipt struct {
		ComputerID     string `json:"computerId"`
		CheckpointLoss struct {
			Checkpoint struct {
				CheckpointID string `json:"checkpointId"`
				SourceEpoch  int64  `json:"sourceEpoch"`
			} `json:"checkpoint"`
			Holds       map[string]string `json:"holds"`
			QueuedTurns []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"queuedTurns"`
			HeldMs float64 `json:"heldMs"`
		} `json:"checkpointLoss"`
	}
	if err := json.Unmarshal(result, &receipt); err != nil {
		return err
	}
	fault := receipt.CheckpointLoss
	if len(fault.Holds) != 4 || len(fault.QueuedTurns) != 2 || fault.HeldMs < 20000 {
		return errors.New("incomplete checkpoint loss observations")
	}
	for _, turn := range fault.QueuedTurns {
		if turn.ID == "" || turn.Status != "queued" {
			return errors.New("checkpoint loss dispatched or lost accepted input")
		}
	}
	var valid bool
	var raw []byte
	err := pool.QueryRow(ctx, `SELECT
 p.status='lost' AND p.terminal_evidence='owning worker proved checkpoint contents invalid after physical cleanup'
 AND source.fenced_at>=p.ready_at AND p.controls_reconciled_at IS NULL
 AND (SELECT count(*)=1 AND bool_and(l.fenced_at IS NOT NULL AND l.initialized_at IS NULL AND l.restored_from_save_id=p.disk_save_id)
      FROM computer_leases l WHERE l.environment_id=p.environment_id AND l.computer_id=p.computer_id AND l.epoch>p.source_lease_epoch)
 AND (SELECT count(*)=4 AND bool_and(s.epoch=1 AND s.fenced_at IS NOT NULL AND s.failure_recorded_at IS NOT NULL)
      FROM session_processes s WHERE s.environment_id=p.environment_id AND s.computer_id=p.computer_id),
 jsonb_build_object('checkpointId',p.id,'status',p.status,'terminalEvidence',p.terminal_evidence,'sourceFencedAt',source.fenced_at,
 'leases',(SELECT jsonb_agg(jsonb_build_object('epoch',l.epoch,'status',l.status,'deliveredAt',l.delivered_at,'initializedAt',l.initialized_at,'fencedAt',l.fenced_at,'fenceEvidence',l.fence_evidence)) FROM computer_leases l WHERE l.environment_id=p.environment_id AND l.computer_id=p.computer_id),
 'processes',(SELECT jsonb_agg(jsonb_build_object('sessionId',s.session_id,'epoch',s.epoch,'status',s.status,'fencedAt',s.fenced_at,'failureRecordedAt',s.failure_recorded_at)) FROM session_processes s WHERE s.environment_id=p.environment_id AND s.computer_id=p.computer_id))
 FROM computer_checkpoints p JOIN computer_leases source ON (source.environment_id,source.computer_id,source.epoch)=(p.environment_id,p.computer_id,p.source_lease_epoch)
 WHERE p.environment_id=$1 AND p.computer_id=$2 AND p.id=$3 AND p.source_lease_epoch=$4`, env, receipt.ComputerID, fault.Checkpoint.CheckpointID, fault.Checkpoint.SourceEpoch).Scan(&valid, &raw)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(evidence, "checkpoint-loss.json"), raw, 0600); err != nil {
		return err
	}
	if !valid {
		return fmt.Errorf("checkpoint loss did not preserve physical fencing and held process generations: %s", raw)
	}
	return nil
}
