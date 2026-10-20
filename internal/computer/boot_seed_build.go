package computer

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

type BootSeedBuild struct {
	EnvironmentID, ID, DeploymentID, SpecID uuid.UUID
	ImageID                                 *uuid.UUID
	CPUMillis, MemoryBytes                  int64
	Status                                  string
}

type BootSeedExecutor struct {
	EnvironmentID, BuildID, InstanceID uuid.UUID
	Epoch                              int64
	ChannelCredential                  []byte
}

func (e BootSeedExecutor) Valid() bool {
	return e.EnvironmentID != uuid.Nil() && e.BuildID != uuid.Nil() && e.InstanceID != uuid.Nil() && e.Epoch == 1 && len(e.ChannelCredential) == 32
}

func ReadBootSeedBuild(ctx context.Context, q db.DBTX, env, id uuid.UUID) (BootSeedBuild, error) {
	r := BootSeedBuild{EnvironmentID: env, ID: id}
	err := q.QueryRow(ctx, `SELECT deployment_id,image_spec_id,image_id,cpu_millis,memory_bytes,status FROM boot_seed_builds WHERE environment_id=$1 AND id=$2`, env, id).
		Scan(&r.DeploymentID, &r.SpecID, &r.ImageID, &r.CPUMillis, &r.MemoryBytes, &r.Status)
	return r, err
}

// QueueDeploymentBootSeeds records only the actual definitions in a finalized
// deployment. A missing prepared image leaves cheap metadata waiting; it does
// not cause authored preparation or hold promotion.
func QueueDeploymentBootSeeds(ctx context.Context, tx pgx.Tx, env, deployment uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT image_spec_id,cpu_millis,memory_bytes,min(max_image_age_ms) FROM template_definitions
 WHERE environment_id=$1 AND deployment_id=$2 GROUP BY image_spec_id,cpu_millis,memory_bytes ORDER BY image_spec_id,cpu_millis,memory_bytes`, env, deployment)
	if err != nil {
		return err
	}
	type request struct {
		spec        uuid.UUID
		cpu, memory int64
		maxAge      *int64
	}
	requests, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (request, error) {
		var r request
		err := row.Scan(&r.spec, &r.cpu, &r.memory, &r.maxAge)
		return r, err
	})
	if err != nil {
		return err
	}
	for _, r := range requests {
		if err := LockImageSpecSecrets(ctx, tx, env, r.spec); err != nil {
			return err
		}
	}
	for _, r := range requests {
		if err := LockImageSpec(ctx, tx, env, r.spec); err != nil {
			return err
		}
		var image *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT (SELECT i.id FROM images i
 JOIN image_builds p ON (p.environment_id,p.id)=(i.environment_id,i.image_build_id)
 JOIN image_specs spec ON (spec.environment_id,spec.id)=(i.environment_id,i.image_spec_id)
 WHERE i.environment_id=$1 AND i.image_spec_id=$2 AND i.root_id IS NOT NULL AND p.status='succeeded'
 AND ($3::bigint IS NULL OR extract(epoch FROM clock_timestamp()-i.published_at)*1000<=$3)
 AND `+usableImageSecretsSQL+` ORDER BY i.seq DESC LIMIT 1)`, env, r.spec, r.maxAge).Scan(&image); err != nil {
			return err
		}
		status := "waiting"
		if image != nil {
			status = "queued"
		}
		if _, err := tx.Exec(ctx, `INSERT INTO boot_seed_builds(environment_id,id,deployment_id,image_spec_id,image_id,cpu_millis,memory_bytes,status)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, env, uuid.NewV7(), deployment, r.spec, image, r.cpu, r.memory, status); err != nil {
			return err
		}
	}
	return dbnotify.Notify(ctx, tx, "dispatch")
}

// QueueImageBootSeeds runs under the publishing image's spec lock. Waiting
// deployments consume their first image; a refresh also derives a seed for the
// currently promoted definitions. Historical deployments are not precomputed.
func QueueImageBootSeeds(ctx context.Context, tx pgx.Tx, env, spec, image uuid.UUID) error {
	if _, err := tx.Exec(ctx, `UPDATE boot_seed_builds SET image_id=$3,status='queued'
 WHERE environment_id=$1 AND image_spec_id=$2 AND status='waiting'`, env, spec, image); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT d.deployment_id,d.cpu_millis,d.memory_bytes FROM template_definitions d
 JOIN environments e ON e.id=d.environment_id AND e.current_deployment_id=d.deployment_id
 WHERE d.environment_id=$1 AND d.image_spec_id=$2 ORDER BY d.deployment_id,d.cpu_millis,d.memory_bytes`, env, spec)
	if err != nil {
		return err
	}
	type shape struct {
		deployment  uuid.UUID
		cpu, memory int64
	}
	shapes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (shape, error) {
		var s shape
		err := row.Scan(&s.deployment, &s.cpu, &s.memory)
		return s, err
	})
	if err != nil {
		return err
	}
	for _, s := range shapes {
		if _, err := tx.Exec(ctx, `INSERT INTO boot_seed_builds(environment_id,id,deployment_id,image_spec_id,image_id,cpu_millis,memory_bytes,status)
 VALUES($1,$2,$3,$4,$5,$6,$7,'queued') ON CONFLICT DO NOTHING`, env, uuid.NewV7(), s.deployment, spec, image, s.cpu, s.memory); err != nil {
			return err
		}
	}
	return dbnotify.Notify(ctx, tx, "dispatch")
}

