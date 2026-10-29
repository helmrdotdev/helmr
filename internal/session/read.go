package session

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/jackc/pgx/v5"
)

// SessionView reports retained work independently of current execution eligibility.
// A revoked Computer does not revoke a human's authority to inspect its Sessions.
type SessionView struct {
	RelatedVia           string
	Permissions          []string
	AgentDefinitionID    string
	TemplateDefinitionID string
	Activity             SessionActivityView
	Suspension           string
	CloseAfterIdleMs     *int64
	IdleSince            *time.Time
	ID                   uuid.UUID
	AgentID              uuid.UUID
	DeploymentID         uuid.UUID
	ComputerID           *uuid.UUID
	RootSessionID        uuid.UUID
	ParentSessionID      *uuid.UUID
	RequesterSessionID   *uuid.UUID
	InitialTurnID        *uuid.UUID
	InitialTurnStatus    *string
	SlackChannelID       *string
	Key                  *string
	Status               string
	CreatedAt            time.Time
	Holds                []HoldView
}

type SessionActivityView struct {
	Process *ProcessActivityView `json:"process"`
}
type ProcessActivityView struct {
	Epoch                  int64            `json:"epoch"`
	Status                 string           `json:"status"`
	Applied                bool             `json:"applied"`
	ReconstructionRequired bool             `json:"reconstruction_required"`
	Stop                   *ProcessStopView `json:"stop"`
}
type ProcessStopView struct {
	ProcessEpoch int64     `json:"process_epoch"`
	TurnID       *string   `json:"turn_id"`
	State        string    `json:"state"`
	Deadline     time.Time `json:"deadline"`
}

const processStopFields = `jsonb_build_object('process_epoch',p.epoch,'turn_id',p.stop_turn_id,'state',CASE WHEN p.fenced_at IS NOT NULL THEN 'stopped' WHEN p.status IN ('stopping','lost') THEN 'stopping' ELSE 'requested' END,'deadline',p.stop_deadline_at)`
const sessionActivityColumn = `jsonb_build_object(
 'process',(SELECT jsonb_build_object('epoch',p.epoch,'status',p.status,'applied',p.control_kind='resume' AND p.control_generation=sessions.authority_generation AND p.control_attachment=p.attachment_sequence AND p.control_acknowledged_at IS NOT NULL AND p.control_error IS NULL,
 'reconstruction_required',p.failure_recorded_at IS NOT NULL,'stop',CASE WHEN p.stop_deadline_at IS NOT NULL THEN ` + processStopFields + ` END)
 FROM session_processes p WHERE p.environment_id=sessions.environment_id AND p.session_id=sessions.id ORDER BY p.epoch DESC LIMIT 1))`

type HoldView struct {
	RecoveryGap *RecoveryGap
	Kind        string
	ID          uuid.UUID
	SessionID   uuid.UUID
	Scope       string
	Reason      string
	CreatedAt   time.Time
}

type TurnView struct {
	Stop             *ProcessStopView
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
}

type SessionListRequest struct {
	RelatedUserID      uuid.UUID
	Agent              string
	EnvironmentID      uuid.UUID
	Before             uuid.UUID
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
	_, err := readScopeFor(ctx, tx, caller, env)
	return err
}

