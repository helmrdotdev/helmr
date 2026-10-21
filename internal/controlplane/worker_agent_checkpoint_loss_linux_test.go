//go:build linux && computerproof

package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
func injectNativeCheckpointFault(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, rawURI, observations string, mode nativeExecutionMode) (resultErr error) {
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
	if mode == nativeExecutionMissingMemory || mode == nativeExecutionCorruptMemory {
		role = "memory"
	} else if mode != nativeExecutionCorruptConfig {
		return errors.New("unsupported checkpoint fault")
	}
	var object nativeCheckpointObject
	err = pool.QueryRow(ctx, `SELECT o.role,o.digest,o.size_bytes
 FROM checkpoint_objects o JOIN checkpoints p ON (p.environment_id,p.id)=(o.environment_id,o.checkpoint_id)
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
	var prepared nativeMemoryFaultObject
	defer func() {
		if prepared.key == "" {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		deletion := &awss3.DeleteObjectInput{Bucket: aws.String(uri.Host), Key: aws.String(prepared.key)}
		if prepared.version == "" {
			staged, err := store.HeadObject(cleanup, &awss3.HeadObjectInput{Bucket: deletion.Bucket, Key: deletion.Key})
			if err != nil {
				resultErr = errors.Join(resultErr, err)
				return
			}
			prepared.version = aws.ToString(staged.VersionId)
		}
		if prepared.version != "" {
			deletion.VersionId = aws.String(prepared.version)
		}
		_, err := store.DeleteObject(cleanup, deletion)
		resultErr = errors.Join(resultErr, err)
	}()
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

	} else if mode == nativeExecutionCorruptMemory {
		if err := corruptNativeMemoryPayload(ctx, store, uri.Host, key, head, object, observations, evidence, &prepared); err != nil {
			return err
		}
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
	if err := os.Rename(observations+".fault-receipt.new", observations+".fault-receipt"); err != nil {
		return err
	}
	if mode == nativeExecutionCorruptMemory {
		for {
			if _, err := os.Stat(observations + ".activate-fault-request"); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case <-time.After(50 * time.Millisecond):
			}
		}
		var active bool
		var allocation string
		if err := pool.QueryRow(ctx, `SELECT p.status='consumed' AND p.controls_reconciled_at IS NOT NULL AND l.initialized_at IS NOT NULL AND l.fenced_at IS NULL,l.allocation_id::text
 FROM checkpoints p JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.target_lease_epoch)
 WHERE p.environment_id=$1 AND p.id=$2`, env, request.CheckpointID).Scan(&active, &allocation); err != nil {
			return err
		}
		if !active {
			return errors.New("memory fault target is not an activated restore")
		}
		log, err := os.ReadFile(filepath.Join(filepath.Dir(observations), "worker.log"))
		if err != nil {
			return err
		}
		if regexp.MustCompile(`(?m)^.*allocation_id=` + regexp.QuoteMeta(allocation) + ` .*phase=restore_memory_hydration .*`).Match(log) {
			return errors.New("hydration finished before live fault injection")
		}
		copied, err := store.CopyObject(ctx, &awss3.CopyObjectInput{Bucket: aws.String(uri.Host), Key: aws.String(key),
			TaggingDirective: "REPLACE", Tagging: aws.String(url.QueryEscape(cas.ExpirableTagKey) + "=" + url.QueryEscape(cas.ExpirableTagValue)),
			CopySource: aws.String(url.PathEscape(uri.Host+"/"+prepared.key) + "?versionId=" + url.QueryEscape(prepared.version))})
		if err != nil {
			return err
		}
		if aws.ToString(copied.VersionId) == "" || aws.ToString(copied.VersionId) == aws.ToString(head.VersionId) {
			return errors.New("live corruption did not create an object version")
		}
		log, err = os.ReadFile(filepath.Join(filepath.Dir(observations), "worker.log"))
		if err != nil {
			return err
		}
		if regexp.MustCompile(`(?m)^.*allocation_id=` + regexp.QuoteMeta(allocation) + ` .*phase=restore_memory_hydration .*error_class=""(?: |$)`).Match(log) {
			return errors.New("hydration finished during live fault object swap")
		}
		evidence["activatedVersionId"] = aws.ToString(copied.VersionId)
		evidence["activatedAt"] = time.Now().UTC()
		return writeNativeHandoffFile(observations+".activate-fault-receipt", evidence)
	}
	if mode != nativeExecutionMissingMemory {
		return nil
	}
	// Keep the object unavailable through two complete failed restore owners.
	// Repair access to the exact authenticated version, without changing the
	// checkpoint or manufacturing any physical-stop receipt.
	wait, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var failed int
	for {
		var retained bool
		err := pool.QueryRow(wait, `SELECT p.status IN ('ready','restoring'),
 (SELECT count(*) FROM computer_leases l WHERE l.environment_id=p.environment_id AND l.computer_id=p.computer_id AND l.epoch>p.source_lease_epoch AND l.initialized_at IS NULL AND l.fenced_at IS NOT NULL AND l.restored_from_save_id=p.disk_save_id)
 FROM checkpoints p WHERE p.environment_id=$1 AND p.id=$2`, env, request.CheckpointID).Scan(&retained, &failed)
		if err != nil {
			return err
		}
		if !retained {
			return errors.New("missing payload invalidated retained checkpoint")
		}
		if failed >= 2 {
			break
		}
		select {
		case <-wait.Done():
			return context.Cause(wait)
		case <-time.After(100 * time.Millisecond):
		}
	}
	marker := evidence["deleteMarkerVersionId"].(string)
	if _, err := store.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(uri.Host), Key: aws.String(key), VersionId: aws.String(marker)}); err != nil {
		return err
	}
	repaired, err := store.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(uri.Host), Key: aws.String(key)})
	if err != nil {
		return err
	}
	if aws.ToString(repaired.VersionId) != aws.ToString(head.VersionId) || aws.ToInt64(repaired.ContentLength) != object.SizeBytes {
		return errors.New("repair did not restore the exact retained object")
	}
	evidence["failedRestoreOwners"], evidence["repairedVersionId"] = failed, aws.ToString(repaired.VersionId)
	return writeNativeHandoffFile(observations+".repair-receipt", evidence)
}

func verifyNativeCheckpointLoss(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, result []byte, evidence string) error {
	var receipt struct {
		ComputerID     string `json:"computerId"`
		CheckpointLoss struct {
			Checkpoint struct {
				CheckpointID string `json:"checkpointId"`
				SourceEpoch  int64  `json:"sourceEpoch"`
			} `json:"checkpoint"`
			RecoveryTurnID string `json:"recoveryTurnId"`
			Fault          struct {
				Kind string `json:"kind"`
			} `json:"fault"`
		} `json:"checkpointLoss"`
	}
	if err := json.Unmarshal(result, &receipt); err != nil {
		return err
	}
	fault := receipt.CheckpointLoss
	if fault.RecoveryTurnID == "" {
		return errors.New("missing automatic disk recovery receipt")
	}
	var valid bool
	var raw []byte
	err := pool.QueryRow(ctx, `SELECT
 p.status='lost' AND p.terminal_evidence='owning worker proved checkpoint contents invalid after physical cleanup'
 AND source.fenced_at>=p.ready_at AND source.fenced_at<=target.delivered_at AND ($6 OR p.controls_reconciled_at IS NULL)
 AND recovered.status='completed' AND recovered.process_epoch=2
 AND target.disk_only_recovery AND target.initialized_at IS NOT NULL
 AND (target.restored_from_save_id=p.disk_save_id OR ($6 AND EXISTS(SELECT 1 FROM computer_saves recovered_save JOIN computer_saves checkpoint_save
   ON (checkpoint_save.environment_id,checkpoint_save.computer_id)=(recovered_save.environment_id,recovered_save.computer_id)
   WHERE checkpoint_save.id=p.disk_save_id AND recovered_save.environment_id=p.environment_id AND recovered_save.computer_id=p.computer_id
   AND recovered_save.id=target.restored_from_save_id AND recovered_save.status='published' AND recovered_save.seq>=checkpoint_save.seq
   AND recovered_save.computer_lease_epoch>p.source_lease_epoch AND recovered_save.computer_lease_epoch<target.epoch)))
 AND (SELECT count(*)=1 AND bool_and(l.fenced_at IS NOT NULL AND l.fenced_at<=target.delivered_at AND ($6 OR l.initialized_at IS NULL) AND l.restored_from_save_id=p.disk_save_id)
      FROM computer_leases l WHERE l.environment_id=p.environment_id AND l.computer_id=p.computer_id AND l.epoch>p.source_lease_epoch AND l.epoch<target.epoch)
 AND NOT EXISTS(SELECT 1 FROM session_holds h JOIN sessions s ON (s.environment_id,s.id)=(h.environment_id,h.session_id) WHERE s.environment_id=p.environment_id AND s.computer_id=p.computer_id AND h.released_at IS NULL)
 AND (SELECT count(*)=4 AND bool_and(s.epoch=1 AND s.fenced_at IS NOT NULL AND s.fenced_at<=target.delivered_at AND s.failure_recorded_at IS NOT NULL)
      FROM session_processes s WHERE s.environment_id=p.environment_id AND s.computer_id=p.computer_id AND s.epoch=1),
 jsonb_build_object('checkpointId',p.id,'status',p.status,'terminalEvidence',p.terminal_evidence,'sourceFencedAt',source.fenced_at,
 'recoverySaveId',target.restored_from_save_id,
 'leases',(SELECT jsonb_agg(jsonb_build_object('epoch',l.epoch,'status',l.status,'deliveredAt',l.delivered_at,'initializedAt',l.initialized_at,'fencedAt',l.fenced_at,'fenceEvidence',l.fence_evidence)) FROM computer_leases l WHERE l.environment_id=p.environment_id AND l.computer_id=p.computer_id),
 'processes',(SELECT jsonb_agg(jsonb_build_object('sessionId',s.session_id,'epoch',s.epoch,'status',s.status,'fencedAt',s.fenced_at,'failureRecordedAt',s.failure_recorded_at)) FROM session_processes s WHERE s.environment_id=p.environment_id AND s.computer_id=p.computer_id))
 FROM checkpoints p JOIN computer_leases source ON (source.environment_id,source.computer_id,source.epoch)=(p.environment_id,p.computer_id,p.source_lease_epoch)
 JOIN turns recovered ON recovered.environment_id=p.environment_id AND recovered.id=$5
 JOIN session_processes process ON (process.environment_id,process.session_id,process.epoch)=(recovered.environment_id,recovered.session_id,recovered.process_epoch)
 JOIN computer_leases target ON target.environment_id=p.environment_id AND target.computer_id=p.computer_id AND target.epoch=process.computer_lease_epoch
 WHERE p.environment_id=$1 AND p.computer_id=$2 AND p.id=$3 AND p.source_lease_epoch=$4`, env, receipt.ComputerID, fault.Checkpoint.CheckpointID, fault.Checkpoint.SourceEpoch, fault.RecoveryTurnID, fault.Fault.Kind == "corrupt-memory").Scan(&valid, &raw)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(evidence, "checkpoint-loss.json"), raw, 0600); err != nil {
		return err
	}

	if !valid {
		return fmt.Errorf("checkpoint loss did not fence old generations before automatic disk recovery: %s", raw)
	}
	if fault.Fault.Kind == "corrupt-memory" {
		var failedInstance string
		if err := pool.QueryRow(ctx, `SELECT allocation_id FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, env, receipt.ComputerID, fault.Checkpoint.SourceEpoch+1).Scan(&failedInstance); err != nil {
			return err
		}
		log, err := os.ReadFile(filepath.Join(evidence, "worker.log"))
		if err != nil {
			return err
		}
		match := regexp.MustCompile(`memory pager attached instance_id=` + regexp.QuoteMeta(failedInstance) + ` pid=([0-9]+)`).FindSubmatch(log)
		health := regexp.MustCompile(`(?m)^.*allocation_id=` + regexp.QuoteMeta(failedInstance) + ` .*phase=restore_wait_guest_health .*error_class="".*$`).FindIndex(log)
		failure := regexp.MustCompile(`(?m)^.*owner=` + regexp.QuoteMeta(receipt.ComputerID) + fmt.Sprintf(` epoch=%d .*authenticate memory chunk.*$`, fault.Checkpoint.SourceEpoch+1)).FindIndex(log)
		if len(match) != 2 || health == nil || failure == nil || health[0] >= failure[0] {
			return errors.New("missing successful guest health before owned VMM pager authentication failure")
		}
		command, err := os.ReadFile("/proc/" + string(match[1]) + "/cmdline")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if bytes.Contains(command, []byte(failedInstance)) {
			return errors.New("failed restore VMM remains alive after recovery")
		}
		proof, err := json.Marshal(map[string]any{"failedInstance": failedInstance, "pagerAttached": true, "guestHealthBeforeFailure": true, "payloadAuthenticationFailed": true, "ownedVMMAbsent": true})
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(evidence, "memory-pager-failure.json"), proof, 0600)
	}
	return nil
}

