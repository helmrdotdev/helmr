package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"strings"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/org"

	"github.com/helmrdotdev/helmr/internal/slackroute"

	"github.com/helmrdotdev/helmr/internal/lifetime"

	"github.com/helmrdotdev/helmr/internal/content"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// StartRequest contains submitted identity, never resolved defaults. Runtime
// Agent references are definition keys in the caller's pinned Deployment.
// A start that names a Slack channel is admitted through the Slack owner.
type StartRequest struct {
	CloseAfterIdle lifetime.Policy
	EnvironmentID  uuid.UUID
	Agent          string
	ComputerID     uuid.UUID
	SessionKey     *string
	SlackChannelID *string
	IdempotencyKey string
	Input          json.RawMessage
}

// StartPlan is a validated start request and the identity of its receipt.
type StartPlan struct {
	AuditPolicy auditpolicy.Policy
	Caller      Caller
	Request     StartRequest
	Method      string
	Digest      [32]byte
	HasInput    bool
}

// StartPin is what a start resolved: the Agent and the Deployment its Session
// is pinned to. Fresh is false when the start returned a retained receipt.
type StartPin struct {
	AgentID, DeploymentID uuid.UUID
	Fresh                 bool
}

func Start(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, trust computer.TrustIssuer, caller Caller, req StartRequest) (Admission, error) {
	return admitSession(ctx, auditPolicy, pool, trust, caller, req, "create")
}
func Spawn(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, trust computer.TrustIssuer, caller Caller, req StartRequest) (Admission, error) {
	return admitSession(ctx, auditPolicy, pool, trust, caller, req, "spawn")
}

// Creation commits a durable Session receipt, optionally with its first Turn and
// Computer admission. Empty Sessions pin code without allocating execution.
func admitSession(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, trust computer.TrustIssuer, caller Caller, req StartRequest, method string) (Admission, error) {
	if req.SlackChannelID != nil {
		return Admission{}, ErrInvalidInput
	}
	plan, err := PlanStart(auditPolicy, caller, req, method)
	if err != nil {
		return Admission{}, err
	}
	var result Admission
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := LockStartCaller(ctx, tx, plan); err != nil {
			return err
		}
		var err error
		result, _, err = AdmitStart(ctx, tx, trust, plan)
		return err
	})
	if err != nil {
		return Admission{}, StartError(err)
	}
	return result, nil
}

// PlanStart validates a start request and computes its idempotency digest.
func PlanStart(auditPolicy auditpolicy.Policy, caller Caller, req StartRequest, method string) (StartPlan, error) {
	if !req.CloseAfterIdle.Valid() || req.EnvironmentID == uuid.Nil() || !definition.ValidDeclaredID(req.Agent) || len(req.IdempotencyKey) > 512 || !utf8.ValidString(req.IdempotencyKey) || strings.ContainsRune(req.IdempotencyKey, 0) || (req.SessionKey != nil && (len(*req.SessionKey) == 0 || len(*req.SessionKey) > 512 || !utf8.ValidString(*req.SessionKey) || strings.ContainsRune(*req.SessionKey, 0))) {
		return StartPlan{}, ErrInvalidInput
	}
	if req.SlackChannelID != nil && (!definition.ValidSlackChannelID(*req.SlackChannelID) || caller.Kind == "session") {
		return StartPlan{}, ErrInvalidInput
	}
	if caller.ID == uuid.Nil() || (method == "spawn" && caller.Kind != "session") {
		return StartPlan{}, ErrDenied
	}
	if caller.Kind == "session" && (caller.Execution.EnvironmentID != req.EnvironmentID || caller.Execution.SessionID != caller.ID || req.SessionKey != nil) {
		return StartPlan{}, ErrDenied
	}
	if caller.Kind == "session" && req.IdempotencyKey == "" {
		return StartPlan{}, ErrInvalidInput
	}
	hasInput := req.Input != nil
	if hasInput || caller.Kind == "session" || method == "spawn" {
		var err error
		req.Input, err = content.Invocation(req.Input)
		if err != nil {
			return StartPlan{}, err
		}
	}
	raw, err := json.Marshal(struct {
		CloseAfterIdle lifetime.Policy `json:",omitzero"`
		Agent          string
		Computer       any
		SessionKey     *string
		Input          json.RawMessage
		SlackChannelID *string
	}{req.CloseAfterIdle, req.Agent, nullableID(req.ComputerID), req.SessionKey, req.Input, req.SlackChannelID})
	if err != nil {
		return StartPlan{}, ErrInvalidInput
	}
	digest, err := digestJSON(raw)
	if err != nil {
		return StartPlan{}, ErrInvalidInput
	}
	return StartPlan{AuditPolicy: auditPolicy, Caller: caller, Request: req, Method: method, Digest: digest, HasInput: hasInput}, nil
}

