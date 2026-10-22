package session

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/content"
	"github.com/helmrdotdev/helmr/internal/lifetime"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/jackc/pgx/v5"
)

// ScheduledStart is a trigger occurrence's Session: the activation that fired,
// its pinned Agent definition and the occurrence's scheduled time.
type ScheduledStart struct {
	EnvironmentID, ActivationID, AgentID, DeploymentID uuid.UUID
	Input                                              json.RawMessage
	At                                                 time.Time
}

// AdmitScheduled creates the Session, Computer and first Turn of a trigger
// occurrence in the caller's transaction. The caller holds the Environment
// admission lock and passes its policy.
func AdmitScheduled(ctx context.Context, tx pgx.Tx, trust computer.TrustIssuer, start ScheduledStart, policy org.AdmissionPolicy) (Admission, error) {
	s := struct {
		environment, id, agent, deployment uuid.UUID
		input                              json.RawMessage
	}{start.EnvironmentID, start.ActivationID, start.AgentID, start.DeploymentID, start.Input}
	at := start.At
	var key, inputKind string
	var idle *int64
	var never bool
	var suspension string
	if err := tx.QueryRow(ctx, `SELECT t.key,d.close_after_idle_ms,d.close_after_idle_never,d.input_kind,d.suspension FROM agent_definitions d JOIN templates t ON (t.environment_id,t.id)=(d.environment_id,d.template_id) WHERE d.environment_id=$1 AND d.agent_id=$2 AND d.deployment_id=$3`, s.environment, s.agent, s.deployment).Scan(&key, &idle, &never, &inputKind, &suspension); err != nil {
		return Admission{}, err
	}
	input, err := content.InvocationFor(inputKind, s.input)
	if err != nil {
		return Admission{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	s.input = input
	if err := consumeAdmission(ctx, tx, s.environment, policy); err != nil {
		return Admission{}, err
	}
	computerID, err := computer.CreateForAdmission(ctx, tx, trust, s.environment, s.deployment, key, computerLimits(policy))
	if err != nil {
		return Admission{}, err
	}
	if err = computerAcceptsAdmission(ctx, tx, s.environment, computerID, true); err != nil {
		return Admission{}, err
	}
	if err = computer.NoteExecutionDemand(ctx, tx, s.environment, computerID); err != nil {
		return Admission{}, err
	}
	idle = lifetime.Resolve(lifetime.Policy{}, idle, never)
	receipt := Admission{CloseAfterIdleMs: idle, SessionID: uuid.NewV7(), TurnID: uuid.NewV7(), Sequence: 1, Created: true}
	if _, err = tx.Exec(ctx, `INSERT INTO sessions(environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth,next_turn_seq,close_after_idle_ms,suspension) SELECT $1,$2,$3,$4,$5,$2,0,2,$6,$7 FROM environments WHERE id=$1`, s.environment, receipt.SessionID, s.agent, s.deployment, computerID, idle, suspension); err != nil {
		return Admission{}, err
	}
	digest, err := digestJSON(s.input)
	if err != nil {
		return Admission{}, fmt.Errorf("trigger input: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO turns(environment_id,id,session_id,computer_id,seq,caller_kind,caller_id,admission_method,target_id,idempotency_key,request_digest,input) VALUES($1,$2,$3,$4,1,'schedule',$5,'schedule',$6,$7,$8,$9)`, s.environment, receipt.TurnID, receipt.SessionID, computerID, s.id, s.agent, at.UTC().Format(time.RFC3339Nano), digest[:], s.input); err != nil {
		return Admission{}, err
	}
	if err = event(ctx, tx, s.environment, receipt.SessionID, receipt.TurnID, "turn.queued"); err != nil {
		return Admission{}, err
	}
	return receipt, nil
}