func LockBootSeedBuild(ctx context.Context, tx pgx.Tx, env, id uuid.UUID) (BootSeedBuild, error) {
	r, err := ReadBootSeedBuild(ctx, tx, env, id)
	if err != nil {
		return r, err
	}
	if err := LockImageSpecSecrets(ctx, tx, env, r.SpecID); err != nil {
		return r, err
	}
	if err := LockImageSpec(ctx, tx, env, r.SpecID); err != nil {
		return r, err
	}
	var locked uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM boot_seed_builds WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, id).Scan(&locked)
	if err != nil {
		return r, err
	}
	return ReadBootSeedBuild(ctx, tx, env, id)
}

const usableBootSeedImageSQL = `EXISTS(SELECT 1 FROM images i
 JOIN image_builds p ON (p.environment_id,p.id)=(i.environment_id,i.image_build_id)
 JOIN image_specs spec ON (spec.environment_id,spec.id)=(i.environment_id,i.image_spec_id)
 WHERE i.environment_id=b.environment_id AND i.id=b.image_id AND i.root_id IS NOT NULL AND p.status='succeeded' AND ` + usableImageSecretsSQL + `)`

func AssignBootSeedBuild(ctx context.Context, tx pgx.Tx, a Allocation) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE boot_seed_builds b SET status='running',executor_epoch=1,worker_host_id=$3,worker_epoch=$4,allocation_id=$5,channel_credential_digest=$6,
 reserved_cpu_millis=$7,reserved_memory_bytes=$8,reserved_scratch_bytes=$9,vm_platform_id=$10,vm_vcpu_count=$11,cpu_config_digest=$12,
 deadline_at=clock_timestamp()+(SELECT preparation_timeout_ms FROM environments WHERE id=$1)*interval '1 millisecond'
 WHERE environment_id=$1 AND id=$2 AND status='queued' AND `+usableBootSeedImageSQL,
		a.EnvironmentID, a.OwnerID, a.HostID, a.HostEpoch, a.InstanceID, a.CredentialDigest, a.Shape.CPUMillis, a.Shape.MemoryBytes, a.Shape.ScratchBytes, a.Shape.VMPlatformID, a.Shape.VCPUCount, a.Shape.CPUConfigDigest)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	return true, notifyBootSeedChanged(ctx, tx, a.EnvironmentID, a.OwnerID)
}

func DeliverBootSeedBuild(ctx context.Context, tx pgx.Tx, env, id uuid.UUID) (delivered, expires *time.Time, err error) {
	err = tx.QueryRow(ctx, `UPDATE boot_seed_builds b SET delivered_at=COALESCE(delivered_at,clock_timestamp()),
 executor_expires_at=COALESCE(executor_expires_at,LEAST(deadline_at,clock_timestamp()+interval '1 minute'))
 WHERE environment_id=$1 AND id=$2 AND status='running' AND fenced_at IS NULL AND deadline_at>clock_timestamp()
 AND (delivered_at IS NULL OR executor_expires_at>clock_timestamp()) AND `+usableBootSeedImageSQL+`
 RETURNING delivered_at,executor_expires_at`, env, id).Scan(&delivered, &expires)
	return
}

func CheckBootSeedExecutor(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, ref BootSeedExecutor, live bool) error {
	if !ref.Valid() {
		return ErrInvalidInput
	}
	var digest []byte
	var valid bool
	err := tx.QueryRow(ctx, `SELECT channel_credential_digest,COALESCE(worker_host_id=$3 AND worker_epoch=$4 AND executor_epoch=$5 AND allocation_id=$6
 AND (NOT $7 OR (status='running' AND fenced_at IS NULL AND deadline_at>clock_timestamp() AND executor_expires_at>clock_timestamp()
 AND `+usableBootSeedImageSQL+`)),false) FROM boot_seed_builds b WHERE environment_id=$1 AND id=$2`,
		ref.EnvironmentID, ref.BuildID, host.HostID, host.Epoch, ref.Epoch, ref.InstanceID, live).Scan(&digest, &valid)
	if err != nil {
		return err
	}
	expected := sha256.Sum256(ref.ChannelCredential)
	if !valid || subtle.ConstantTimeCompare(digest, expected[:]) != 1 {
		return ErrDenied
	}
	return nil
}

func RenewBootSeedBuild(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, ref BootSeedExecutor) (time.Time, error) {
	var expires time.Time
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := LockHost(ctx, tx, host); err != nil {
			return err
		}
		if _, err := LockBootSeedBuild(ctx, tx, ref.EnvironmentID, ref.BuildID); err != nil {
			return err
		}
		if err := CheckBootSeedExecutor(ctx, tx, host, ref, true); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `UPDATE boot_seed_builds SET executor_expires_at=LEAST(deadline_at,GREATEST(executor_expires_at,clock_timestamp()+interval '1 minute'))
 WHERE environment_id=$1 AND id=$2 RETURNING executor_expires_at`, ref.EnvironmentID, ref.BuildID).Scan(&expires)
	})
	return expires, hideMissing(err)
}

func FailBootSeedBuild(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, ref BootSeedExecutor, code string) error {
	if code == "" || len(code) > 128 {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := LockHost(ctx, tx, host); err != nil {
			return err
		}
		if _, err := LockBootSeedBuild(ctx, tx, ref.EnvironmentID, ref.BuildID); err != nil {
			return err
		}
		if err := CheckBootSeedExecutor(ctx, tx, host, ref, false); err != nil {
			return err
		}
		return TerminateBootSeedBuild(ctx, tx, ref.EnvironmentID, ref.BuildID, code)
	}))
}

func TerminateBootSeedBuild(ctx context.Context, tx pgx.Tx, env, id uuid.UUID, code string) error {
	_, err := tx.Exec(ctx, `UPDATE boot_seed_builds SET status='failed',error_code=$3 WHERE environment_id=$1 AND id=$2 AND status IN ('waiting','queued','running')`, env, id, code)
	if err != nil {
		return err
	}
	return notifyBootSeedChanged(ctx, tx, env, id)
}

func RecordBootSeedStopped(ctx context.Context, tx pgx.Tx, env, id uuid.UUID, evidence string) error {
	if evidence == "" || len(evidence) > 4096 {
		return errors.New("boot seed physical stop evidence is required")
	}
	_, err := tx.Exec(ctx, `UPDATE boot_seed_builds SET fenced_at=COALESCE(fenced_at,clock_timestamp()),fence_evidence=COALESCE(fence_evidence,$3),
 status=CASE WHEN status='running' THEN 'failed' ELSE status END,error_code=CASE WHEN status='running' THEN 'executor_stopped' ELSE error_code END
 WHERE environment_id=$1 AND id=$2 AND worker_host_id IS NOT NULL`, env, id, evidence)
	if err != nil {
		return err
	}
	return notifyBootSeedChanged(ctx, tx, env, id)
}

func notifyBootSeedChanged(ctx context.Context, tx pgx.Tx, env, id uuid.UUID) error {
	var host *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT worker_host_id FROM boot_seed_builds WHERE environment_id=$1 AND id=$2`, env, id).Scan(&host); err != nil {
		return err
	}
	topics := []string{"dispatch", "capacity"}
	if host != nil {
		topics = append(topics, "worker:"+host.String())
	}
	return dbnotify.Notify(ctx, tx, topics...)
}
