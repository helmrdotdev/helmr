package agent

import (
	"context"
	"encoding/json"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// SessionView reports retained work independently of current execution eligibility.
// A revoked Computer does not revoke a human's authority to inspect its Sessions.
type SessionView struct {
	ID                 uuid.UUID
	AgentID            uuid.UUID
	DeploymentID       uuid.UUID
	ComputerID         uuid.UUID
	RootSessionID      uuid.UUID
	ParentSessionID    *uuid.UUID
	RequesterSessionID *uuid.UUID
	InitialTurnID      *uuid.UUID
	InitialTurnStatus  *string
	SlackChannelID     *string
	Key                *string
	Status             string
	CreatedAt          time.Time
	Holds              []HoldView
}

type HoldView struct {
	ID        uuid.UUID
	SessionID uuid.UUID
	Scope     string
	Reason    string
	CreatedAt time.Time
}

type TurnView struct {
	PayloadExpiredAt *time.Time
	ID               uuid.UUID
	SessionID        uuid.UUID
	Sequence         int64
	Status           string
	Input            json.RawMessage
	Result           json.RawMessage
	Response         json.RawMessage
	Error            *TurnError
	StartedAt        *time.Time
	TerminalAt       *time.Time
	CompletionSaveID *uuid.UUID
}

type SessionListRequest struct {
	EnvironmentID      uuid.UUID
	Before             uuid.UUID
	AgentID            uuid.UUID
	ParentSessionID    uuid.UUID
	RequesterSessionID uuid.UUID
	Key                *string
	Statuses           []string
	Limit              int
}

type SessionPage struct {
	Sessions   []SessionView
	NextCursor uuid.UUID
}

type TurnListRequest struct {
	EnvironmentID uuid.UUID
	SessionID     uuid.UUID
	After         int64
	Limit         int
}

type TurnPage struct {
	Turns        []TurnView
	NextSequence int64
}

func authorizeRead(ctx context.Context, tx pgx.Tx, caller Caller, env uuid.UUID) error {
	var allowed bool
	var err error
	switch caller.Kind {
	case "user":
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM environments e
   JOIN org_members m ON m.org_id=e.org_id JOIN users u ON u.id=m.user_id
   WHERE e.id=$1 AND m.user_id=$2 AND m.disabled_at IS NULL AND u.disabled_at IS NULL
   AND m.role IN ('owner','admin','developer','viewer'))`, env, caller.ID).Scan(&allowed)
	case "api_key":
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM api_keys k
   JOIN environments e ON (e.id,e.project_id,e.org_id)=(k.environment_id,k.project_id,k.org_id)
   WHERE e.id=$1 AND k.id=$2 AND k.revoked_at IS NULL
   AND (k.expires_at IS NULL OR k.expires_at>clock_timestamp())
   AND k.role IN ('owner','admin','developer','viewer') AND 'sessions.read'=ANY(k.permissions))`, env, caller.ID).Scan(&allowed)
	default:
		return ErrDenied
	}
	if err != nil {
		return err
	}
	if !allowed {
		return ErrDenied
	}
	return nil
}

