package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// Preparation identifies one execution of immutable inputs. Its outcome and its
// physical executor's fence are distinct; neither expiry nor failure proves stop.
type Preparation struct {
	EnvironmentID uuid.UUID
	ID            uuid.UUID
	SpecID        uuid.UUID
	Status        string
	Deadline      time.Time
}

// AttachComputerPreparation attaches an already admitted, waiting Computer to an
// execution. It does not authorize or create a Computer. The Computer is the
// durable demand owner, so lost acknowledgements return its original attachment
// even after that attempt terminates. A new retry key cannot silently replay code.
// Fresh demand waits behind an eligible pending rotation successor, then joins
// its existing execution window without extending either deadline.
func AttachComputerPreparation(ctx context.Context, pool db.TxBeginner, env, computer uuid.UUID) (Preparation, error) {
	var result Preparation
	if env == uuid.Nil() || computer == uuid.Nil() {
		return result, ErrInvalidInput
	}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		result, err = attachComputerPreparation(ctx, tx, env, computer)
		return err
	})
	if err != nil {
		return Preparation{}, hideMissing(err)
	}
	return result, nil
}

// attachComputerPreparation participates in its caller's admission transaction. It acquires
// Environment and preparation owners before the Computer, so callers must invoke
// it before acquiring existing Session-root or Computer locks. An error leaves
// rollback responsibility with the caller.
func attachComputerPreparation(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID) (Preparation, error) {
	var result Preparation
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM environments WHERE id=$1 AND retired_at IS NULL FOR NO KEY UPDATE`, env).Scan(&locked); err != nil {
		return result, err
	}
	var spec uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT preparation_spec_id FROM computers WHERE environment_id=$1 AND id=$2`, env, computer).Scan(&spec); err != nil {
		return result, err
	}
	if err := lockPreparationImageSecrets(ctx, tx, env, spec); err != nil {
		return result, err
	}
	if err := lockPreparationSpec(ctx, tx, env, spec); err != nil {
		return result, err
	}
	var attached *uuid.UUID
	var deadline time.Time
	var live bool
	if err := tx.QueryRow(ctx, `SELECT preparation_id,preparation_deadline_at,initial_root_digest IS NULL AND preparation_failed_at IS NULL AND deleted_at IS NULL AND preparation_deadline_at>clock_timestamp()
   FROM computers WHERE environment_id=$1 AND id=$2 AND preparation_spec_id=$3 FOR NO KEY UPDATE`, env, computer, spec).Scan(&attached, &deadline, &live); err != nil {
		return result, err
	}
	if attached != nil {
		var err error
		result, err = readPreparation(ctx, tx, env, *attached)
		return result, err
	}
	if !live {
		return result, ErrNotReady
	}
	var attempt uuid.UUID
	var usable bool
	err := tx.QueryRow(ctx, `SELECT p.id,p.status IN ('queued','running') AND p.deadline_at>clock_timestamp()
   AND (p.worker_host_id IS NULL OR ((p.delivered_at IS NULL OR p.executor_expires_at>clock_timestamp()) AND EXISTS (
     SELECT 1 FROM worker_hosts h WHERE h.id=p.worker_host_id AND h.current_epoch=p.worker_epoch AND h.status IN ('active','draining'))))
   AND NOT EXISTS (SELECT 1 FROM secret_exposures x JOIN secrets s ON (s.environment_id,s.id)=(x.environment_id,x.secret_id)
     WHERE x.environment_id=p.environment_id AND x.preparation_id=p.id AND s.status='revoked')
   FROM computer_preparations p WHERE p.environment_id=$1 AND p.preparation_spec_id=$2
   AND (p.status IN ('queued','running') OR (p.worker_host_id IS NOT NULL AND p.fenced_at IS NULL))`, env, spec).Scan(&attempt, &usable)
	if errors.Is(err, pgx.ErrNoRows) {
		var successorPending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_preparations p
 JOIN computer_preparation_specs spec ON (spec.environment_id,spec.id)=(p.environment_id,p.preparation_spec_id)
 WHERE p.environment_id=$1 AND p.preparation_spec_id=$2
 AND NOT EXISTS(SELECT 1 FROM computer_preparations successor WHERE successor.environment_id=p.environment_id AND successor.successor_of=p.id)
 AND `+preparationSuccessorEligibilitySQL+`)`, env, spec).Scan(&successorPending); err != nil {
			return result, err
		}
		if successorPending {
			return result, ErrNotReady
		}
		usable = true
		attempt = uuid.NewV7()
		if _, err = tx.Exec(ctx, `INSERT INTO computer_preparations(environment_id,id,preparation_spec_id,retry_key,deadline_at)
    VALUES($1,$2,$3,$4,$5)`, env, attempt, spec, "computer:"+computer.String(), deadline); err != nil {
			return result, err
		}
	} else if err != nil {
		return result, err
	}
	if err == nil && !usable {
		return result, ErrNotReady
	}
	if _, err = tx.Exec(ctx, `UPDATE computers SET preparation_id=$3 WHERE environment_id=$1 AND id=$2`, env, computer, attempt); err != nil {
		return result, err
	}
	result, err = readPreparation(ctx, tx, env, attempt)
	return result, err
}

func lockPreparationSpec(ctx context.Context, tx pgx.Tx, env, spec uuid.UUID) error {
	var locked uuid.UUID
	return tx.QueryRow(ctx, `SELECT id FROM computer_preparation_specs WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, spec).Scan(&locked)
}
func readPreparation(ctx context.Context, tx pgx.Tx, env, id uuid.UUID) (Preparation, error) {
	result := Preparation{EnvironmentID: env, ID: id}
	err := tx.QueryRow(ctx, `SELECT preparation_spec_id,status,deadline_at FROM computer_preparations WHERE environment_id=$1 AND id=$2`, env, id).Scan(&result.SpecID, &result.Status, &result.Deadline)
	return result, err
}

