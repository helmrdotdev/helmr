package computer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// ImageBuild identifies one execution of immutable inputs. Its outcome and its
// physical executor's fence are distinct; neither expiry nor failure proves stop.
type ImageBuild struct {
	EnvironmentID uuid.UUID
	ID            uuid.UUID
	SpecID        uuid.UUID
	Status        string
	Deadline      time.Time
}

// A retained attempt can satisfy fresh demand only while its executor and
// exposed Secret authority are usable. Physical exclusion alone is insufficient.
const usableImageBuildSQL = `p.status IN ('queued','running') AND p.deadline_at>clock_timestamp()
   AND (p.worker_host_id IS NULL OR ((p.delivered_at IS NULL OR p.executor_expires_at>clock_timestamp()) AND EXISTS (
     SELECT 1 FROM worker_hosts h WHERE h.id=p.worker_host_id AND h.current_epoch=p.worker_epoch AND h.status IN ('active','draining'))))
   AND NOT EXISTS (SELECT 1 FROM secret_exposures x JOIN secrets s ON (s.environment_id,s.id)=(x.environment_id,x.secret_id)
     WHERE x.environment_id=p.environment_id AND x.image_build_id=p.id AND s.status='revoked')`

// AttachImageBuild participates in its caller's admission transaction. It acquires
// Environment and preparation owners before the Computer, so callers must invoke
// it before acquiring existing Session-root or Computer locks. An error leaves
// rollback responsibility with the caller.
func AttachImageBuild(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID) (ImageBuild, error) {
	var result ImageBuild
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM environments WHERE id=$1 FOR NO KEY UPDATE`, env).Scan(&locked); err != nil {
		return result, err
	}
	var spec uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT image_spec_id FROM computers WHERE environment_id=$1 AND id=$2`, env, computer).Scan(&spec); err != nil {
		return result, err
	}
	if err := LockImageSpecSecrets(ctx, tx, env, spec); err != nil {
		return result, err
	}
	if err := LockImageSpec(ctx, tx, env, spec); err != nil {
		return result, err
	}
	var attached *uuid.UUID
	var deadline time.Time
	var live bool
	if err := tx.QueryRow(ctx, `SELECT image_build_id,preparation_deadline_at,initial_root_digest IS NULL AND preparation_failed_at IS NULL AND deleted_at IS NULL AND preparation_deadline_at>clock_timestamp()
   FROM computers WHERE environment_id=$1 AND id=$2 AND image_spec_id=$3 FOR NO KEY UPDATE`, env, computer, spec).Scan(&attached, &deadline, &live); err != nil {
		return result, err
	}
	if attached != nil {
		var err error
		result, err = ReadImageBuild(ctx, tx, env, *attached)
		return result, err
	}
	if !live {
		return result, ErrNotReady
	}
	var attempt uuid.UUID
	var usable bool
	err := tx.QueryRow(ctx, `SELECT p.id,`+usableImageBuildSQL+`
   FROM image_builds p WHERE p.environment_id=$1 AND p.image_spec_id=$2
   AND (p.status IN ('queued','running') OR (p.worker_host_id IS NOT NULL AND p.fenced_at IS NULL))`, env, spec).Scan(&attempt, &usable)
	if errors.Is(err, pgx.ErrNoRows) {
		var successorPending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM image_builds p
 JOIN image_specs spec ON (spec.environment_id,spec.id)=(p.environment_id,p.image_spec_id)
 WHERE p.environment_id=$1 AND p.image_spec_id=$2
 AND NOT EXISTS(SELECT 1 FROM image_builds successor WHERE successor.environment_id=p.environment_id AND successor.predecessor_build_id=p.id)
 AND `+imageBuildSuccessorEligibilitySQL+`)`, env, spec).Scan(&successorPending); err != nil {
			return result, err
		}
		if successorPending {
			return result, ErrNotReady
		}
		usable = true
		attempt = uuid.NewV7()
		if _, err = tx.Exec(ctx, `INSERT INTO image_builds(environment_id,id,image_spec_id,idempotency_key,deadline_at)
    VALUES($1,$2,$3,$4,$5)`, env, attempt, spec, "computer:"+computer.String(), deadline); err != nil {
			return result, err
		}
		if err = notifyImageBuildChanged(ctx, tx, env, attempt); err != nil {
			return result, err
		}
	} else if err != nil {
		return result, err
	}
	if err == nil && !usable {
		return result, ErrNotReady
	}
	if _, err = tx.Exec(ctx, `UPDATE computers SET image_build_id=$3 WHERE environment_id=$1 AND id=$2`, env, computer, attempt); err != nil {
		return result, err
	}
	result, err = ReadImageBuild(ctx, tx, env, attempt)
	return result, err
}

func LockImageSpec(ctx context.Context, tx pgx.Tx, env, spec uuid.UUID) error {
	var locked uuid.UUID
	return tx.QueryRow(ctx, `SELECT id FROM image_specs WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, spec).Scan(&locked)
}
func ReadImageBuild(ctx context.Context, tx pgx.Tx, env, id uuid.UUID) (ImageBuild, error) {
	result := ImageBuild{EnvironmentID: env, ID: id}
	err := tx.QueryRow(ctx, `SELECT image_spec_id,status,deadline_at FROM image_builds WHERE environment_id=$1 AND id=$2`, env, id).Scan(&result.SpecID, &result.Status, &result.Deadline)
	return result, err
}