// StartError reports a start failure the way transports expect: storage
// rejects of the input are invalid input, and missing rows are denials.
func StartError(err error) error {
	var invalid *pgconn.PgError
	if errors.As(err, &invalid) && strings.HasPrefix(invalid.Code, "22") {
		return fmt.Errorf("%w: input cannot be stored", ErrInvalidInput)
	}
	return hideMissing(err)
}

// LockStartCaller takes a runtime caller's worker supply gate (step 2 of the
// global order) and checks the caller may start Sessions. Slack route locks
// (step 3) follow it, then AdmitStart.
func LockStartCaller(ctx context.Context, tx pgx.Tx, p StartPlan) error {
	if err := workergroup.LockExecutionSupply(ctx, tx, p.Request.EnvironmentID); err != nil {
		return err
	}
	if p.Caller.Kind == "session" {
		if err := lockRuntimeHost(ctx, tx, p.Caller); err != nil {
			return err
		}
		return RequireRootCaller(ctx, tx, p.Caller)
	}
	return authorizeStart(ctx, tx, p.Caller, p.Request.EnvironmentID)
}

// AdmitStart creates the planned Session, with its first Turn and Computer
// when the plan has input, or returns the retained receipt of an identical
// start. The caller holds LockStartCaller and any Slack route locks.
func AdmitStart(ctx context.Context, tx pgx.Tx, trust computer.TrustIssuer, p StartPlan) (Admission, StartPin, error) {
	caller, req, method, digest, hasInput := p.Caller, p.Request, p.Method, p.Digest, p.HasInput
	var result Admission
	var pin StartPin
	err := func() error {
		var rootThread uuid.UUID
		if caller.Kind == "session" && method == "spawn" {
			sourceRoute, err := slackroute.ChannelOf(ctx, tx, req.EnvironmentID, caller.ID)
			if err != nil {
				return err
			}
			if sourceRoute != nil {
				if _, err := slackroute.LockChannels(ctx, tx, req.EnvironmentID, []uuid.UUID{*sourceRoute}); err != nil {
					return err
				}
			}
			rootThread, err = slackroute.RootThread(ctx, tx, req.EnvironmentID, caller.ID)
			if err != nil {
				return err
			}
		}
		policy, err := org.LockAdmission(ctx, tx, req.EnvironmentID)
		if err != nil {
			return err
		}
		var agentID uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT id FROM agents WHERE environment_id=$1 AND key=$2`, req.EnvironmentID, req.Agent).Scan(&agentID); err != nil {
			return err
		}
		lineage, err := resolveStartOrigin(ctx, tx, caller, req, agentID)
		if err != nil {
			return err
		}
		if err = requireAgentAvailable(ctx, tx, req.EnvironmentID, agentID); err != nil {
			return err
		}
		if caller.Kind == "session" {
			op := "start"
			if method == "spawn" {
				op = "spawn"
			}
			if err := requireMCPCreation(ctx, tx, caller, op, req); err != nil {
				return err
			}
		}
		prior, err := sessionCreationReceipt(ctx, tx, caller, req.EnvironmentID, agentID, method, req.IdempotencyKey, digest)
		if err != nil {
			return err
		}
		pin.AgentID = agentID
		if prior != nil {
			result = *prior
			return nil
		}
		if req.SessionKey != nil {
			var occupied bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE environment_id=$1 AND agent_id=$2 AND key=$3)`, req.EnvironmentID, agentID, *req.SessionKey).Scan(&occupied); err != nil {
				return err
			}
			if occupied {
				return ErrConflict
			}
		}
		var deployment uuid.UUID
		if caller.Kind == "session" && method == "spawn" {
			if err = tx.QueryRow(ctx, `SELECT deployment_id FROM sessions WHERE environment_id=$1 AND id=$2`, req.EnvironmentID, caller.ID).Scan(&deployment); err != nil {
				return err
			}
		} else {
			if policy.Deployment == nil {
				return ErrNotReady
			}
			deployment = *policy.Deployment
		}
		var templateKey, inputKind string
		var duration *int64
		var never bool
		if err = tx.QueryRow(ctx, `SELECT t.key,d.close_after_idle_ms,d.close_after_idle_never,d.input_kind FROM agent_definitions d JOIN templates t ON (t.environment_id,t.id)=(d.environment_id,d.template_id) WHERE d.environment_id=$1 AND d.agent_id=$2 AND d.deployment_id=$3`, req.EnvironmentID, agentID, deployment).Scan(&templateKey, &duration, &never, &inputKind); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotReady
			}
			return err
		}
		if req.SlackChannelID != nil && inputKind != "text" {
			return ErrInvalidInput
		}
		if hasInput {
			req.Input, err = content.InvocationFor(inputKind, req.Input)
			if err != nil {
				return err
			}
			if inputKind == "text" && bytes.Equal(req.Input, []byte("[]")) {
				return ErrInvalidInput
			}
		}
		pin.DeploymentID, pin.Fresh = deployment, true
		computerID := req.ComputerID
		if computerID == uuid.Nil() && hasInput {
			computerID, err = computer.CreateForAdmission(ctx, tx, trust, req.EnvironmentID, deployment, templateKey, computerLimits(policy))
			if err != nil {
				return err
			}
		}
		var callerIDs, computerIDs []uuid.UUID
		if caller.Kind == "session" {
			callerIDs = append(callerIDs, caller.ID)
		}
		if computerID != uuid.Nil() {
			computerIDs = append(computerIDs, computerID)
		}
		locked, err := LockSessionsAndComputers(ctx, tx, req.EnvironmentID, callerIDs, computerIDs)
		if err != nil {
			return err
		}
		if caller.Kind == "session" {
			if err = authorizeEnqueue(ctx, tx, caller, req.EnvironmentID, locked[caller.ID]); err != nil {
				return err
			}
		} else if err = authorizeStart(ctx, tx, caller, req.EnvironmentID); err != nil {
			return err
		}
		if err = lineage.authorize(ctx, tx, req.EnvironmentID, agentID); err != nil {
			return err
		}
		if computerID != uuid.Nil() {
			if err = computerAcceptsAdmission(ctx, tx, req.EnvironmentID, computerID, true); err != nil {
				return err
			}
			if req.ComputerID != uuid.Nil() {
				var profile string
				if err = tx.QueryRow(ctx, `SELECT spec.seed->>'profile' FROM computers c JOIN image_specs spec ON (spec.environment_id,spec.id)=(c.environment_id,c.image_spec_id) WHERE c.environment_id=$1 AND c.id=$2`, req.EnvironmentID, computerID).Scan(&profile); err != nil {
					return err
				}
				if profile != definition.ComputerSeedProfile {
					return ErrInvalidInput
				}
			}
		}
		if hasInput {
			err = consumeAdmission(ctx, tx, req.EnvironmentID, policy)
		} else {
			err = consumeAdmissionToken(ctx, tx, req.EnvironmentID)
		}
		if err != nil {
			return err
		}
		if computerID != uuid.Nil() {
			if err = computer.NoteExecutionDemand(ctx, p.AuditPolicy, tx, req.EnvironmentID, computerID); err != nil {
				return err
			}
		}
		result.SessionID, result.Created = uuid.NewV7(), true
		result.CloseAfterIdleMs = lifetime.Resolve(req.CloseAfterIdle, duration, never)
		if hasInput {
			result.TurnID, result.Sequence = uuid.NewV7(), 1
		}
		root := result.SessionID
		var parent, requester, origin any
		causal := 0
		if caller.Kind == "session" {
			if err = tx.QueryRow(ctx, `SELECT causal_depth FROM sessions WHERE environment_id=$1 AND id=$2`, req.EnvironmentID, caller.ID).Scan(&causal); err != nil {
				return err
			}
			causal++
			requester, origin = caller.ID, nullableID(caller.TurnID)
			if method == "spawn" {
				parent, root = caller.ID, locked[caller.ID].root
			}
			if causal > policy.MaxCausal {
				return ErrInvalidInput
			}
		}
		if _, err = tx.Exec(ctx, `INSERT INTO sessions(environment_id,id,agent_id,deployment_id,computer_id,root_session_id,parent_session_id,requester_session_id,origin_turn_id,causal_depth,key,close_after_idle_ms,creation_caller_kind,creation_caller_id,creation_method,creation_idempotency_key,creation_digest,creation_turn_id,idle_since,next_turn_seq,origin_user_id,origin_api_key_id,origin_activation_id,origin_slack_connection_id)
 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,NULLIF($16,''),$17,$18,CASE WHEN $18::uuid IS NULL THEN clock_timestamp() END,CASE WHEN $18::uuid IS NULL THEN 1 ELSE 2 END,$19,$20,$21,$22 FROM environments WHERE id=$1`, req.EnvironmentID, result.SessionID, agentID, deployment, nullableID(computerID), root, parent, requester, origin, causal, req.SessionKey, result.CloseAfterIdleMs, caller.Kind, caller.ID, method, req.IdempotencyKey, digest[:], nullableID(result.TurnID), lineage.User, lineage.Key, lineage.Activation, lineage.Connection); err != nil {
			return err
		}
		if lineage.User != nil && lineage.Connection == nil {
			if err = saveSessionHuman(ctx, tx, req.EnvironmentID, result.SessionID, lineage.Human); err != nil {
				return err
			}
		}
		if err = inheritComputerRecoveryGap(ctx, tx, req.EnvironmentID, result.SessionID, computerID); err != nil {
			return err
		}
		if parent != nil {
			if _, err = tx.Exec(ctx, `UPDATE sessions SET idle_since=NULL WHERE environment_id=$1 AND id=$2`, req.EnvironmentID, parent); err != nil {
				return err
			}
		}
		if rootThread != uuid.Nil() {
			joined, err := slackroute.JoinRootThread(ctx, tx, req.EnvironmentID, result.SessionID, rootThread)
			if err != nil {
				return err
			}
			if !joined {
				return ErrDenied
			}
		}
		if hasInput {
			if _, err = tx.Exec(ctx, `INSERT INTO turns(environment_id,id,session_id,computer_id,seq,caller_kind,caller_id,admission_method,target_id,idempotency_key,request_digest,input,origin_turn_id) VALUES($1,$2,$3,$4,1,$5,$6,$7,$8,NULLIF($9,''),$10,$11,$12)`, req.EnvironmentID, result.TurnID, result.SessionID, computerID, caller.Kind, caller.ID, method, agentID, req.IdempotencyKey, digest[:], req.Input, nullableID(caller.TurnID)); err != nil {
				return err
			}
			if err = saveTurnHuman(ctx, tx, req.EnvironmentID, result.TurnID, caller); err != nil {
				return err
			}
		}
		if err = auditStart(ctx, tx, p, result, agentID, deployment, computerID); err != nil {
			return err
		}
		if hasInput {
			return event(ctx, tx, req.EnvironmentID, result.SessionID, result.TurnID, "turn.queued")
		}
		return notifySessionsChanged(ctx, tx, req.EnvironmentID, result.SessionID)
	}()
	return result, pin, err
}

