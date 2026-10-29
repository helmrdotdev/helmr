package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/mcppolicy"
	"github.com/jackc/pgx/v5"
	"uuid"
)

var (
	ErrRuntimeMCPDenied     = fmt.Errorf("%w: runtime MCP scope denies this operation", ErrDenied)
	ErrRuntimeMCPScopeLimit = errors.New("runtime MCP scope limit reached")
)

type MCPScope struct {
	ID     uuid.UUID        `json:"id"`
	Policy mcppolicy.Policy `json:"policy"`
}

// IssueMCPScope reserves the pinned maximum first, serializing on the existing
// process row. Issuance is control-plane work and does not release business holds.
func IssueMCPScope(ctx context.Context, pool db.TxBeginner, caller Caller, raw json.RawMessage) (MCPScope, error) {
	var result MCPScope
	if caller.Kind != "session" || caller.Host == nil || caller.ID != caller.Execution.SessionID {
		return result, ErrDenied
	}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		authority, err := lockRuntimeAuthority(ctx, tx, *caller.Host, caller.Execution)
		if err != nil {
			return err
		}
		if authority.Generation != caller.Execution.AuthorityGeneration {
			return ErrDenied
		}
		if err = authorizeRuntimeOrigin(ctx, tx, caller.Execution.EnvironmentID, caller.ID); err != nil {
			return err
		}
		var epoch int64
		if err = tx.QueryRow(ctx, `SELECT epoch FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 FOR UPDATE`, caller.Execution.EnvironmentID, caller.ID, caller.Execution.ProcessEpoch).Scan(&epoch); err != nil {
			return hideMissing(err)
		}
		authority, err = lockRuntimeAuthority(ctx, tx, *caller.Host, caller.Execution)
		if err != nil {
			return err
		}
		if authority.Generation != caller.Execution.AuthorityGeneration {
			return ErrDenied
		}
		if err := executionPhysical(ctx, tx, caller.Execution, true, true); err != nil {
			return err
		}
		var eligible bool
		if err := tx.QueryRow(ctx, `SELECT status IN ('open','closing') FROM sessions WHERE environment_id=$1 AND id=$2`, caller.Execution.EnvironmentID, caller.ID).Scan(&eligible); err != nil {
			return hideMissing(err)
		}
		if !eligible {
			return ErrDenied
		}
		maximum, err := runtimeMCPMaximum(ctx, tx, caller)
		if err != nil {
			return err
		}
		requested := maximum
		if len(raw) > 0 {
			keys, err := runtimeMCPAgentKeys(ctx, tx, caller)
			if err != nil {
				return err
			}
			requested, err = mcppolicy.Normalize(raw, keys)
			if err != nil {
				return err
			}
			if !requested.SubsetOf(maximum) {
				return ErrRuntimeMCPDenied
			}
		}
		rows, err := tx.Query(ctx, `SELECT id,slot,policy,policy_digest FROM session_mcp_scopes WHERE environment_id=$1 AND session_id=$2 AND process_epoch=$3 ORDER BY slot`, caller.Execution.EnvironmentID, caller.ID, caller.Execution.ProcessEpoch)
		if err != nil {
			return err
		}
		type stored struct {
			id     uuid.UUID
			slot   int16
			policy []byte
			digest []byte
		}
		scopes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (stored, error) {
			var s stored
			err := row.Scan(&s.id, &s.slot, &s.policy, &s.digest)
			return s, err
		})
		if err != nil {
			return err
		}
		var found uuid.UUID
		for i, s := range scopes {
			p, err := mcppolicy.Decode(s.policy)
			if err != nil {
				return ErrRuntimeMCPDenied
			}
			digest := p.Digest()
			if !bytes.Equal(digest[:], s.digest) || int(s.slot) != i || (i == 0 && !p.Equal(maximum)) || (i > 0 && (!p.SubsetOf(maximum) || p.Equal(maximum))) {
				return ErrRuntimeMCPDenied
			}
			if p.Digest() == requested.Digest() {
				if !p.Equal(requested) {
					return ErrRuntimeMCPDenied
				}
				found = s.id
			}
		}
		insert := func(slot int, p mcppolicy.Policy) (uuid.UUID, error) {
			id := uuid.NewV7()
			digest := p.Digest()
			_, err := tx.Exec(ctx, `INSERT INTO session_mcp_scopes(environment_id,session_id,process_epoch,id,slot,policy,policy_digest) VALUES($1,$2,$3,$4,$5,$6,$7)`, caller.Execution.EnvironmentID, caller.ID, caller.Execution.ProcessEpoch, id, slot, p.Bytes(), digest[:])
			return id, err
		}
		if len(scopes) == 0 {
			id, err := insert(0, maximum)
			if err != nil {
				return err
			}
			if requested.Equal(maximum) {
				found = id
			}
			scopes = append(scopes, stored{})
		}
		if found == uuid.Nil() {
			if len(scopes) >= 16 {
				return ErrRuntimeMCPScopeLimit
			}
			found, err = insert(len(scopes), requested)
			if err != nil {
				return err
			}
		}
		result = MCPScope{ID: found, Policy: requested}
		return nil
	})
	return result, err
}
func runtimeMCPMaximum(ctx context.Context, q db.DBTX, caller Caller) (mcppolicy.Policy, error) {
	var raw []byte
	err := q.QueryRow(ctx, `SELECT d.runtime_mcp_policy FROM sessions s JOIN agent_definitions d ON (d.environment_id,d.agent_id,d.deployment_id)=(s.environment_id,s.agent_id,s.deployment_id) WHERE s.environment_id=$1 AND s.id=$2`, caller.Execution.EnvironmentID, caller.ID).Scan(&raw)
	if err != nil {
		return mcppolicy.Policy{}, hideMissing(err)
	}
	p, err := mcppolicy.Decode(raw)
	if err != nil || p.Kind != "allow" {
		return p, ErrRuntimeMCPDenied
	}
	return p, nil
}
func runtimeMCPAgentKeys(ctx context.Context, q db.DBTX, caller Caller) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT a.key FROM sessions s JOIN agent_definitions d ON d.environment_id=s.environment_id AND d.deployment_id=s.deployment_id JOIN agents a ON (a.environment_id,a.id)=(d.environment_id,d.agent_id) WHERE s.environment_id=$1 AND s.id=$2 ORDER BY a.key`, caller.Execution.EnvironmentID, caller.ID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// RuntimeMCPPolicy must be called within the operation's authority transaction.
func RuntimeMCPPolicy(ctx context.Context, q db.DBTX, caller Caller) (mcppolicy.Policy, error) {
	if caller.Kind != "session" || caller.MCPScopeID == uuid.Nil() {
		return mcppolicy.Policy{}, ErrRuntimeMCPDenied
	}
	var raw, digest []byte
	var slot int16
	err := q.QueryRow(ctx, `SELECT policy,policy_digest,slot FROM session_mcp_scopes WHERE environment_id=$1 AND id=$2 AND session_id=$3 AND process_epoch=$4`, caller.Execution.EnvironmentID, caller.MCPScopeID, caller.ID, caller.Execution.ProcessEpoch).Scan(&raw, &digest, &slot)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return mcppolicy.Policy{}, ErrRuntimeMCPDenied
		}
		return mcppolicy.Policy{}, err
	}
	p, err := mcppolicy.Decode(raw)
	if err != nil {
		return p, ErrRuntimeMCPDenied
	}
	hash := p.Digest()
	if !bytes.Equal(hash[:], digest) {
		return p, ErrRuntimeMCPDenied
	}
	maximum, err := runtimeMCPMaximum(ctx, q, caller)
	if err != nil {
		return p, err
	}
	if !p.SubsetOf(maximum) || (slot == 0 && !p.Equal(maximum)) || (slot != 0 && p.Equal(maximum)) {
		return p, ErrRuntimeMCPDenied
	}

	return p, nil
}
func requireMCPOperation(ctx context.Context, q db.DBTX, caller Caller, operation string) error {
	p, err := RuntimeMCPPolicy(ctx, q, caller)
	if err != nil {
		return err
	}
	if !p.Has(operation) {
		return ErrRuntimeMCPDenied
	}
	return nil
}
func requireMCPTarget(ctx context.Context, q db.DBTX, caller Caller, operation string, target uuid.UUID) error {
	p, err := RuntimeMCPPolicy(ctx, q, caller)
	if err != nil {
		return err
	}
	var key string
	if err = q.QueryRow(ctx, `SELECT a.key FROM sessions s JOIN agents a ON (a.environment_id,a.id)=(s.environment_id,s.agent_id) WHERE s.environment_id=$1 AND s.id=$2`, caller.Execution.EnvironmentID, target).Scan(&key); err != nil {
		return hideMissing(err)
	}
	if !p.Allows(operation, key, "") {
		return ErrRuntimeMCPDenied
	}
	return nil
}
func requireMCPCreation(ctx context.Context, q db.DBTX, caller Caller, operation string, req StartRequest) error {
	p, err := RuntimeMCPPolicy(ctx, q, caller)
	if err != nil {
		return err
	}
	placement := "new"
	if req.ComputerID != uuid.Nil() {
		var own uuid.UUID
		if err = q.QueryRow(ctx, `SELECT computer_id FROM sessions WHERE environment_id=$1 AND id=$2`, req.EnvironmentID, caller.ID).Scan(&own); err != nil {
			return err
		}
		if own == req.ComputerID {
			placement = "caller"
		} else {
			placement = "other"
		}
	}
	if !p.Allows(operation, req.Agent, placement) {
		return ErrRuntimeMCPDenied
	}
	return nil
}
