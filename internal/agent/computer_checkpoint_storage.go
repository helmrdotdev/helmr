package agent

import (
	"bytes"
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// CheckpointPublication is an authenticated source receipt, never customer
// authority. Capturing bytes and every upload must stay under this operation.
type CheckpointPublication struct {
	EnvironmentID uuid.UUID
	Host          workergroup.HostPrincipal
}

// RegisterCheckpoint pins all runtime objects before upload. It neither makes
// the checkpoint ready nor permits releasing or restoring any physical machine.
func (p *SavePublisher) RegisterCheckpoint(ctx context.Context, ref CheckpointPublication, manifest computercheckpoint.Manifest) error {
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
		if err = validateCheckpointRuntime(ctx, tx, ref.Host, manifest.Runtime); err != nil {
			return err
		}
		for _, o := range manifest.Objects() {
			if _, err = tx.Exec(ctx, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,$2) ON CONFLICT DO NOTHING`, o.Digest, o.SizeBytes); err != nil {
				return err
			}
			var size int64
			var available bool
			if err = tx.QueryRow(ctx, `SELECT size_bytes,not_retired FROM cas_blobs WHERE digest=$1 FOR KEY SHARE`, o.Digest).Scan(&size, &available); err != nil {
				return err
			}
			if size != o.SizeBytes || !available {
				return ErrConflict
			}
			if _, err = tx.Exec(ctx, `INSERT INTO computer_checkpoint_objects(environment_id,checkpoint_id,role,digest,size_bytes,media_type) VALUES($1,$2,$3,$4,$5,$6)`, ref.EnvironmentID, manifest.CheckpointID, o.Role, o.Digest, o.SizeBytes, o.MediaType); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE computer_checkpoints SET manifest=$3,vm_platform_id=$4 WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, manifest.CheckpointID, raw, manifest.Runtime.RuntimeID); err != nil {
			return err
		}
		_, err = currentComputerLease(ctx, tx, ref.Host, ref.EnvironmentID, r.Computer, r.SourceEpoch)
		return err
	}))
}

// CompleteCheckpoint verifies storage outside DB locks and then atomically
// records readiness. The source remains sealed. Target ownership and source
// release require their separate physical transition; readiness alone grants no
// execution. An exact committed receipt can replay after source lease expiry
// while the checkpoint remains ready/restoring, never after abort or consumption.
func (p *SavePublisher) CompleteCheckpoint(ctx context.Context, ref CheckpointPublication, manifest computercheckpoint.Manifest) error {
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
			return saveStorageUnavailable(e)
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
		if err = tx.QueryRow(ctx, `SELECT s.status='published',COALESCE(c.recovery_save_id=s.id,false) FROM computer_checkpoints p JOIN computer_saves s ON (s.environment_id,s.id)=(p.environment_id,p.disk_save_id) JOIN computers c ON (c.environment_id,c.id)=(p.environment_id,p.computer_id) WHERE p.environment_id=$1 AND p.id=$2`, ref.EnvironmentID, manifest.CheckpointID).Scan(&published, &head); err != nil {
			return err
		}
		if !published || !head {
			return ErrNotReady
		}
		var org uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT org_id FROM environments WHERE id=$1`, ref.EnvironmentID).Scan(&org); err != nil {
			return err
		}
		// Typed pins must still be present with their exact registered descriptors.
		for _, o := range manifest.Objects() {
			var pinned bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_checkpoint_objects WHERE environment_id=$1 AND checkpoint_id=$2 AND role=$3 AND digest=$4 AND size_bytes=$5 AND media_type=$6)`, ref.EnvironmentID, manifest.CheckpointID, o.Role, o.Digest, o.SizeBytes, o.MediaType).Scan(&pinned); err != nil {
				return err
			}
			if !pinned {
				return ErrConflict
			}
			if _, err = db.New(tx).UpsertCasObject(ctx, db.UpsertCasObjectParams{OrgID: pgvalue.UUID(org), Digest: o.Digest, SizeBytes: o.SizeBytes, MediaType: o.MediaType}); err != nil {
				return err
			}
		}
		if _, err = currentComputerLease(ctx, tx, ref.Host, ref.EnvironmentID, manifest.ComputerID, manifest.LeaseEpoch); err != nil {
			return err
		}
		if err = requireComputerImageAllowed(ctx, tx, ref.EnvironmentID, manifest.ComputerID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE computer_checkpoints SET status='ready',ready_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, manifest.CheckpointID)
		return err
	}))
}

func lockCheckpointPublication(ctx context.Context, tx pgx.Tx, ref CheckpointPublication, m computercheckpoint.Manifest) (captureRecord, []byte, bool, error) {
	var r captureRecord
	var raw []byte
	if err := lockComputerHost(ctx, tx, ref.Host); err != nil {
		return r, nil, false, err
	}
	r, err := lockCapture(ctx, tx, ref.EnvironmentID, m.CheckpointID)
	if err != nil {
		return r, nil, false, err
	}
	var digest []byte
	var committed bool
	if err = tx.QueryRow(ctx, `SELECT manifest,capture_digest,ready_at IS NOT NULL FROM computer_checkpoints WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, m.CheckpointID).Scan(&raw, &digest, &committed); err != nil {
		return r, nil, false, err
	}
	var instance, host uuid.UUID
	var epoch int64
	if err = tx.QueryRow(ctx, `SELECT computer_instance_id,worker_host_id,worker_epoch FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, ref.EnvironmentID, r.Computer, r.SourceEpoch).Scan(&instance, &host, &epoch); err != nil {
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
	if err = requireComputerImageAllowed(ctx, tx, ref.EnvironmentID, r.Computer); err != nil {
		return r, nil, false, err
	}
	if r.State != "sealed" {
		return r, nil, false, ErrNotReady
	}
	if _, err = currentComputerLease(ctx, tx, ref.Host, ref.EnvironmentID, r.Computer, r.SourceEpoch); err != nil {
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

func validateCheckpointRuntime(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, r vm.CheckpointIdentity) error {
	if r.RuntimeBackend != "firecracker" || !sha256sum.ValidDigest(r.VMConfigDigest) || r.VMVCPUCount <= 0 {
		return ErrConflict
	}
	var valid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM worker_hosts h JOIN vm_platforms p ON p.id=h.vm_platform_id JOIN worker_pool_cpu_shapes s ON s.worker_pool_id=h.worker_pool_id WHERE h.id=$1 AND p.id=$2 AND p.arch=$3 AND p.contract=$4 AND p.kernel_digest=$5 AND p.initramfs_digest=$6 AND p.rootfs_digest=$7 AND s.vcpu_count=$8 AND s.cpu_config_digest=$9 AND h.per_vm_cpu_millis >= $8::bigint*1000)`, host.HostID, r.RuntimeID, r.RuntimeArch, r.VMRuntimeContract, r.KernelDigest, r.InitramfsDigest, r.RootfsDigest, r.VMVCPUCount, r.CPUConfigDigest).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrConflict
	}
	return nil
}

func checkpointManifestError(err error) error {
	if errors.Is(err, computercheckpoint.ErrInvalidManifest) {
		return ErrInvalidInput
	}
	return err
}
