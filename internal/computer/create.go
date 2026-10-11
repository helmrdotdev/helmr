package computer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type CAIssuer interface {
	GenerateProxyTrust(environmentID, computerID uuid.UUID, createdAt time.Time) (secret.ProxyTrust, error)
}
type Creator struct{ ca CAIssuer }

func NewCreator(ca CAIssuer) Creator { return Creator{ca: ca} }

type Request struct {
	Scope          Scope
	DefinitionKey  string
	Key            *string
	Secrets        []secretbinding.Reference
	IdempotencyKey string
}
type Created struct {
	ComputerID uuid.UUID
	Snapshot   Snapshot
	Replayed   bool
}
type createReceipt struct {
	Computer Snapshot `json:"computer"`
}

// Create pins the current Deployment once, alongside the platform key and
// actual Computer bindings. A replay reads its retained receipt, never the
// Environment's later current Deployment or current Secret versions.
func (c Creator) Create(ctx context.Context, txb db.TxBeginner, request Request) (Created, error) {
	if !definition.ValidDeclaredID(request.DefinitionKey) {
		return Created{}, invalidInput("invalid Computer definition key")
	}
	if err := ValidateKey(request.Key); err != nil {
		return Created{}, err
	}
	if request.Secrets == nil {
		request.Secrets = []secretbinding.Reference{}
	}
	refs, err := secretbinding.CanonicalReferences(request.Secrets)
	if err != nil {
		return Created{}, invalidInput("invalid Computer Secret bindings: %v", err)
	}
	request.Secrets = refs
	raw, err := json.Marshal(refs)
	if err != nil {
		return Created{}, err
	}
	var retry idempotency.Request
	if request.IdempotencyKey != "" {
		retry, err = idempotency.NewExternalComputerCreateRequest(request.Scope.EnvironmentID, request.DefinitionKey, request.IdempotencyKey, idempotency.ComputerCreateFingerprint{Key: request.Key, Secrets: raw})
		if err != nil {
			return Created{}, invalidInput("%v", err)
		}
	}
	var result Created
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		var deployment *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT current_deployment_id FROM environments WHERE id=$1 AND org_id=$2 AND project_id=$3 AND retired_at IS NULL FOR NO KEY UPDATE`, request.Scope.EnvironmentID, request.Scope.OrgID, request.Scope.ProjectID).Scan(&deployment); err != nil {
			return err
		}
		claims, err := idempotency.TransactionFor(tx)
		if err != nil {
			return err
		}
		var accepted *db.PlatformRetryKey
		if retry != nil {
			acquired, err := claims.Acquire(ctx, retry)
			if err != nil {
				return err
			}
			if !acquired.New {
				result, err = createdFromReceipt(acquired.Claim.Receipt)
				if err != nil {
					return err
				}
				if !acquired.Claim.ComputerID.Valid || uuid.UUID(acquired.Claim.ComputerID.Bytes) != result.ComputerID {
					return ErrReceiptInvalid
				}
				result.Replayed = true
				return nil
			}
			accepted = &acquired.Claim
		}
		if deployment == nil {
			return ErrNotDeployed
		}
		var live bool
		if err = tx.QueryRow(ctx, `SELECT execution_revoked_at IS NULL FROM deployments WHERE environment_id=$1 AND id=$2 FOR SHARE`, request.Scope.EnvironmentID, *deployment).Scan(&live); err != nil {
			return err
		}
		if !live {
			return ErrNotDeployed
		}
		ids := []uuid.UUID{}
		for _, ref := range refs {
			ids = append(ids, uuid.MustParse(ref.SecretID))
		}
		rows, err := tx.Query(ctx, `SELECT id FROM secrets WHERE environment_id=$1 AND id=ANY($2::uuid[]) AND status='active' ORDER BY id FOR SHARE`, request.Scope.EnvironmentID, ids)
		if err != nil {
			return err
		}
		locked, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (uuid.UUID, error) {
			var id uuid.UUID
			err := row.Scan(&id)
			return id, err
		})
		if err != nil {
			return err
		}
		available := map[uuid.UUID]bool{}
		for _, id := range locked {
			available[id] = true
		}
		for _, id := range ids {
			if !available[id] {
				return ErrSecretUnavailable
			}
		}
		id, err := agent.CreateStandaloneComputer(ctx, tx, c.ca, request.Scope.EnvironmentID, *deployment, request.DefinitionKey, refs)
		if err != nil {
			return err
		}
		if request.Key != nil {
			if _, err = tx.Exec(ctx, `UPDATE computers SET key=$3 WHERE environment_id=$1 AND id=$2`, request.Scope.EnvironmentID, id, *request.Key); err != nil {
				return err
			}
		}
		snapshot, err := Read(ctx, tx, request.Scope, id)
		if err != nil {
			return err
		}
		result = Created{ComputerID: id, Snapshot: snapshot}
		if accepted != nil {
			body, err := json.Marshal(createReceipt{Computer: snapshot})
			if err != nil {
				return err
			}
			if _, err = claims.Complete(ctx, *accepted, idempotency.Target{ComputerID: id}, body); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Created{}, ErrNotDeployed
	}
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23505" && pgerr.ConstraintName == "computers_key" && request.Key != nil {
		return Created{}, KeyConflictError{Key: *request.Key}
	}
	if err != nil {
		return Created{}, fmt.Errorf("create Computer: %w", err)
	}
	return result, nil
}
func createdFromReceipt(raw []byte) (Created, error) {
	var r createReceipt
	if json.Unmarshal(raw, &r) != nil {
		return Created{}, ErrReceiptInvalid
	}
	id, err := ids.Parse(r.Computer.ID)
	if err != nil || ids.Validate(r.Computer.DeploymentID) != nil || !definition.ValidDeclaredID(r.Computer.DefinitionKey) || r.Computer.Status != StatusAvailable || r.Computer.CreatedAt.IsZero() || r.Computer.UpdatedAt.IsZero() || ValidateKey(r.Computer.Key) != nil {
		return Created{}, ErrReceiptInvalid
	}
	if _, err = secretbinding.NormalizeReferences(r.Computer.Secrets); err != nil {
		return Created{}, ErrReceiptInvalid
	}
	return Created{ComputerID: id, Snapshot: r.Computer}, nil
}