// PreparationExecutor is the exact private allocation selected by the trusted
// allocator. The allocator retains this identity and credential across retries;
// neither a new connection nor an expired lease permits another execution.
type PreparationExecutor struct {
	EnvironmentID     uuid.UUID
	PreparationID     uuid.UUID
	InstanceID        uuid.UUID
	Epoch             int64
	ChannelCredential []byte
}

func (e PreparationExecutor) valid() bool {
	return e.EnvironmentID != uuid.Nil() && e.PreparationID != uuid.Nil() && e.InstanceID != uuid.Nil() && e.Epoch > 0 && len(e.ChannelCredential) == 32
}

// Host operations follow supply -> Secrets (if any) -> spec -> attempt. They do
// not acquire Environment or Computer locks. Waiter settlement takes spec locks
// before its complete Session-root/Computer set and the deciding attempt locks.
func lockPreparationExecutor(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, ref PreparationExecutor, live bool) error {
	if !ref.valid() {
		return ErrInvalidInput
	}
	if err := lockComputerHost(ctx, tx, host); err != nil {
		return err
	}
	return lockPreparationExecutorOnHost(ctx, tx, host, ref, live)
}

// lockPreparationExecutorOnHost requires the caller to hold lockComputerHost,
// allowing declared Secret locks to be acquired before the spec/attempt locks.
func lockPreparationExecutorOnHost(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, ref PreparationExecutor, live bool) error {
	p, err := readPreparation(ctx, tx, ref.EnvironmentID, ref.PreparationID)
	if err != nil {
		return err
	}
	if err = lockPreparationSpec(ctx, tx, p.EnvironmentID, p.SpecID); err != nil {
		return err
	}
	var id uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT id FROM computer_preparations WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, p.EnvironmentID, p.ID).Scan(&id); err != nil {
		return err
	}
	return checkPreparationExecutor(ctx, tx, host, ref, live)
}
func checkPreparationExecutor(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, ref PreparationExecutor, live bool) error {
	var digest []byte
	var valid bool
	err := tx.QueryRow(ctx, `SELECT channel_credential_digest,COALESCE(worker_host_id=$3 AND worker_epoch=$4 AND executor_epoch=$5 AND instance_id=$6
  AND (NOT $7 OR (status='running' AND fenced_at IS NULL AND deadline_at>clock_timestamp() AND executor_expires_at>clock_timestamp()
   AND NOT EXISTS(SELECT 1 FROM secret_exposures x JOIN secrets s ON (s.environment_id,s.id)=(x.environment_id,x.secret_id) WHERE x.environment_id=$1 AND x.preparation_id=$2 AND s.status='revoked'))), false)
  FROM computer_preparations WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, ref.PreparationID, host.HostID, host.Epoch, ref.Epoch, ref.InstanceID, live).Scan(&digest, &valid)
	if err != nil {
		return err
	}
	presented := sha256.Sum256(ref.ChannelCredential)
	if !valid || subtle.ConstantTimeCompare(digest, presented[:]) != 1 {
		return ErrDenied
	}
	return nil
}
func RenewPreparation(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, ref PreparationExecutor) (time.Time, error) {
	var expires time.Time
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockPreparationExecutor(ctx, tx, host, ref, true); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `UPDATE computer_preparations SET executor_expires_at=LEAST(deadline_at,GREATEST(executor_expires_at,clock_timestamp()+interval '1 minute'))
   WHERE environment_id=$1 AND id=$2 RETURNING executor_expires_at`, ref.EnvironmentID, ref.PreparationID).Scan(&expires)
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

func RecordPreparationExposure(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, ref PreparationExecutor) ([]ExposedSecret, error) {
	if !ref.valid() {
		return nil, ErrInvalidInput
	}
	var result []ExposedSecret
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		var spec uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT s.id FROM computer_preparations p JOIN computer_preparation_specs s ON (s.environment_id,s.id)=(p.environment_id,p.preparation_spec_id)
   WHERE p.environment_id=$1 AND p.id=$2`, ref.EnvironmentID, ref.PreparationID).Scan(&spec); err != nil {
			return err
		}
		bindings, err := readPreparationBindings(ctx, tx, ref.EnvironmentID, spec)
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
		if err := lockPreparationSpec(ctx, tx, ref.EnvironmentID, spec); err != nil {
			return err
		}
		var locked uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM computer_preparations WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, ref.EnvironmentID, ref.PreparationID).Scan(&locked); err != nil {
			return err
		}
		if err := checkPreparationExecutor(ctx, tx, host, ref, true); err != nil {
			return err
		}
		var sealed bool
		if err := tx.QueryRow(ctx, `SELECT logical_bytes IS NOT NULL FROM computer_preparations WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, ref.PreparationID).Scan(&sealed); err != nil {
			return err
		}
		if sealed {
			return ErrDenied
		}
		rows, err := tx.Query(ctx, `SELECT secret_id,version_id FROM secret_exposures WHERE environment_id=$1 AND preparation_id=$2`, ref.EnvironmentID, ref.PreparationID)
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
				if _, err = tx.Exec(ctx, `INSERT INTO secret_exposures(environment_id,preparation_id,secret_id,version_id,revocation_generation) SELECT $1,$2,$3,$4,revocation_generation FROM secrets WHERE environment_id=$1 AND id=$3`, ref.EnvironmentID, ref.PreparationID, id, versions[id]); err != nil {
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
		return checkPreparationExecutor(ctx, tx, host, ref, true)
	})
	if err != nil {
		return nil, hideMissing(err)
	}
	slices.SortFunc(result, func(a, b ExposedSecret) int { return strings.Compare(a.Target, b.Target) })
	return result, nil
}

// FailPreparation records an executor-observed failure without releasing its
// writer exclusion. Late reports can reconcile the original terminal outcome.
func FailPreparation(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, ref PreparationExecutor, code string) error {
	if code == "" || len(code) > 256 || !utf8.ValidString(code) {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockPreparationExecutor(ctx, tx, host, ref, false); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE computer_preparations SET proxy_ca_certificate=NULL,proxy_ca_private_key_nonce=NULL,proxy_ca_private_key_ciphertext=NULL,proxy_ca_not_after=NULL,status='failed',error_code=$3
   WHERE environment_id=$1 AND id=$2 AND status='running'`, ref.EnvironmentID, ref.PreparationID, code)
		return err
	}))
}

