package session

import (
	"bytes"
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// CheckpointPublication is an authenticated source receipt, never customer
// authority. Capturing bytes and every upload must stay under this operation.
type CheckpointPublication struct {
	EnvironmentID uuid.UUID
	Host          workergroup.HostPrincipal
}

// CheckpointPublisher records checkpoints under the capture protocol: it takes
// the capture's Session locks and asks the Computer owner for row writes.
type CheckpointPublisher struct {
	auditPolicy auditpolicy.Policy
	pool        db.TxBeginner
	objects     computer.SaveObjectStore
}

func NewCheckpointPublisher(pool db.TxBeginner, auditPolicy auditpolicy.Policy, objects computer.SaveObjectStore) (*CheckpointPublisher, error) {
	if err := auditPolicy.Validate(); err != nil {
		return nil, err
	}
	if pool == nil || objects == nil {
		return nil, errors.New("checkpoint database and object store are required")
	}
	return &CheckpointPublisher{pool: pool, auditPolicy: auditPolicy, objects: objects}, nil
}

// RegisterCheckpoint pins all runtime objects before upload. It neither makes
// the checkpoint ready nor permits releasing or restoring any physical machine.
func (p *CheckpointPublisher) RegisterCheckpoint(ctx context.Context, ref CheckpointPublication, manifest computercheckpoint.Manifest) error {
	raw, err := manifest.Encode()
	if err != nil {
		return checkpointManifestError(err)
	}
	return hideMissing(db.RunTx(ctx, p.pool, func(tx pgx.Tx) error {
		r, stored, _, err := lockCheckpointPublication(ctx, tx, ref, manifest)
		if err != nil {
			return err
		}
		if len(stored) > 0 {
			if !bytes.Equal(stored, raw) {
				return ErrConflict
			}
			return nil
		}
		if err = computer.ValidateCheckpointRuntime(ctx, tx, ref.Host, manifest.Runtime); err != nil {
			return err
		}
		if err = computer.RecordCheckpointManifest(ctx, tx, ref.EnvironmentID, manifest, raw); err != nil {
			return err
		}
		_, err = computer.CurrentLease(ctx, tx, ref.Host, ref.EnvironmentID, r.Computer, r.SourceEpoch)
		return err
	}))
}

// CompleteCheckpoint verifies storage outside DB locks and then atomically
// records readiness. The source remains sealed. Target ownership and source
// release require their separate physical transition; readiness alone grants no
// execution. An exact committed receipt can replay after source lease expiry
// while the checkpoint remains ready/restoring, never after abort or consumption.
func (p *CheckpointPublisher) CompleteCheckpoint(ctx context.Context, ref CheckpointPublication, manifest computercheckpoint.Manifest) error {
	raw, err := manifest.Encode()
	if err != nil {
		return checkpointManifestError(err)
	}
	var ready bool
	check := func(tx pgx.Tx) error {
		_, stored, committed, e := lockCheckpointPublication(ctx, tx, ref, manifest)
		if e != nil {
			return e
		}
		if len(stored) == 0 {
			return ErrNotReady
		}
		if !bytes.Equal(stored, raw) {
			return ErrConflict
		}
		ready = committed
		return nil
	}
	if err = db.RunTx(ctx, p.pool, check); err != nil {
		return hideMissing(err)
	}
	if ready {
		return nil
	}
	for _, o := range manifest.Objects() {
		observed, e := p.objects.Stat(ctx, o.Digest)
		if e != nil {
			return computer.SaveStorageUnavailable(e)
		}
		if observed.Digest != o.Digest || observed.SizeBytes != o.SizeBytes || observed.MediaType != o.MediaType {
			return ErrConflict
		}
	}
	return hideMissing(db.RunTx(ctx, p.pool, func(tx pgx.Tx) error {
		if err = check(tx); err != nil {
			return err
		}
		if ready {
			return nil
		}
		var published, head bool
		if err = tx.QueryRow(ctx, `SELECT s.status='published',COALESCE(c.recovery_save_id=s.id,false) FROM checkpoints p JOIN computer_saves s ON (s.environment_id,s.id)=(p.environment_id,p.disk_save_id) JOIN computers c ON (c.environment_id,c.id)=(p.environment_id,p.computer_id) WHERE p.environment_id=$1 AND p.id=$2`, ref.EnvironmentID, manifest.CheckpointID).Scan(&published, &head); err != nil {
			return err
		}
		if !published || !head {
			return ErrNotReady
		}
		if err = computer.PublishCheckpointObjects(ctx, tx, ref.EnvironmentID, manifest); err != nil {
			return err
		}
		if _, err = computer.CurrentLease(ctx, tx, ref.Host, ref.EnvironmentID, manifest.ComputerID, manifest.LeaseEpoch); err != nil {
			return err
		}
		if err := computer.MarkCheckpointReady(ctx, p.auditPolicy, tx, ref.EnvironmentID, manifest.CheckpointID); err != nil {
			return err
		}
		return audit.Append(ctx, tx, p.auditPolicy, audit.Event{Evidence: audit.AssignedWorker(ref.Host.HostID, ref.Host.HostClaimVersion, "computer", manifest.ComputerID, manifest.LeaseEpoch), EnvironmentID: ref.EnvironmentID, ComputerID: manifest.ComputerID, TargetKind: "computer", TargetID: manifest.ComputerID, Action: "computer.checkpoint_published", Details: audit.CheckpointDetails{CheckpointID: manifest.CheckpointID, Generation: manifest.LeaseEpoch}})
	}))
}

func lockCheckpointPublication(ctx context.Context, tx pgx.Tx, ref CheckpointPublication, m computercheckpoint.Manifest) (CaptureRecord, []byte, bool, error) {
	var r CaptureRecord
	var raw []byte
	if err := computer.LockHost(ctx, tx, ref.Host); err != nil {
		return r, nil, false, err
	}
	r, err := LockCapture(ctx, tx, ref.EnvironmentID, m.CheckpointID)
	if err != nil {
		return r, nil, false, err
	}
	var digest []byte
	var committed bool
	if err = tx.QueryRow(ctx, `SELECT manifest,capture_digest,ready_at IS NOT NULL FROM checkpoints WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, m.CheckpointID).Scan(&raw, &digest, &committed); err != nil {
		return r, nil, false, err
	}
	var instance, host uuid.UUID
	var epoch int64
	if err = tx.QueryRow(ctx, `SELECT allocation_id,worker_host_id,worker_epoch FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, ref.EnvironmentID, r.Computer, r.SourceEpoch).Scan(&instance, &host, &epoch); err != nil {
		return r, nil, false, err
	}
	if host != ref.Host.HostID || epoch != ref.Host.Epoch {
		return r, nil, false, ErrDenied
	}
	if r.Computer != m.ComputerID || r.SourceEpoch != m.LeaseEpoch || r.Version != m.ControlVersion {
		return r, nil, false, ErrConflict
	}
	if instance != m.InstanceID || !bytes.Equal(digest, m.CaptureDigest) || len(r.Members) != len(m.Members) {
		return r, nil, false, ErrConflict
	}
	members := make(map[uuid.UUID]int64, len(m.Members))
	for _, v := range m.Members {
		members[v.SessionID] = v.ProcessEpoch
	}
	for _, v := range r.Members {
		if members[v.Session] != v.Epoch {
			return r, nil, false, ErrConflict
		}
	}
	if committed {
		if r.State != "ready" && r.State != "restoring" {
			return r, nil, false, ErrNotReady
		}
		return r, raw, true, nil
	}
	if r.State != "sealed" {
		return r, nil, false, ErrNotReady
	}
	if _, err = computer.CurrentLease(ctx, tx, ref.Host, ref.EnvironmentID, r.Computer, r.SourceEpoch); err != nil {
		return r, nil, false, err
	}
	if err = r.requireMembers(ctx, tx, ref.EnvironmentID); err != nil {
		return r, nil, false, err
	}
	var root *string
	if err = tx.QueryRow(ctx, `SELECT 'sha256:'||encode(captured_root_digest,'hex') FROM computer_saves WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, r.Save).Scan(&root); err != nil {
		return r, nil, false, err
	}
	identity, _ := m.Disk.Digest()
	if root == nil {
		return r, nil, false, ErrNotReady
	}
	if *root != identity {
		return r, nil, false, ErrConflict
	}
	return r, raw, false, nil
}

func checkpointManifestError(err error) error {
	if errors.Is(err, computercheckpoint.ErrInvalidManifest) {
		return ErrInvalidInput
	}
	return err
}