func readTransaction(ctx context.Context, pool db.TxBeginner, caller Caller, env uuid.UUID, read func(pgx.Tx) error) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		// Holds and outcomes share the Session snapshot. Authority checks take
		// row locks so source retirement cannot race an authorized read.
		if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`); err != nil {
			return err
		}
		if err := authorizeRead(ctx, tx, caller, env); err != nil {
			return err
		}
		return read(tx)
	}))
}

const sessionViewColumns = `(SELECT a.key FROM agents a WHERE a.environment_id=sessions.environment_id AND a.id=sessions.agent_id),
 (SELECT t.key FROM agent_definitions d JOIN templates t ON (t.environment_id,t.id)=(d.environment_id,d.template_id) WHERE d.environment_id=sessions.environment_id AND d.deployment_id=sessions.deployment_id AND d.agent_id=sessions.agent_id),
 id,agent_id,deployment_id,computer_id,root_session_id,parent_session_id,key,status,created_at,(SELECT c.slack_channel_id FROM slack_threads t JOIN slack_channel_bindings c ON c.id=t.slack_channel_binding_id WHERE t.environment_id=sessions.environment_id AND t.front_session_id=sessions.id),requester_session_id,
 (SELECT t.id FROM turns t WHERE t.environment_id=sessions.environment_id AND t.session_id=sessions.id AND t.seq=1) AS initial_turn_id,
 (SELECT t.status FROM turns t WHERE t.environment_id=sessions.environment_id AND t.session_id=sessions.id AND t.seq=1) AS initial_turn_status,
 (WITH RECURSIVE ancestors AS (
   SELECT s.id,s.parent_session_id FROM sessions s WHERE s.environment_id=sessions.environment_id AND s.id=sessions.id
   UNION ALL SELECT s.id,s.parent_session_id FROM sessions s JOIN ancestors a ON s.id=a.parent_session_id WHERE s.environment_id=sessions.environment_id
  ) SELECT COALESCE(jsonb_agg(jsonb_build_object('ID',h.id,'SessionID',h.session_id,'Scope',h.scope,'Kind',h.kind,'RecoveryGap',h.recovery_gap || jsonb_build_object('holdId',h.id,'computerId',h.recovery_computer_id,'leaseEpoch',h.recovery_lease_epoch),'Reason',h.reason,'CreatedAt',h.created_at) ORDER BY h.created_at,h.id),'[]'::jsonb)
  FROM session_holds h JOIN ancestors a ON a.id=h.session_id
  WHERE h.environment_id=sessions.environment_id AND h.released_at IS NULL AND (h.session_id=sessions.id OR h.scope='subtree')) AS effective_holds,(SELECT d.suspension FROM agent_definitions d WHERE (d.environment_id,d.agent_id,d.deployment_id)=(sessions.environment_id,sessions.agent_id,sessions.deployment_id)),close_after_idle_ms,idle_since,` + sessionActivityColumn

func scanSession(row pgx.Row, extra ...any) (SessionView, error) {
	var v SessionView
	var holds, activity []byte
	dest := []any{&v.AgentDefinitionID, &v.TemplateDefinitionID, &v.ID, &v.AgentID, &v.DeploymentID, &v.ComputerID, &v.RootSessionID, &v.ParentSessionID, &v.Key, &v.Status, &v.CreatedAt, &v.SlackChannelID, &v.RequesterSessionID, &v.InitialTurnID, &v.InitialTurnStatus, &holds, &v.Suspension, &v.CloseAfterIdleMs, &v.IdleSince, &activity}
	err := row.Scan(append(dest, extra...)...)
	if err == nil {
		err = json.Unmarshal(holds, &v.Holds)
	}
	if err == nil {
		err = json.Unmarshal(activity, &v.Activity)
	}
	return v, err
}

func GetSession(ctx context.Context, policy auditpolicy.Policy, pool db.TxBeginner, caller Caller, env, id uuid.UUID) (SessionView, error) {
	var result SessionView
	err := discloseTransaction(ctx, policy, pool, caller, env, id, uuid.Nil(), func(tx pgx.Tx) error {
		var err error
		if err = authorizeSessionRead(ctx, tx, caller, env, id); err != nil {
			return err
		}
		result, err = getSession(ctx, tx, env, id)
		if err != nil {
			return err
		}
		views := []SessionView{result}
		if err := redactSessionRelations(ctx, tx, caller, env, views); err != nil {
			return err
		}
		result = views[0]
		result.Permissions, err = externalSessionPermissions(ctx, tx, caller, env, id)
		return err
	})
	if err != nil {
		return SessionView{}, err
	}
	return result, nil
}

func getSession(ctx context.Context, tx pgx.Tx, env, id uuid.UUID) (SessionView, error) {
	result, err := scanSession(tx.QueryRow(ctx, `SELECT `+sessionViewColumns+` FROM sessions WHERE environment_id=$1 AND id=$2`, env, id))
	return result, err
}

func ListSessions(ctx context.Context, policy auditpolicy.Policy, pool db.TxBeginner, caller Caller, req SessionListRequest) (SessionPage, error) {
	result := SessionPage{Sessions: []SessionView{}}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`); err != nil {
			return err
		}
		var err error
		filter, err := readScopeFor(ctx, tx, caller, req.EnvironmentID)
		if err != nil {
			return err
		}
		result, err = listSessions(ctx, tx, req, filter)
		if err != nil {
			return err
		}
		if err := redactSessionRelations(ctx, tx, caller, req.EnvironmentID, result.Sessions); err != nil {
			return err
		}
		return recordSessionHoldsRead(ctx, policy, tx, caller, req.EnvironmentID, result.Sessions)
	})
	if err != nil {
		return SessionPage{}, hideMissing(err)
	}
	return result, nil
}

const sessionReadPredicate = `($2::boolean OR agent_id=ANY($7::uuid[]) OR (origin_slack_connection_id=$8) OR (origin_slack_connection_id IS NULL AND (origin_user_id=$3 OR origin_api_key_id=$4) AND ($5::boolean OR agent_id=ANY($6::uuid[]))))`

