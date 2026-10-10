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
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerAgentCheckpointReadRetry(t *testing.T) {
	runWorkerAgentNativeExecution(t, nativeExecutionCheckpointRetry)
}

type nativeCheckpointReadRetry struct {
	CheckpointID   uuid.UUID `json:"checkpointId"`
	ComputerID     uuid.UUID `json:"computerId"`
	SourceEpoch    int64     `json:"sourceEpoch"`
	ManifestDigest string    `json:"manifestDigest"`
	FailedEpochs   []int64   `json:"failedEpochs"`
	ResumedEpoch   int64     `json:"resumedEpoch"`
}

// The real authenticated CP read succeeds before the fixture replaces its response
// with 503. Two distinct physical allocations must close before ordinary retry can
// read the same checkpoint. Production servers and Workers have no fault switch.
func (g *nativePublicationGate) serveCheckpointReadRetry(w http.ResponseWriter, r *http.Request) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return err
	}
	var identity workerapi.AllocationIdentity
	if err := json.Unmarshal(body, &identity); err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	response := httptest.NewRecorder()
	g.next.ServeHTTP(response, r)
	forward := func() {
		for name, values := range response.Header() {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.Code)
		_, _ = io.Copy(w, response.Body)
	}
	if response.Code != http.StatusOK {
		forward()
		return nil
	}
	var manifest computercheckpoint.Manifest
	if err := json.Unmarshal(response.Body.Bytes(), &manifest); err != nil {
		return err
	}
	encoded, err := manifest.Encode()
	if err != nil {
		return err
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(encoded))
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.checkpointReadRetry == nil {
		g.checkpointReadRetry = &nativeCheckpointReadRetry{CheckpointID: manifest.CheckpointID, ComputerID: manifest.ComputerID, SourceEpoch: manifest.LeaseEpoch, ManifestDigest: digest, FailedEpochs: []int64{}}
	}
	proof := g.checkpointReadRetry
	if manifest.CheckpointID != proof.CheckpointID {
		forward()
		return nil
	}
	if digest != proof.ManifestDigest || manifest.ComputerID != proof.ComputerID || manifest.LeaseEpoch != proof.SourceEpoch {
		return errors.New("transient checkpoint read changed its retained image")
	}
	for _, epoch := range proof.FailedEpochs {
		if identity.Epoch == epoch {
			http.Error(w, "temporary checkpoint read failure", http.StatusServiceUnavailable)
			return nil
		}
	}
	var valid bool
	err = g.pool.QueryRow(r.Context(), `SELECT p.status IN ('ready','restoring') AND p.controls_reconciled_at IS NULL
 AND source.fenced_at>=p.ready_at AND current.fenced_at IS NULL AND current.initialized_at IS NULL
 AND current.restored_from_save_id=p.disk_save_id AND current.delivered_at IS NOT NULL
 AND (SELECT count(*)=cardinality($5::bigint[]) AND COALESCE(bool_and(bad.fenced_at IS NOT NULL
  AND bad.initialized_at IS NULL AND bad.fence_evidence='owning worker joined physical VM closure'
  AND bad.fenced_at<=current.delivered_at AND bad.restored_from_save_id=p.disk_save_id),true)
  FROM computer_leases bad WHERE bad.environment_id=p.environment_id AND bad.computer_id=p.computer_id AND bad.epoch=ANY($5::bigint[]))
 FROM computer_checkpoints p
 JOIN computer_leases source ON(source.environment_id,source.computer_id,source.epoch)=(p.environment_id,p.computer_id,p.source_lease_epoch)
 JOIN computer_leases current ON current.environment_id=p.environment_id AND current.computer_id=p.computer_id AND current.epoch=$3 AND current.computer_instance_id=$4
 WHERE p.environment_id=$1 AND p.id=$2`, g.environment, proof.CheckpointID, identity.Epoch, identity.InstanceID, proof.FailedEpochs).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return errors.New("checkpoint retry lost its retained state or prior physical closure")
	}
	if identity.Epoch != proof.SourceEpoch+int64(len(proof.FailedEpochs))+1 {
		return errors.New("checkpoint read retry skipped an unobserved allocation")
	}
	fail := len(proof.FailedEpochs) < 2
	if fail {
		proof.FailedEpochs = append(proof.FailedEpochs, identity.Epoch)
	} else {
		proof.ResumedEpoch = identity.Epoch
	}
	if err := writeNativeHandoffFile(filepath.Join(g.evidence, "checkpoint-read-retry.json"), proof); err != nil {
		return err
	}
	if fail {
		http.Error(w, "temporary checkpoint read failure", http.StatusServiceUnavailable)
		return nil
	}
	forward()
	return nil
}

func (g *nativePublicationGate) verifyCheckpointReadRetry(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	proof := g.checkpointReadRetry
	if proof == nil || len(proof.FailedEpochs) != 2 || proof.FailedEpochs[0] != proof.SourceEpoch+1 || proof.FailedEpochs[1] != proof.SourceEpoch+2 || proof.ResumedEpoch != proof.SourceEpoch+3 {
		return errors.New("checkpoint retry did not observe two failed allocations and a third successful read")
	}
	var valid bool
	var raw []byte
	err := g.pool.QueryRow(ctx, `SELECT p.status='consumed' AND p.controls_reconciled_at IS NOT NULL AND target.epoch=$3
 AND (SELECT count(*)=2 AND bool_and(bad.initialized_at IS NULL
  AND bad.fenced_at IS NOT NULL AND bad.fence_evidence='owning worker joined physical VM closure'
  AND bad.fenced_at<=target.delivered_at AND bad.restored_from_save_id=p.disk_save_id)
  FROM computer_leases bad WHERE bad.environment_id=p.environment_id AND bad.computer_id=p.computer_id
  AND bad.epoch>p.source_lease_epoch AND bad.epoch<target.epoch),p.manifest
 FROM computer_checkpoints p
 JOIN computer_leases target ON(target.environment_id,target.computer_id,target.epoch)=(p.environment_id,p.computer_id,p.target_lease_epoch)
 WHERE p.environment_id=$1 AND p.id=$2`, g.environment, proof.CheckpointID, proof.ResumedEpoch).Scan(&valid, &raw)
	if err != nil {
		return err
	}
	if !valid {
		return errors.New("transient reads did not converge through the same checkpoint after physical closure")
	}
	var manifest computercheckpoint.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return err
	}
	encoded, err := manifest.Encode()
	if err != nil {
		return err
	}
	if fmt.Sprintf("sha256:%x", sha256.Sum256(encoded)) != proof.ManifestDigest {
		return errors.New("restored checkpoint differs from the image retained through transient reads")
	}
	return nil
}