// ImageBuildExecutor is the exact private allocation selected by the trusted
// allocator. The allocator retains this identity and credential across retries;
// neither a new connection nor an expired lease permits another execution.
type ImageBuildExecutor struct {
	EnvironmentID     uuid.UUID
	ImageBuildID      uuid.UUID
	InstanceID        uuid.UUID
	Epoch             int64
	ChannelCredential []byte
}

func (e ImageBuildExecutor) Valid() bool {
	return e.EnvironmentID != uuid.Nil() && e.ImageBuildID != uuid.Nil() && e.InstanceID != uuid.Nil() && e.Epoch > 0 && len(e.ChannelCredential) == 32
}

// Host operations follow supply -> Secrets (if any) -> spec -> attempt. They do
// not acquire Environment or Computer locks. Waiter settlement takes spec locks
// before its complete Session-root/Computer set and the deciding attempt locks.
func lockImageBuildExecutor(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, ref ImageBuildExecutor, live bool) error {
	if !ref.Valid() {
		return ErrInvalidInput
	}
	if err := LockHost(ctx, tx, host); err != nil {
		return err
	}
	return lockImageBuildExecutorOnHost(ctx, tx, host, ref, live)
}

// lockImageBuildExecutorOnHost requires the caller to hold LockHost,
// allowing declared Secret locks to be acquired before the spec/attempt locks.
func lockImageBuildExecutorOnHost(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, ref ImageBuildExecutor, live bool) error {
	p, err := ReadImageBuild(ctx, tx, ref.EnvironmentID, ref.ImageBuildID)
	if err != nil {
		return err
	}
	if err = LockImageSpec(ctx, tx, p.EnvironmentID, p.SpecID); err != nil {
		return err
	}
	var id uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT id FROM image_builds WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, p.EnvironmentID, p.ID).Scan(&id); err != nil {
		return err
	}
	return CheckImageBuildExecutor(ctx, tx, host, ref, live)
}
func CheckImageBuildExecutor(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, ref ImageBuildExecutor, live bool) error {
	var digest []byte
	var valid bool
	err := tx.QueryRow(ctx, `SELECT channel_credential_digest,COALESCE(worker_host_id=$3 AND worker_epoch=$4 AND executor_epoch=$5 AND allocation_id=$6
  AND (NOT $7 OR (status='running' AND fenced_at IS NULL AND deadline_at>clock_timestamp() AND executor_expires_at>clock_timestamp()
   AND NOT EXISTS(SELECT 1 FROM secret_exposures x JOIN secrets s ON (s.environment_id,s.id)=(x.environment_id,x.secret_id) WHERE x.environment_id=$1 AND x.image_build_id=$2 AND s.status='revoked'))), false)
  FROM image_builds WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, ref.ImageBuildID, host.HostID, host.Epoch, ref.Epoch, ref.InstanceID, live).Scan(&digest, &valid)
	if err != nil {
		return err
	}
	presented := sha256.Sum256(ref.ChannelCredential)
	if !valid || subtle.ConstantTimeCompare(digest, presented[:]) != 1 {
		return ErrDenied
	}
	return nil
}
func RenewImageBuild(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, ref ImageBuildExecutor) (time.Time, error) {
	var expires time.Time
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockImageBuildExecutor(ctx, tx, host, ref, true); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `UPDATE image_builds SET executor_expires_at=LEAST(deadline_at,GREATEST(executor_expires_at,clock_timestamp()+interval '1 minute'))
   WHERE environment_id=$1 AND id=$2 RETURNING executor_expires_at`, ref.EnvironmentID, ref.ImageBuildID).Scan(&expires)
	})
	if err != nil {
		return time.Time{}, hideMissing(err)
	}
	return expires, nil
}

// ExposedSecret is encrypted material whose owner exposure transaction must
// commit before decryption or delivery. It carries no plaintext Secret value.
type ExposedSecret struct {
	Target         string
	Kind           string
	Mode           string
	AllowedOrigins []string
	SecretID       uuid.UUID
	VersionID      uuid.UUID
	Version        int64
	Nonce          []byte
	Ciphertext     []byte
}

func RecordImageBuildExposure(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, host workergroup.HostPrincipal, ref ImageBuildExecutor) ([]ExposedSecret, error) {
	if !ref.Valid() {
		return nil, ErrInvalidInput
	}
	var result []ExposedSecret
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := LockHost(ctx, tx, host); err != nil {
			return err
		}
		var spec uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT s.id FROM image_builds p JOIN image_specs s ON (s.environment_id,s.id)=(p.environment_id,p.image_spec_id)
   WHERE p.environment_id=$1 AND p.id=$2`, ref.EnvironmentID, ref.ImageBuildID).Scan(&spec); err != nil {
			return err
		}
		bindings, err := readImageBuildBindings(ctx, tx, ref.EnvironmentID, spec)
		if err != nil {
			return err
		}
		set := map[uuid.UUID]bool{}
		for _, binding := range bindings {
			set[binding.Secret] = true
		}
		ids := make([]uuid.UUID, 0, len(set))
		for id := range set {
			ids = append(ids, id)
		}
		slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
		versions := map[uuid.UUID]uuid.UUID{}
		if len(ids) > 0 {
			rows, err := tx.Query(ctx, `SELECT id,current_version_id FROM secrets WHERE environment_id=$1 AND id=ANY($2::uuid[]) AND status='active' ORDER BY id FOR SHARE`, ref.EnvironmentID, ids)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id, version uuid.UUID
				if err = rows.Scan(&id, &version); err != nil {
					rows.Close()
					return err
				}
				versions[id] = version
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if len(versions) != len(ids) {
				return ErrDenied
			}
		}
		if err := LockImageSpec(ctx, tx, ref.EnvironmentID, spec); err != nil {
			return err
		}
		var locked uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM image_builds WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, ref.EnvironmentID, ref.ImageBuildID).Scan(&locked); err != nil {
			return err
		}
		if err := CheckImageBuildExecutor(ctx, tx, host, ref, true); err != nil {
			return err
		}
		var sealed bool
		if err := tx.QueryRow(ctx, `SELECT logical_bytes IS NOT NULL FROM image_builds WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, ref.ImageBuildID).Scan(&sealed); err != nil {
			return err
		}
		if sealed {
			return ErrDenied
		}
		rows, err := tx.Query(ctx, `SELECT secret_id,version_id FROM secret_exposures WHERE environment_id=$1 AND image_build_id=$2`, ref.EnvironmentID, ref.ImageBuildID)
		if err != nil {
			return err
		}
		prior := map[uuid.UUID]uuid.UUID{}
		for rows.Next() {
			var id, version uuid.UUID
			if err = rows.Scan(&id, &version); err != nil {
				rows.Close()
				return err
			}
			prior[id] = version
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(prior) > 0 {
			if len(prior) != len(ids) {
				return ErrConflict
			}
			for id := range prior {
				if !set[id] {
					return ErrConflict
				}
			}
			versions = prior // Reconnect never replaces an attempt's already exposed version.
		} else {
			for _, id := range ids {
				var exposureID uuid.UUID
				if err = tx.QueryRow(ctx, `INSERT INTO secret_exposures(id,environment_id,image_build_id,secret_id,version_id) VALUES(uuidv7(),$1,$2,$3,$4) RETURNING id`, ref.EnvironmentID, ref.ImageBuildID, id, versions[id]).Scan(&exposureID); err != nil {
					return err
				}
				actor := audit.AssignedWorker(host.HostID, host.HostClaimVersion, "image_build", ref.ImageBuildID, ref.Epoch)
				if err = audit.Append(ctx, tx, auditPolicy, audit.Event{Evidence: actor, EnvironmentID: ref.EnvironmentID, Action: "secret.exposed", TargetKind: "secret", TargetID: id, Details: audit.SecretExposureDetails{ExposureID: exposureID, VersionID: versions[id], RevocationGeneration: 0, ImageBuildID: ref.ImageBuildID}}); err != nil {
					return err
				}

			}
		}
		for _, binding := range bindings {
			id := binding.Secret
			value := ExposedSecret{Target: binding.Target, Kind: binding.Kind, Mode: binding.Mode, AllowedOrigins: binding.Origins, SecretID: id, VersionID: versions[id]}
			if err = tx.QueryRow(ctx, `SELECT version,nonce,ciphertext FROM secret_versions WHERE secret_id=$1 AND id=$2`, id, value.VersionID).Scan(&value.Version, &value.Nonce, &value.Ciphertext); err != nil {
				return err
			}
			result = append(result, value)
		}
		// Exposure acquisition can wait for another writer. Recheck the time predicate
		// after every deciding lock and insertion, before any ciphertext leaves the tx.
		return CheckImageBuildExecutor(ctx, tx, host, ref, true)
	})
	if err != nil {
		return nil, hideMissing(err)
	}
	slices.SortFunc(result, func(a, b ExposedSecret) int { return strings.Compare(a.Target, b.Target) })
	return result, nil
}

// FailImageBuild records an executor-observed failure without releasing its
// writer exclusion. Late reports can reconcile the original terminal outcome.
func FailImageBuild(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, ref ImageBuildExecutor, code string) error {
	if code == "" || len(code) > 256 || !utf8.ValidString(code) {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockImageBuildExecutor(ctx, tx, host, ref, false); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE image_builds SET proxy_ca_certificate=NULL,proxy_ca_private_key_nonce=NULL,proxy_ca_private_key_ciphertext=NULL,proxy_ca_not_after=NULL,status='failed',error_code=$3
   WHERE environment_id=$1 AND id=$2 AND status='running'`, ref.EnvironmentID, ref.ImageBuildID, code)
		if err != nil {
			return err
		}
		return notifyImageBuildChanged(ctx, tx, ref.EnvironmentID, ref.ImageBuildID)
	}))
}