func listSessions(ctx context.Context, tx pgx.Tx, req SessionListRequest, filter sessionReadScope) (SessionPage, error) {
	result := SessionPage{Sessions: []SessionView{}}
	if req.Limit < 1 || req.Limit > 100 {
		return result, ErrInvalidInput
	}
	for _, status := range req.Statuses {
		if status != "open" && status != "closing" && status != "closed" && status != "cancelled" {
			return result, ErrInvalidInput
		}
	}
	if req.Agent != "" && !definition.ValidDeclaredID(req.Agent) {
		return result, ErrInvalidInput
	}
	if req.Key != nil && (req.Agent == "" || *req.Key == "") {
		return result, ErrInvalidInput
	}
	rows, err := tx.Query(ctx, `WITH RECURSIVE seeds(id,priority) AS (
 SELECT id,1 FROM sessions WHERE environment_id=$1 AND origin_user_id=$17 AND parent_session_id IS NULL AND requester_session_id IS NULL
 UNION ALL SELECT session_id,2 FROM turns WHERE environment_id=$1 AND caller_kind='user' AND caller_id=$17
 UNION ALL SELECT session_id,2 FROM turn_messages WHERE environment_id=$1 AND caller_kind='user' AND caller_id=$17
 UNION ALL SELECT session_id,3 FROM turn_asks WHERE environment_id=$1 AND answered_by_user_id=$17
 ), direct AS (
 SELECT id,CASE min(priority) WHEN 1 THEN 'initiated' WHEN 2 THEN 'contributed' ELSE 'answered' END AS relation FROM seeds GROUP BY id
 ), related(id) AS (SELECT id FROM direct UNION SELECT s.id FROM sessions s JOIN related p ON (s.parent_session_id=p.id OR s.requester_session_id=p.id) WHERE s.environment_id=$1)
 SELECT `+sessionViewColumns+`,CASE WHEN $17::uuid IS NULL THEN '' ELSE COALESCE((SELECT relation FROM direct WHERE direct.id=sessions.id),'descendant') END FROM sessions WHERE environment_id=$1 AND ($17::uuid IS NULL OR id IN(SELECT id FROM related)) AND `+sessionReadPredicate+`
 AND ($9::uuid IS NULL OR id<$9) AND ($11::text IS NULL OR key=$11)
 AND (cardinality($12::text[]) IS NULL OR cardinality($12::text[])=0 OR status=ANY($12))
 AND ($13::uuid IS NULL OR parent_session_id=$13) AND ($14::uuid IS NULL OR requester_session_id=$14)
 AND ($15::text = '' OR agent_id=(SELECT a.id FROM agents a WHERE a.environment_id=$1 AND a.key=$15))
 AND ($16::text[] IS NULL OR agent_id IN (SELECT id FROM agents WHERE environment_id=$1 AND key=ANY($16::text[])))
 ORDER BY id DESC LIMIT $10`, req.EnvironmentID, filter.All, filter.User, filter.Key, filter.AllAgents, filter.Agents, filter.ViewAgents, nullableID(filter.SharedConnection), nullableID(req.Before), req.Limit+1, req.Key, req.Statuses, nullableID(req.ParentSessionID), nullableID(req.RequesterSessionID), req.Agent, filter.MCPAgentKeys, nullableID(req.RelatedUserID))
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var relatedVia string
		v, err := scanSession(rows, &relatedVia)
		v.RelatedVia = relatedVia
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

// Result and response become visible atomically with the completion commit.
const turnViewColumns = `id,session_id,seq,status,input,CASE WHEN status='completed' THEN result END,CASE WHEN status='completed' THEN response END,error_code,error_message,started_at,terminal_at,payload_expired_at,(SELECT ` + processStopFields + ` FROM session_processes p WHERE p.environment_id=turns.environment_id AND p.session_id=turns.session_id AND p.stop_deadline_at IS NOT NULL AND (p.stop_turn_id=turns.id OR turns.status='queued') AND NOT EXISTS(SELECT 1 FROM session_processes newer WHERE newer.environment_id=p.environment_id AND newer.session_id=p.session_id AND newer.epoch>p.epoch))`

func scanTurn(row pgx.Row) (TurnView, error) {
	var v TurnView
	var code *string
	var message, stop []byte
	err := row.Scan(&v.ID, &v.SessionID, &v.Sequence, &v.Status, &v.Input, &v.Result, &v.Response, &code, &message, &v.StartedAt, &v.TerminalAt, &v.PayloadExpiredAt, &stop)
	if err == nil && stop != nil {
		err = json.Unmarshal(stop, &v.Stop)
	}
	if code != nil {
		v.Error = &TurnError{Code: *code, Message: string(message)}
	}
	return v, err
}

func GetTurn(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, caller Caller, env, session, id uuid.UUID) (TurnView, error) {
	var result TurnView
	err := discloseTransaction(ctx, auditPolicy, pool, caller, env, session, id, func(tx pgx.Tx) error {
		var err error
		if err = authorizeSessionRead(ctx, tx, caller, env, session); err != nil {
			return err
		}
		result, err = scanTurn(tx.QueryRow(ctx, `SELECT `+turnViewColumns+` FROM turns WHERE environment_id=$1 AND session_id=$2 AND id=$3`, env, session, id))
		return err
	})
	if err != nil {
		return TurnView{}, hideMissing(err)
	}
	return result, err
}

func ListTurns(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, caller Caller, req TurnListRequest) (TurnPage, error) {
	result := TurnPage{Turns: []TurnView{}}
	if req.Limit < 1 || req.Limit > 100 || req.After < 0 {
		return result, ErrInvalidInput
	}
	err := discloseTransaction(ctx, auditPolicy, pool, caller, req.EnvironmentID, req.SessionID, uuid.Nil(), func(tx pgx.Tx) error {
		if err := authorizeSessionRead(ctx, tx, caller, req.EnvironmentID, req.SessionID); err != nil {
			return err
		}
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
	if err != nil {
		return TurnPage{}, hideMissing(err)
	}
	return result, err
}

func redactSessionRelations(ctx context.Context, tx pgx.Tx, caller Caller, env uuid.UUID, views []SessionView) error {
	cache := make(map[uuid.UUID]bool)
	for _, view := range views {
		cache[view.ID] = true
	}
	// Human/API reads reuse the list predicate and resolve every related ID in
	// one query. Runtime reads retain their stronger relationship checker.
	if caller.Kind != "session" && caller.SlackThreadID == uuid.Nil() {
		ids := make([]uuid.UUID, 0)
		add := func(id uuid.UUID) {
			if _, ok := cache[id]; !ok {
				cache[id] = false
				ids = append(ids, id)
			}
		}
		for _, view := range views {
			add(view.RootSessionID)
			if view.ParentSessionID != nil {
				add(*view.ParentSessionID)
			}
			if view.RequesterSessionID != nil {
				add(*view.RequesterSessionID)
			}
			for _, hold := range view.Holds {
				add(hold.SessionID)
			}
		}
		if len(ids) > 0 {
			filter, err := readScopeFor(ctx, tx, caller, env)
			if err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT id FROM sessions WHERE environment_id=$1 AND `+sessionReadPredicate+` AND id=ANY($9::uuid[])`, env, filter.All, filter.User, filter.Key, filter.AllAgents, filter.Agents, filter.ViewAgents, nullableID(filter.SharedConnection), ids)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id uuid.UUID
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				cache[id] = true
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
	}
	visible := func(id uuid.UUID) (bool, error) {
		if allowed, ok := cache[id]; ok {
			return allowed, nil
		}
		err := authorizeSessionRead(ctx, tx, caller, env, id)
		if err != nil && !errors.Is(err, ErrDenied) && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
		cache[id] = err == nil
		return err == nil, nil
	}
	for i := range views {
		view := &views[i]
		for _, field := range []**uuid.UUID{&view.ParentSessionID, &view.RequesterSessionID} {
			if *field == nil {
				continue
			}
			allowed, err := visible(**field)
			if err != nil {
				return err
			}
			if !allowed {
				*field = nil
			}
		}
		if allowed, err := visible(view.RootSessionID); err != nil {
			return err
		} else if !allowed {
			view.RootSessionID = uuid.Nil()
		}
		for i := range view.Holds {
			hold := &view.Holds[i]
			allowed, err := visible(hold.SessionID)
			if err != nil {
				return err
			}
			if !allowed {
				hold.ID = uuid.Nil()
				hold.SessionID = uuid.Nil()
				hold.Reason = "An ancestor session is held"
				hold.RecoveryGap = nil
			}
		}
	}
	return nil
}

// Metadata discovery records only selections containing unredacted free-form holds.
func recordSessionHoldsRead(ctx context.Context, policy auditpolicy.Policy, tx pgx.Tx, caller Caller, env uuid.UUID, views []SessionView) error {
	for _, view := range views {
		for _, hold := range view.Holds {
			if hold.ID != uuid.Nil() {
				if err := RecordContentRead(ctx, policy, tx, caller, env, view.ID); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

// AuthorizeSessionRead checks a metadata-only invalidation subscription without
// selecting or disclosing Session content.
func AuthorizeSessionRead(ctx context.Context, pool db.TxBeginner, caller Caller, env, id uuid.UUID) error {
	return readTransaction(ctx, pool, caller, env, func(tx pgx.Tx) error { return authorizeSessionRead(ctx, tx, caller, env, id) })
}