func readTransaction(ctx context.Context, pool db.TxBeginner, caller Caller, env uuid.UUID, read func(pgx.Tx) error) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		// Holds and outcomes must describe the same snapshot as the Session row.
		if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY`); err != nil {
			return err
		}
		if err := authorizeRead(ctx, tx, caller, env); err != nil {
			return err
		}
		return read(tx)
	}))
}

const sessionViewColumns = `id,agent_id,deployment_id,computer_id,root_session_id,parent_session_id,session_key,status,created_at,(SELECT c.slack_channel_id FROM slack_channels c WHERE c.id=sessions.slack_channel_id),requester_session_id,
 (SELECT t.id FROM turns t WHERE t.environment_id=sessions.environment_id AND t.session_id=sessions.id AND t.seq=1) AS initial_turn_id,
 (SELECT t.status FROM turns t WHERE t.environment_id=sessions.environment_id AND t.session_id=sessions.id AND t.seq=1) AS initial_turn_status,
 (WITH RECURSIVE ancestors AS (
   SELECT s.id,s.parent_session_id FROM sessions s WHERE s.environment_id=sessions.environment_id AND s.id=sessions.id
   UNION ALL SELECT s.id,s.parent_session_id FROM sessions s JOIN ancestors a ON s.id=a.parent_session_id WHERE s.environment_id=sessions.environment_id
  ) SELECT COALESCE(jsonb_agg(jsonb_build_object('ID',h.id,'SessionID',h.session_id,'Scope',h.scope,'Reason',h.reason,'CreatedAt',h.created_at) ORDER BY h.created_at,h.id),'[]'::jsonb)
  FROM session_holds h JOIN ancestors a ON a.id=h.session_id
  WHERE h.environment_id=sessions.environment_id AND h.released_at IS NULL AND (h.session_id=sessions.id OR h.scope='subtree')) AS effective_holds`

func scanSession(row pgx.Row) (SessionView, error) {
	var v SessionView
	var holds []byte
	err := row.Scan(&v.ID, &v.AgentID, &v.DeploymentID, &v.ComputerID, &v.RootSessionID, &v.ParentSessionID, &v.Key, &v.Status, &v.CreatedAt, &v.SlackChannelID, &v.RequesterSessionID, &v.InitialTurnID, &v.InitialTurnStatus, &holds)
	if err == nil {
		err = json.Unmarshal(holds, &v.Holds)
	}
	return v, err
}

func GetSession(ctx context.Context, pool db.TxBeginner, caller Caller, env, id uuid.UUID) (SessionView, error) {
	var result SessionView
	err := readTransaction(ctx, pool, caller, env, func(tx pgx.Tx) error {
		var err error
		result, err = getSession(ctx, tx, env, id)
		return err
	})
	return result, err
}

func getSession(ctx context.Context, tx pgx.Tx, env, id uuid.UUID) (SessionView, error) {
	result, err := scanSession(tx.QueryRow(ctx, `SELECT `+sessionViewColumns+` FROM sessions WHERE environment_id=$1 AND id=$2`, env, id))
	return result, err
}

func ListSessions(ctx context.Context, pool db.TxBeginner, caller Caller, req SessionListRequest) (SessionPage, error) {
	result := SessionPage{Sessions: []SessionView{}}
	err := readTransaction(ctx, pool, caller, req.EnvironmentID, func(tx pgx.Tx) error {
		var err error
		result, err = listSessions(ctx, tx, req)
		return err
	})
	return result, err
}

func listSessions(ctx context.Context, tx pgx.Tx, req SessionListRequest) (SessionPage, error) {
	result := SessionPage{Sessions: []SessionView{}}
	if req.Limit < 1 || req.Limit > 100 {
		return result, ErrInvalidInput
	}
	for _, status := range req.Statuses {
		if status != "open" && status != "closing" && status != "closed" && status != "cancelled" {
			return result, ErrInvalidInput
		}
	}
	if req.Key != nil && (req.AgentID == uuid.Nil() || *req.Key == "") {
		return result, ErrInvalidInput
	}
	rows, err := tx.Query(ctx, `SELECT `+sessionViewColumns+` FROM sessions WHERE environment_id=$1 AND ($2::uuid IS NULL OR id<$2)
 AND ($4::uuid IS NULL OR agent_id=$4) AND ($5::text IS NULL OR session_key=$5)
 AND (cardinality($6::text[]) IS NULL OR cardinality($6::text[])=0 OR status=ANY($6))
 AND ($7::uuid IS NULL OR parent_session_id=$7) AND ($8::uuid IS NULL OR requester_session_id=$8)
 ORDER BY id DESC LIMIT $3`, req.EnvironmentID, nullableID(req.Before), req.Limit+1, nullableID(req.AgentID), req.Key, req.Statuses, nullableID(req.ParentSessionID), nullableID(req.RequesterSessionID))
	if err != nil {
		return result, err
	}
	for rows.Next() {
		v, err := scanSession(rows)
		if err != nil {
			rows.Close()
			return result, err
		}
		result.Sessions = append(result.Sessions, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, err
	}
	if len(result.Sessions) > req.Limit {
		result.Sessions = result.Sessions[:req.Limit]
		result.NextCursor = result.Sessions[len(result.Sessions)-1].ID
	}
	return result, nil
}

// A prepared result is not a successful outcome. Expose it only after the own-save
// transaction has committed completion; finalizing remains explicitly unfinished.
const turnViewColumns = `id,session_id,seq,status,input,CASE WHEN status='completed' THEN result END,CASE WHEN status='completed' THEN response END,error_code,error_message,started_at,terminal_at,completion_save_id,payload_expired_at`

func scanTurn(row pgx.Row) (TurnView, error) {
	var v TurnView
	var code *string
	var message []byte
	err := row.Scan(&v.ID, &v.SessionID, &v.Sequence, &v.Status, &v.Input, &v.Result, &v.Response, &code, &message, &v.StartedAt, &v.TerminalAt, &v.CompletionSaveID, &v.PayloadExpiredAt)
	if code != nil {
		v.Error = &TurnError{Code: *code, Message: string(message)}
	}
	return v, err
}

func GetTurn(ctx context.Context, pool db.TxBeginner, caller Caller, env, session, id uuid.UUID) (TurnView, error) {
	var result TurnView
	err := readTransaction(ctx, pool, caller, env, func(tx pgx.Tx) error {
		var err error
		result, err = scanTurn(tx.QueryRow(ctx, `SELECT `+turnViewColumns+` FROM turns WHERE environment_id=$1 AND session_id=$2 AND id=$3`, env, session, id))
		return err
	})
	return result, err
}

func ListTurns(ctx context.Context, pool db.TxBeginner, caller Caller, req TurnListRequest) (TurnPage, error) {
	result := TurnPage{Turns: []TurnView{}}
	if req.Limit < 1 || req.Limit > 100 || req.After < 0 {
		return result, ErrInvalidInput
	}
	err := readTransaction(ctx, pool, caller, req.EnvironmentID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE environment_id=$1 AND id=$2)`, req.EnvironmentID, req.SessionID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrDenied
		}
		rows, err := tx.Query(ctx, `SELECT `+turnViewColumns+` FROM turns WHERE environment_id=$1 AND session_id=$2 AND seq>$3 ORDER BY seq LIMIT $4`, req.EnvironmentID, req.SessionID, req.After, req.Limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scanTurn(rows)
			if err != nil {
				return err
			}
			result.Turns = append(result.Turns, v)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(result.Turns) > req.Limit {
			result.Turns = result.Turns[:req.Limit]
			result.NextSequence = result.Turns[len(result.Turns)-1].Sequence
		}
		return nil
	})
	return result, err
}
