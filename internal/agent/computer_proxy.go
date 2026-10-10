package agent

import (
	"context"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// Each capture binds the immutable physical instance to the current Computer
// lease and authenticated host, using a fresh primary statement snapshot.
const computerProxyAuthority = `WITH authority AS (
 SELECT c.environment_id,c.id,l.epoch,l.status,c.proxy_ca_certificate AS certificate,c.proxy_ca_not_after AS not_after,
 c.proxy_ca_private_key_nonce AS nonce,c.proxy_ca_private_key_ciphertext AS ciphertext,
 statement_timestamp() AS authorized_at,
 COALESCE(h.claim_version=$5 AND g.claim_version=$6,false) AS claims_current
 FROM computer_leases l
 JOIN computers c ON (c.environment_id,c.id)=(l.environment_id,l.computer_id)
 JOIN environments e ON e.id=c.environment_id AND e.retired_at IS NULL
 JOIN worker_hosts h ON h.id=l.worker_host_id AND h.current_epoch=l.worker_epoch
 JOIN worker_groups g ON g.id=h.worker_group_id
 WHERE l.computer_instance_id=$1 AND l.worker_host_id=$2 AND l.worker_epoch=$3 AND h.worker_group_id=$4
 AND h.status IN ('active','draining') AND g.status IN ('active','paused','draining')
 AND h.observed_at>=statement_timestamp()-interval '120 seconds'
 AND l.status IN ('acquiring','active') AND l.fenced_at IS NULL AND l.expires_at>statement_timestamp()
 AND (l.status='active' OR (h.status IN ('active','draining') AND g.status='active'
   AND h.run_paused_reason IS NULL AND h.vm_paused_reason IS NULL
   AND EXISTS(SELECT 1 FROM worker_pools p WHERE p.id=h.worker_pool_id AND p.status='active')))
 AND c.deleted_at IS NULL AND c.integrity_fault_at IS NULL AND c.preparation_failed_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM computer_preparations p WHERE p.instance_id=l.computer_instance_id)
 AND NOT EXISTS(SELECT 1 FROM computer_secret_revocations r WHERE r.environment_id=c.environment_id AND r.computer_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM computer_secret_bindings b JOIN secrets s ON (s.environment_id,s.id)=(b.environment_id,b.secret_id)
   WHERE b.environment_id=c.environment_id AND b.computer_id=c.id AND s.status<>'active')
) `

type ComputerProxyTrust struct {
	Trust   secret.ProxyTrust
	Origins []string
}

func CaptureComputerProxyTrust(ctx context.Context, database db.DBTX, host workergroup.HostPrincipal, instance uuid.UUID) (ComputerProxyTrust, error) {
	var result ComputerProxyTrust
	var expiry *time.Time
	var current bool
	err := database.QueryRow(ctx, computerProxyAuthority+`SELECT a.environment_id,a.id,a.certificate,a.not_after,a.nonce,a.ciphertext,a.claims_current,
 ARRAY(SELECT DISTINCT unnest(b.allowed_origins) FROM computer_secret_bindings b WHERE b.environment_id=a.environment_id AND b.computer_id=a.id AND b.mode='protected')
 FROM authority a`, instance, host.HostID, host.Epoch, host.GroupID, host.HostClaimVersion, host.GroupClaimVersion).Scan(&result.Trust.EnvironmentID, &result.Trust.ComputerID, &result.Trust.Certificate, &expiry, &result.Trust.PrivateKeyNonce, &result.Trust.PrivateKeyCiphertext, &current, &result.Origins)
	if err != nil {
		return ComputerProxyTrust{}, hideMissing(err)
	}
	if !current {
		return ComputerProxyTrust{}, workergroup.ErrStaleClaims
	}
	if expiry != nil {
		result.Trust.NotAfter = *expiry
	}
	if len(result.Origins) > 0 {
		if err := secret.ValidateProxyTrust(result.Trust.Certificate, result.Trust.NotAfter, time.Now()); err != nil {
			return ComputerProxyTrust{}, err
		}
	}
	return result, nil
}

// CaptureComputerProtectedSecrets resolves a logical owner pin only while that
// owner remains eligible on the requesting physical lease.
// Origin, current revocation, complete selector coverage and expiry remain checked
// for every request, even when the host reuses a previously issued TLS leaf.
func CaptureComputerProtectedSecrets(ctx context.Context, database db.DBTX, host workergroup.HostPrincipal, instance uuid.UUID, origin string, selectors []string) ([]secret.ProtectedCapture, error) {
	canonical, err := secretbinding.CanonicalOrigin(origin)
	if err != nil || canonical != origin {
		return nil, ErrInvalidInput
	}
	if err := secret.ValidateProtectedSelectors(selectors); err != nil {
		return nil, ErrInvalidInput
	}
	rows, err := database.Query(ctx, computerProxyAuthority+`, owners AS (
 SELECT p.environment_id,p.computer_id,p.session_id AS owner_id,p.epoch AS owner_epoch,'process'::text AS kind
 FROM authority a JOIN session_processes p ON p.environment_id=a.environment_id AND p.computer_id=a.id AND p.computer_lease_epoch=a.epoch
 JOIN sessions s ON (s.environment_id,s.id)=(p.environment_id,p.session_id)
 JOIN deployments d ON (d.environment_id,d.id)=(s.environment_id,s.deployment_id)
 WHERE p.status IN ('starting','ready') AND p.fenced_at IS NULL AND p.failure_recorded_at IS NULL AND d.execution_revoked_at IS NULL AND s.status IN ('open','closing')
 UNION ALL
 SELECT c.environment_id,c.computer_id,c.id,0::bigint,'command'::text FROM authority a
 JOIN computer_commands c ON c.environment_id=a.environment_id AND c.computer_id=a.id AND c.computer_lease_epoch=a.epoch
 WHERE c.status IN ('starting','running') AND c.cancel_requested_at IS NULL
), candidates AS (
 SELECT 'hlmr_protected_'||encode(sha256(convert_to('helmr.runtime-secret-selector.v1|'||a.environment_id::text||'|'||o.kind||'|'||o.owner_id::text||'|'||o.owner_epoch::text||'|'||b.placeholder,'UTF8')),'hex') AS placeholder,
 a.environment_id,b.secret_id,x.version_id,v.version,v.nonce,v.ciphertext,a.certificate,a.not_after,a.authorized_at,a.claims_current
 FROM authority a JOIN owners o ON o.environment_id=a.environment_id AND o.computer_id=a.id
 JOIN computer_secret_bindings b ON b.environment_id=a.environment_id AND b.computer_id=a.id
 JOIN secret_exposures x ON x.environment_id=a.environment_id AND x.secret_id=b.secret_id
 AND ((o.kind='process' AND x.session_id=o.owner_id AND x.process_epoch=o.owner_epoch) OR (o.kind='command' AND x.command_id=o.owner_id))
 JOIN secret_versions v ON v.secret_id=x.secret_id AND v.id=x.version_id
 WHERE a.status='active' AND b.mode='protected' AND $8=ANY(b.allowed_origins)
 AND NOT EXISTS(SELECT 1 FROM computer_checkpoints cp WHERE cp.environment_id=a.environment_id AND cp.computer_id=a.id
 AND (cp.status IN ('capturing','sealed','ready','restoring','aborting') OR (cp.status='consumed' AND cp.controls_reconciled_at IS NULL)))
) SELECT * FROM candidates WHERE placeholder=ANY($7::text[]) ORDER BY placeholder`, instance, host.HostID, host.Epoch, host.GroupID, host.HostClaimVersion, host.GroupClaimVersion, selectors, origin)
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
