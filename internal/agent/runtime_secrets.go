package agent

import (
	"context"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

type SessionStartExposure struct {
	Start        ProcessStart
	Secrets      []ExposedSecret
	ProtectedEnv map[string]string
	ProxyCA      []byte
}

type runtimeSecretBinding struct {
	secret                          uuid.UUID
	kind, target, mode, placeholder string
	origins                         []string
}

func readRuntimeBindings(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID) ([]runtimeSecretBinding, error) {
	rows, err := tx.Query(ctx, `SELECT secret_id,placement_kind,placement_target,mode,allowed_origins,COALESCE(placeholder,'') FROM computer_secret_bindings WHERE environment_id=$1 AND computer_id=$2 ORDER BY placement_kind,placement_target`, env, computer)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (runtimeSecretBinding, error) {
		var b runtimeSecretBinding
		err := row.Scan(&b.secret, &b.kind, &b.target, &b.mode, &b.origins, &b.placeholder)
		return b, err
	})
}

// RecordSessionStartExposure commits possible exposure before any ciphertext may
// leave this transaction. A retry retains the logical generation's original pins.
// Healthy restore never calls this startup operation for an existing process.
func RecordSessionStartExposure(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, e Execution, attachment int64) (SessionStartExposure, error) {
	if attachment <= 0 {
		return SessionStartExposure{}, ErrInvalidInput
	}
	var result SessionStartExposure
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		var computer uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT computer_id FROM sessions WHERE environment_id=$1 AND id=$2`, e.EnvironmentID, e.SessionID).Scan(&computer); err != nil {
			return err
		}
		bindings, err := readRuntimeBindings(ctx, tx, e.EnvironmentID, computer)
		if err != nil {
			return err
		}
		ids := []uuid.UUID{}
		for _, b := range bindings {
			ids = append(ids, b.secret)
		}
		// Match the stable Secret lock order used by revocation and preparation before
		// acquiring Computer/Session owners. Duplicate placements share one version.
		rows, err := tx.Query(ctx, `SELECT id FROM secrets WHERE environment_id=$1 AND id=ANY($2::uuid[]) AND status='active' ORDER BY id FOR SHARE`, e.EnvironmentID, ids)
		if err != nil {
			return err
		}
		active, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (uuid.UUID, error) {
			var id uuid.UUID
			err := row.Scan(&id)
			return id, err
		})
		if err != nil {
			return err
		}
		available := map[uuid.UUID]bool{}
		for _, id := range active {
			available[id] = true
		}
		for _, b := range bindings {
			if !available[b.secret] {
				return ErrDenied
			}
		}
		if err = checkSessionStart(ctx, tx, host, e, attachment, &result.Start); err != nil {
			return err
		}
		if result.Start.ComputerID != computer {
			return ErrDenied
		}
		if _, err = tx.Exec(ctx, `INSERT INTO secret_exposures(environment_id,session_id,process_epoch,secret_id,version_id,revocation_generation)
   SELECT $1,$2,$3,s.id,s.current_version_id,s.revocation_generation FROM secrets s WHERE s.environment_id=$1 AND s.id=ANY($4::uuid[]) AND s.status='active'
   ON CONFLICT(environment_id,session_id,process_epoch,secret_id) WHERE session_id IS NOT NULL DO NOTHING`, e.EnvironmentID, e.SessionID, e.ProcessEpoch, ids); err != nil {
			return err
		}
		result.Secrets = make([]ExposedSecret, 0, len(bindings))
		result.ProtectedEnv = map[string]string{}
		for _, b := range bindings {
			value := ExposedSecret{Target: b.target, Kind: b.kind, Mode: b.mode, AllowedOrigins: b.origins, SecretID: b.secret}
			if err = tx.QueryRow(ctx, `SELECT x.version_id,v.version,v.nonce,v.ciphertext FROM secret_exposures x JOIN secret_versions v ON v.secret_id=x.secret_id AND v.id=x.version_id WHERE x.environment_id=$1 AND x.session_id=$2 AND x.process_epoch=$3 AND x.secret_id=$4`, e.EnvironmentID, e.SessionID, e.ProcessEpoch, b.secret).Scan(&value.VersionID, &value.Version, &value.Nonce, &value.Ciphertext); err != nil {
				return err
			}
			result.Secrets = append(result.Secrets, value)
			if b.mode == "protected" {
				result.ProtectedEnv[b.target] = secret.RuntimeSelector(e.EnvironmentID, "process", e.SessionID, e.ProcessEpoch, b.placeholder)
			}
		}
		if len(result.ProtectedEnv) > 0 {
			var expiry time.Time
			if err = tx.QueryRow(ctx, `SELECT proxy_ca_certificate,proxy_ca_not_after FROM computers WHERE environment_id=$1 AND id=$2`, e.EnvironmentID, computer).Scan(&result.ProxyCA, &expiry); err != nil {
				return err
			}
			if err = secret.ValidateProxyTrust(result.ProxyCA, expiry, time.Now()); err != nil {
				return err
			}
		}
		// Locks and crypto validation can take time; release only while the exact
		// process, transport, controls and physical lease still authorize setup.
		return checkSessionStart(ctx, tx, host, e, attachment, &result.Start)
	})
	if err != nil {
		return SessionStartExposure{}, allocationError(err)
	}
	return result, nil
}
