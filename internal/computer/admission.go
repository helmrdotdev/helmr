package computer

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/lifetime"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/jackc/pgx/v5"
)

type ComputerTrustIssuer interface {
	GenerateProxyTrust(environmentID, computerID uuid.UUID, createdAt time.Time) (secret.ProxyTrust, error)
}

// Limits are the Environment bounds a new Computer must fit, read under the
// caller's Environment lock (step 4 of the global order).
type Limits struct {
	MaxCPU, MaxMemory, MaxStorage, PreparationTimeout int64
}

// CreateForAdmission creates the Computer of an admitted Session from the
// Agent's template, copying the definition's Secret bindings.
func CreateForAdmission(ctx context.Context, tx pgx.Tx, trust ComputerTrustIssuer, env, deployment uuid.UUID, key string, limits Limits) (uuid.UUID, error) {
	return createComputer(ctx, tx, trust, env, deployment, key, limits, nil)
}

// CreateStandaloneComputer joins the external owner's transaction. Explicit
// runtime bindings are actual-Computer declarations; Agent-created Computers
// instead copy their immutable definition bindings. Both share preparation,
// trust, resource validation and reservation admission.
func CreateStandaloneComputer(ctx context.Context, tx pgx.Tx, trust ComputerTrustIssuer, env, deployment uuid.UUID, key string, bindings []secretbinding.Reference) (uuid.UUID, error) {
	var limits Limits
	if err := tx.QueryRow(ctx, `SELECT max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms
 FROM environments WHERE id=$1 AND retired_at IS NULL FOR NO KEY UPDATE`, env).Scan(&limits.MaxCPU, &limits.MaxMemory, &limits.MaxStorage, &limits.PreparationTimeout); err != nil {
		return uuid.Nil(), err
	}
	if bindings == nil {
		bindings = []secretbinding.Reference{}
	}
	return createComputer(ctx, tx, trust, env, deployment, key, limits, &bindings)
}

func createComputer(ctx context.Context, tx pgx.Tx, trust ComputerTrustIssuer, env, deployment uuid.UUID, key string, limits Limits, explicit *[]secretbinding.Reference) (uuid.UUID, error) {
	var spec, template uuid.UUID
	var cpuMillis, memoryBytes int64
	var diskBytes *int64
	var seed []byte
	var maxAge, deleteUnused *int64
	var deleteNever bool
	if err := tx.QueryRow(ctx, `SELECT d.template_id,d.image_spec_id,d.cpu_millis,d.memory_bytes,d.disk_bytes,d.max_image_age_ms,s.seed,d.delete_after_unused_ms,d.delete_after_unused_never FROM template_definitions d JOIN templates t ON (t.environment_id,t.id)=(d.environment_id,d.template_id) JOIN image_specs s ON (s.environment_id,s.id)=(d.environment_id,d.image_spec_id) WHERE d.environment_id=$1 AND d.deployment_id=$2 AND t.key=$3`, env, deployment, key).Scan(&template, &spec, &cpuMillis, &memoryBytes, &diskBytes, &maxAge, &seed, &deleteUnused, &deleteNever); err != nil {
		return uuid.Nil(), err
	}
	var base definition.ComputerSeedManifest
	if json.Unmarshal(seed, &base) != nil || base.Profile != definition.ComputerSeedProfile {
		return uuid.Nil(), ErrInvalidInput
	}
	physicalCPU, err := vm.ReservedCPUMillis(cpuMillis)
	if err != nil || (diskBytes != nil && *diskBytes != disk.SeedCapacity) || physicalCPU > limits.MaxCPU || memoryBytes > limits.MaxMemory || disk.SeedCapacity > limits.MaxStorage {
		return uuid.Nil(), ErrInvalidInput
	}
	var reserved int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(storage_reservation_bytes),0) FROM computers WHERE environment_id=$1`, env).Scan(&reserved); err != nil {
		return uuid.Nil(), err
	}
	if reserved > limits.MaxStorage-disk.SeedCapacity {
		return uuid.Nil(), ErrNotReady
	}
	id := uuid.NewV7()
	if _, err := tx.Exec(ctx, `INSERT INTO computers(environment_id,id,image_spec_id,origin_deployment_id,origin_template_id,cpu_millis,memory_bytes,disk_bytes,storage_reservation_bytes,preparation_deadline_at,preparation_max_age_ms,delete_after_unused_ms) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,clock_timestamp()+$10*interval '1 millisecond',$11,$12)`, env, id, spec, deployment, template, cpuMillis, memoryBytes, diskBytes, disk.SeedCapacity, limits.PreparationTimeout, maxAge, lifetime.Resolve(lifetime.Policy{}, deleteUnused, deleteNever)); err != nil {
		return uuid.Nil(), err
	}
	if explicit == nil {
		if err := copyDefinitionSecretBindings(ctx, tx, env, deployment, template, id); err != nil {
			return uuid.Nil(), err
		}
	} else {
		resolved, err := ResolveSecretReferences(ctx, tx, env, *explicit)
		if err != nil {
			return uuid.Nil(), err
		}
		bindings, err := RegistrationSecretBindings(*explicit, resolved, nil, nil, nil)
		if err != nil {
			return uuid.Nil(), err
		}
		for i := range bindings {
			bindings[i].Computer = &id
		}
		if err = RegisterSecretBindings(ctx, tx, env, bindings); err != nil {
			return uuid.Nil(), err
		}
	}

	var protected bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secret_bindings WHERE environment_id=$1 AND computer_id=$2 AND mode='protected')`, env, id).Scan(&protected); err != nil {
		return uuid.Nil(), err
	}
	if protected {
		if trust == nil {
			return uuid.Nil(), ErrNotReady
		}
		var createdAt time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&createdAt); err != nil {
			return uuid.Nil(), err
		}
		material, err := trust.GenerateProxyTrust(env, id, createdAt)
		if err != nil {
			return uuid.Nil(), err
		}
		if _, err = tx.Exec(ctx, `UPDATE computers SET proxy_ca_certificate=$3,proxy_ca_not_after=$4,proxy_ca_private_key_nonce=$5,proxy_ca_private_key_ciphertext=$6 WHERE environment_id=$1 AND id=$2`, env, id, material.Certificate, material.NotAfter, material.PrivateKeyNonce, material.PrivateKeyCiphertext); err != nil {
			return uuid.Nil(), err
		}
	}

	if _, err := PinImage(ctx, tx, env, id); err == nil {
		return id, nil
	} else if !errors.Is(err, ErrNotReady) {
		return uuid.Nil(), err
	}
	if _, err := AttachImageBuild(ctx, tx, env, id); err != nil && !errors.Is(err, ErrNotReady) {
		return uuid.Nil(), err
	}
	return id, nil
}