func verifyMissingCheckpointRepair(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, observations string) error {
	raw, err := os.ReadFile(observations + ".repair-receipt")
	if err != nil {
		return err
	}
	var proof struct {
		CheckpointID        uuid.UUID `json:"checkpointId"`
		FailedRestoreOwners int       `json:"failedRestoreOwners"`
		OriginalVersionID   string    `json:"originalVersionId"`
		RepairedVersionID   string    `json:"repairedVersionId"`
	}
	if err := json.Unmarshal(raw, &proof); err != nil {
		return err
	}
	if proof.FailedRestoreOwners < 2 || proof.OriginalVersionID == "" || proof.OriginalVersionID != proof.RepairedVersionID {
		return errors.New("missing checkpoint repair proof incomplete")
	}
	var valid bool
	err = pool.QueryRow(ctx, `SELECT p.status='consumed' AND p.controls_reconciled_at IS NOT NULL
 AND target.initialized_at IS NOT NULL AND target.restored_from_save_id=p.disk_save_id
 AND (SELECT count(*)>=2 AND bool_and(l.fenced_at IS NOT NULL AND l.fenced_at<=target.delivered_at AND l.initialized_at IS NULL AND l.restored_from_save_id=p.disk_save_id)
 FROM computer_leases l WHERE l.environment_id=p.environment_id AND l.computer_id=p.computer_id AND l.epoch>p.source_lease_epoch AND l.epoch<target.epoch)
 FROM checkpoints p JOIN computer_leases target ON (target.environment_id,target.computer_id,target.epoch)=(p.environment_id,p.computer_id,p.target_lease_epoch)
 WHERE p.environment_id=$1 AND p.id=$2`, env, proof.CheckpointID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return errors.New("repair did not resume the retained checkpoint after fencing failed targets")
	}
	return nil
}