// PreparationIdentity names physical custody independently of execution authority.
// A Host may have received only this identity before a delivery became ineligible.
type PreparationIdentity struct {
	EnvironmentID uuid.UUID
	PreparationID uuid.UUID
	InstanceID    uuid.UUID
	Epoch         int64
}

func (e PreparationExecutor) Identity() PreparationIdentity {
	return PreparationIdentity{EnvironmentID: e.EnvironmentID, PreparationID: e.PreparationID, InstanceID: e.InstanceID, Epoch: e.Epoch}
}

// ObservePreparationStopped accepts only the owning Host's joined physical VM
// closure. It does not need an execution credential: delivery may never have
// arrived. Provider absence and successor cleanup have separate proof paths.
func ObservePreparationStopped(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, ref PreparationIdentity) error {
	if ref.EnvironmentID == uuid.Nil() || ref.PreparationID == uuid.Nil() || ref.InstanceID == uuid.Nil() || ref.Epoch <= 0 {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		p, err := readPreparation(ctx, tx, ref.EnvironmentID, ref.PreparationID)
		if err != nil {
			return err
		}
		if err := lockPreparationSpec(ctx, tx, ref.EnvironmentID, p.SpecID); err != nil {
			return err
		}
		var owned bool
		if err := tx.QueryRow(ctx, `SELECT COALESCE(worker_host_id=$3 AND worker_epoch=$4 AND executor_epoch=$5 AND instance_id=$6,false)
 FROM computer_preparations WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, ref.EnvironmentID, ref.PreparationID, host.HostID, host.Epoch, ref.Epoch, ref.InstanceID).Scan(&owned); err != nil {
			return err
		}
		if !owned {
			return ErrDenied
		}
		return recordPreparationStopped(ctx, tx, ref.EnvironmentID, ref.PreparationID, "owning worker joined physical VM closure")
	}))
}
func recordPreparationStopped(ctx context.Context, tx pgx.Tx, env, id uuid.UUID, evidence string) error {
	_, err := tx.Exec(ctx, `UPDATE computer_preparations SET proxy_ca_certificate=NULL,proxy_ca_private_key_nonce=NULL,proxy_ca_private_key_ciphertext=NULL,proxy_ca_not_after=NULL,fenced_at=COALESCE(fenced_at,clock_timestamp()),fence_evidence=COALESCE(fence_evidence,$3),
  status=CASE WHEN status IN ('queued','running') THEN 'failed' ELSE status END,
  error_code=CASE WHEN status IN ('queued','running') THEN 'executor_stopped' ELSE error_code END WHERE environment_id=$1 AND id=$2 AND worker_host_id IS NOT NULL`, env, id, evidence)
	return err
}
