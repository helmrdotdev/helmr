package agent

import (
	"context"
	"encoding/json"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// The instance is immutable and belongs to exactly one physical allocation.
// Routing is not authorization: both captures below establish fresh authority.
func IsPreparationProxyInstance(ctx context.Context, database db.DBTX, instance uuid.UUID) (bool, error) {
	var preparation, computer bool
	if err := database.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_preparations WHERE instance_id=$1),EXISTS(SELECT 1 FROM computer_leases WHERE computer_instance_id=$1)`, instance).Scan(&preparation, &computer); err != nil {
		return false, err
	}
	if preparation && computer {
		return false, ErrDenied
	}
	return preparation, nil
}

// Each capture runs as a fresh primary statement. A preceding lookup, a channel
// credential, or a previously signed leaf never authorizes a later resolution.
const preparationProxyAuthority = `WITH authority AS (
 SELECT p.environment_id,p.id,p.preparation_spec_id,p.proxy_ca_certificate AS certificate,p.proxy_ca_not_after AS not_after,
 p.proxy_ca_private_key_nonce AS nonce,p.proxy_ca_private_key_ciphertext AS ciphertext,
 statement_timestamp() AS authorized_at,
 COALESCE(h.claim_version=$5 AND g.claim_version=$6,false) AS claims_current
 FROM computer_preparations p
 JOIN environments e ON e.id=p.environment_id AND e.retired_at IS NULL
 JOIN worker_hosts h ON h.id=p.worker_host_id AND h.current_epoch=p.worker_epoch
 JOIN worker_groups g ON g.id=h.worker_group_id
 WHERE p.instance_id=$1 AND p.worker_host_id=$2 AND p.worker_epoch=$3 AND h.worker_group_id=$4
 AND h.status IN ('active','draining') AND g.status IN ('active','paused','draining')
 AND h.observed_at>=statement_timestamp()-interval '120 seconds'
 AND p.status='running' AND p.fenced_at IS NULL AND p.logical_bytes IS NULL
 AND p.deadline_at>statement_timestamp() AND p.executor_expires_at>statement_timestamp()
 AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.computer_instance_id=p.instance_id)
 AND NOT EXISTS(SELECT 1 FROM computer_secret_bindings b JOIN secrets s ON (s.environment_id,s.id)=(b.environment_id,b.secret_id)
   WHERE b.environment_id=p.environment_id AND b.preparation_spec_id=p.preparation_spec_id AND s.status<>'active')
) `

type PreparationProxyTrust struct {
	Trust   secret.ProxyTrust
	Origins []string
	Env     map[string]string
}

func CapturePreparationProxyTrust(ctx context.Context, database db.DBTX, host workergroup.HostPrincipal, instance uuid.UUID) (PreparationProxyTrust, error) {
	var result PreparationProxyTrust
	var expiry *time.Time
	var current bool
	var env []byte
	err := database.QueryRow(ctx, preparationProxyAuthority+`SELECT a.environment_id,a.id,a.certificate,a.not_after,a.nonce,a.ciphertext,a.claims_current,
 ARRAY(SELECT DISTINCT unnest(b.allowed_origins) FROM computer_secret_bindings b WHERE b.environment_id=a.environment_id AND b.preparation_spec_id=a.preparation_spec_id AND b.mode='protected'),
 COALESCE((SELECT jsonb_object_agg(b.placement_target,b.placeholder) FROM computer_secret_bindings b WHERE b.environment_id=a.environment_id AND b.preparation_spec_id=a.preparation_spec_id AND b.mode='protected'),'{}'::jsonb)
 FROM authority a`, instance, host.HostID, host.Epoch, host.GroupID, host.HostClaimVersion, host.GroupClaimVersion).Scan(&result.Trust.EnvironmentID, &result.Trust.PreparationID, &result.Trust.Certificate, &expiry, &result.Trust.PrivateKeyNonce, &result.Trust.PrivateKeyCiphertext, &current, &result.Origins, &env)
	if err != nil {
		return PreparationProxyTrust{}, hideMissing(err)
	}
	if !current {
		return PreparationProxyTrust{}, workergroup.ErrStaleClaims
	}
	if expiry != nil {
		result.Trust.NotAfter = *expiry
	}
	if err := json.Unmarshal(env, &result.Env); err != nil {
		return PreparationProxyTrust{}, err
	}
	if len(result.Origins) > 0 {
		if err := secret.ValidateProxyTrust(result.Trust.Certificate, result.Trust.NotAfter, time.Now()); err != nil {
			return PreparationProxyTrust{}, err
		}
	}
	return result, nil
}

// CapturePreparationProtectedSecrets selects only already exposed versions.
// Origin, current revocation, complete selector coverage and expiry remain checked
// for every request, even when the host reuses a previously issued TLS leaf.
func CapturePreparationProtectedSecrets(ctx context.Context, database db.DBTX, host workergroup.HostPrincipal, instance uuid.UUID, origin string, selectors []string) ([]secret.ProtectedCapture, error) {
	canonical, err := secretbinding.CanonicalOrigin(origin)
	if err != nil || canonical != origin {
		return nil, ErrInvalidInput
	}
	if err := secret.ValidateProtectedSelectors(selectors); err != nil {
		return nil, ErrInvalidInput
	}
	rows, err := database.Query(ctx, preparationProxyAuthority+`SELECT b.placeholder,a.environment_id,b.secret_id,x.version_id,v.version,v.nonce,v.ciphertext,a.certificate,a.not_after,a.authorized_at,a.claims_current
 FROM authority a JOIN computer_secret_bindings b ON b.environment_id=a.environment_id AND b.preparation_spec_id=a.preparation_spec_id
 JOIN secret_exposures x ON x.environment_id=a.environment_id AND x.preparation_id=a.id AND x.secret_id=b.secret_id
 JOIN secret_versions v ON v.secret_id=x.secret_id AND v.id=x.version_id
 WHERE b.mode='protected' AND b.placeholder=ANY($7::text[]) AND $8=ANY(b.allowed_origins)
 ORDER BY b.placeholder`, instance, host.HostID, host.Epoch, host.GroupID, host.HostClaimVersion, host.GroupClaimVersion, selectors, origin)
	if err != nil {
		return nil, err
	}
	result, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (secret.ProtectedCapture, error) {
		var result secret.ProtectedCapture
		err := row.Scan(&result.Placeholder, &result.EnvironmentID, &result.SecretID, &result.VersionID, &result.Version, &result.Nonce, &result.Ciphertext, &result.Certificate, &result.NotAfter, &result.AuthorizedAt, &result.ClaimsCurrent)
		return result, err
	})
	if err != nil {
		return nil, err
	}
	for _, r := range result {
		if !r.ClaimsCurrent {
			return nil, workergroup.ErrStaleClaims
		}
	}
	if len(result) != len(selectors) {
		return nil, ErrDenied
	}
	return result, nil
}