type nativeMemoryFaultObject struct{ key, version string }

// Prepare an invalid payload under a separate key, preserving the authenticated
// header/index. The original stays intact until a restored Turn is running.
// Stream the object through disk, not host RAM.
func corruptNativeMemoryPayload(ctx context.Context, store *awss3.Client, bucket, key string, head *awss3.HeadObjectOutput, object nativeCheckpointObject, observations string, evidence map[string]any, prepared *nativeMemoryFaultObject) error {
	if object.SizeBytes < 81 {
		return errors.New("memory object has no payload")
	}
	file, err := os.CreateTemp(filepath.Dir(observations), "corrupt-memory-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	get, err := store.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: head.VersionId})
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, readErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(get.Body, object.SizeBytes+1))
	if err = errors.Join(readErr, get.Body.Close()); err != nil {
		return err
	}
	if n != object.SizeBytes || fmt.Sprintf("sha256:%x", hash.Sum(nil)) != object.Digest {
		return errors.New("memory fault source differs from checkpoint")
	}
	// v1 has a16-byte magic,16-byte key ID,32-byte salt and16-byte geometry.
	var header [80]byte
	if _, err = file.ReadAt(header[:], 0); err != nil {
		return err
	}
	indexBytes := binary.BigEndian.Uint64(header[72:])
	if string(header[:16]) != "helmr-memory-v1\n" || indexBytes > uint64(object.SizeBytes-80) || object.SizeBytes-int64(indexBytes) <= 80 {
		return errors.New("unexpected memory format")
	}
	payloadEnd := object.SizeBytes - int64(indexBytes)
	buffer := make([]byte, 256<<10)
	for offset := int64(80); offset < payloadEnd; {
		part := buffer[:min(int64(len(buffer)), payloadEnd-offset)]
		if _, err := file.ReadAt(part, offset); err != nil {
			return err
		}
		for i := range part {
			part[i] ^= 1
		}
		if _, err := file.WriteAt(part, offset); err != nil {
			return err
		}
		offset += int64(len(part))
	}
	var changed [1]byte
	if _, err := file.ReadAt(changed[:], 80); err != nil {
		return err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash.Reset()
	if _, err = io.Copy(hash, file); err != nil {
		return err
	}
	evidence["corruptedDigest"] = fmt.Sprintf("sha256:%x", hash.Sum(nil))
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	preparedKey := key + ".native-corrupt-" + uuid.NewV7().String()
	put, err := store.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(preparedKey), Body: file, ContentLength: aws.Int64(object.SizeBytes), IfNoneMatch: aws.String("*"), ContentType: head.ContentType, Metadata: head.Metadata, Tagging: aws.String(url.QueryEscape(cas.ExpirableTagKey) + "=" + url.QueryEscape(cas.ExpirableTagValue))})
	if err != nil {
		return err
	}
	prepared.key, prepared.version = preparedKey, aws.ToString(put.VersionId)
	if aws.ToString(put.VersionId) == "" || aws.ToString(put.VersionId) == "null" || aws.ToString(put.VersionId) == aws.ToString(head.VersionId) {
		return errors.New("memory corruption did not create a new version")
	}
	check, err := store.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(preparedKey), VersionId: put.VersionId, Range: aws.String("bytes=80-80")})
	if err != nil {
		return err
	}
	var observed [1]byte
	_, readErr = io.ReadFull(check.Body, observed[:])
	if err = errors.Join(readErr, check.Body.Close()); err != nil {
		return err
	}
	if observed != changed || aws.ToString(check.VersionId) != aws.ToString(put.VersionId) {
		return errors.New("memory corruption not observed")
	}
	evidence["corruptedVersionId"] = aws.ToString(put.VersionId)
	evidence["preparedObjectKey"] = preparedKey
	evidence["preparedVersionId"] = aws.ToString(put.VersionId)
	evidence["corruptedPayloadBytes"] = payloadEnd - 80
	return nil
}