// StartReceipt returns the retained receipt of an identical earlier start
// and, for a fresh start, the Agent it names after the checks a start makes
// before any remote work: caller authority, the invoking Turn and the
// Session key.
func StartReceipt(ctx context.Context, pool db.TxBeginner, p StartPlan) (*Admission, uuid.UUID, error) {
	caller, req, method := p.Caller, p.Request, p.Method
	var receipt *Admission
	var agentID uuid.UUID
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := LockStartCaller(ctx, tx, p); err != nil {
			return err
		}
		var err error
		if caller.Kind == "session" && method == "spawn" {
			err = tx.QueryRow(ctx, `SELECT d.agent_id FROM sessions s JOIN agent_definitions d ON (d.environment_id,d.deployment_id)=(s.environment_id,s.deployment_id) JOIN agents a ON (a.environment_id,a.id)=(d.environment_id,d.agent_id) WHERE s.environment_id=$1 AND s.id=$2 AND a.key=$3`, req.EnvironmentID, caller.ID, req.Agent).Scan(&agentID)
		} else {
			err = tx.QueryRow(ctx, `SELECT id FROM agents WHERE environment_id=$1 AND key=$2`, req.EnvironmentID, req.Agent).Scan(&agentID)
		}
		if err != nil {
			return err
		}
		if _, err = resolveStartOrigin(ctx, tx, caller, req, agentID); err != nil {
			return err
		}
		receipt, err = sessionCreationReceipt(ctx, tx, caller, req.EnvironmentID, agentID, method, req.IdempotencyKey, p.Digest)
		if err != nil || receipt != nil {
			return err
		}
		if caller.Kind == "session" {
			owner, e := lockSession(ctx, tx, req.EnvironmentID, caller.ID)
			if e != nil {
				return e
			}
			if e = authorizeEnqueue(ctx, tx, caller, req.EnvironmentID, owner); e != nil {
				return e
			}
		}
		if req.SessionKey != nil {
			var occupied bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE environment_id=$1 AND agent_id=$2 AND key=$3)`, req.EnvironmentID, agentID, *req.SessionKey).Scan(&occupied); err != nil {
				return err
			}
			if occupied {
				return ErrConflict
			}
		}
		return nil
	})
	if err != nil {
		return nil, uuid.Nil(), hideMissing(err)
	}
	return receipt, agentID, nil
}

// A creation receipt never depends on a Turn existing or the Agent remaining in
// current. Caller authority is still checked on each observation.
func sessionCreationReceipt(ctx context.Context, tx pgx.Tx, caller Caller, env, agentID uuid.UUID, method, retry string, digest [32]byte) (*Admission, error) {
	if retry == "" {
		return nil, nil
	}
	var result Admission
	var turn *uuid.UUID
	var prior []byte
	err := tx.QueryRow(ctx, `SELECT id,creation_turn_id,creation_digest,close_after_idle_ms FROM sessions WHERE environment_id=$1 AND creation_caller_kind=$2 AND creation_caller_id=$3 AND creation_method=$4 AND agent_id=$5 AND creation_idempotency_key=$6`, env, caller.Kind, caller.ID, method, agentID, retry).Scan(&result.SessionID, &turn, &prior, &result.CloseAfterIdleMs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if caller.Kind == "session" {
		owner, err := lockSession(ctx, tx, env, caller.ID)
		if err != nil {
			return nil, err
		}
		if err = executionReceipt(ctx, tx, caller.Execution, owner); err != nil {
			return nil, err
		}
	} else if err = authorizeStart(ctx, tx, caller, env); err != nil {
		return nil, err
	}
	if err := authorizeTarget(ctx, tx, caller, env, result.SessionID, auth.PermissionSessionsRead); err != nil {
		return nil, err
	}
	if !bytes.Equal(prior, digest[:]) {
		return nil, ErrConflict
	}
	result.Created = true
	if turn != nil {
		result.TurnID, result.Sequence = *turn, 1
	}
	return &result, nil
}

func authorizeStart(ctx context.Context, tx pgx.Tx, caller Caller, env uuid.UUID) error {
	_, err := callerPrincipal(ctx, tx, caller, env)
	return accessError(err)
}

func computerAcceptsAdmission(ctx context.Context, tx pgx.Tx, env, computerID uuid.UUID, newPlacement bool) error {
	var available bool
	if err := tx.QueryRow(ctx, `SELECT preparation_failed_at IS NULL AND deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM (`+db.RevokedComputerSecretsSQL+`) revoked_secrets WHERE environment_id=$1 AND computer_id=$2) AND (NOT $3 OR (integrity_fault_at IS NULL AND (initial_root_id IS NOT NULL OR preparation_deadline_at>clock_timestamp()))) FROM computers WHERE environment_id=$1 AND id=$2`, env, computerID, newPlacement).Scan(&available); err != nil {
		return err
	}
	if !available {
		return ErrNotReady
	}
	return computer.MarkUsed(ctx, tx, env, computerID)
}