// ImageBuildIdentity names physical custody independently of execution authority.
// A Host may have received only this identity before a delivery became ineligible.
type ImageBuildIdentity struct {
	EnvironmentID uuid.UUID
	ImageBuildID  uuid.UUID
	InstanceID    uuid.UUID
	Epoch         int64
}

// ObserveImageBuildStopped accepts only the owning Host's joined physical VM
// closure. It does not need an execution credential: delivery may never have
// arrived. Provider absence and successor cleanup have separate proof paths.
func ObserveImageBuildStopped(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, ref ImageBuildIdentity) error {
	if ref.EnvironmentID == uuid.Nil() || ref.ImageBuildID == uuid.Nil() || ref.InstanceID == uuid.Nil() || ref.Epoch <= 0 {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := LockHost(ctx, tx, host); err != nil {
			return err
		}
		p, err := ReadImageBuild(ctx, tx, ref.EnvironmentID, ref.ImageBuildID)
		if err != nil {
			return err
		}
		if err := LockImageSpec(ctx, tx, ref.EnvironmentID, p.SpecID); err != nil {
			return err
		}
		var owned bool
		if err := tx.QueryRow(ctx, `SELECT COALESCE(worker_host_id=$3 AND worker_epoch=$4 AND executor_epoch=$5 AND allocation_id=$6,false)
 FROM image_builds WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, ref.EnvironmentID, ref.ImageBuildID, host.HostID, host.Epoch, ref.Epoch, ref.InstanceID).Scan(&owned); err != nil {
			return err
		}
		if !owned {
			return ErrDenied
		}
		return RecordImageBuildStopped(ctx, tx, ref.EnvironmentID, ref.ImageBuildID, "owning worker joined physical VM closure")
	}))
}
func RecordImageBuildStopped(ctx context.Context, tx pgx.Tx, env, id uuid.UUID, evidence string) error {
	_, err := tx.Exec(ctx, `UPDATE image_builds SET proxy_ca_certificate=NULL,proxy_ca_private_key_nonce=NULL,proxy_ca_private_key_ciphertext=NULL,proxy_ca_not_after=NULL,fenced_at=COALESCE(fenced_at,clock_timestamp()),fence_evidence=COALESCE(fence_evidence,$3),
  status=CASE WHEN status IN ('queued','running') THEN 'failed' ELSE status END,
  error_code=CASE WHEN status IN ('queued','running') THEN 'executor_stopped' ELSE error_code END WHERE environment_id=$1 AND id=$2 AND worker_host_id IS NOT NULL`, env, id, evidence)
	if err != nil {
		return err
	}
	return notifyImageBuildChanged(ctx, tx, env, id)
}
