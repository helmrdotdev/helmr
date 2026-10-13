package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/slack"

	"github.com/helmrdotdev/helmr/internal/slackroute"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/jackc/pgx/v5"
)

// TriggerLateness bounds catch-up after a delay or outage: only the latest due
// occurrence within this window of its scheduled time starts.
const TriggerLateness = 5 * time.Minute

type triggerActivation struct {
	slackChannelID                     *uuid.UUID
	slackRouteActive                   bool
	environment, id, agent, deployment uuid.UUID
	trigger, cron, timezone            string
	input                              json.RawMessage
	from, next                         time.Time
	until                              *time.Time
}

// triggerEvaluation selects only the latest due instant inside the activation
// and recovery grace. At most the stored cursor and that instant are evaluated;
// outages never expand into an unbounded sequence of admissions.
type triggerEvaluation struct {
	latest *time.Time
	missed *time.Time
	next   time.Time
}

func evaluateTrigger(s triggerActivation, now time.Time) (triggerEvaluation, error) {
	end := now
	if s.until != nil && !end.Before(*s.until) {
		end = s.until.Add(-time.Microsecond)
	}
	if s.next.After(end) {
		return triggerEvaluation{}, nil
	}
	result := triggerEvaluation{}
	lower := now.Add(-TriggerLateness)
	if lower.Before(s.next) {
		lower = s.next
	}
	candidate, err := definition.NextCronTime(s.cron, s.timezone, lower.Add(-time.Nanosecond))
	if err != nil {
		return result, err
	}
	for !candidate.After(end) {
		latest := candidate
		result.latest = &latest
		candidate, err = definition.NextCronTime(s.cron, s.timezone, candidate)
		if err != nil {
			return result, err
		}
	}
	// For a closed activation, move outside its half-open interval even when
	// its last instant is older than the recovery grace.
	result.next, err = definition.NextCronTime(s.cron, s.timezone, end)
	if err != nil {
		return result, err
	}
	if result.latest == nil || !result.latest.Equal(s.next) {
		missed := s.next
		result.missed = &missed
	}
	return result, nil
}

// EvaluateTrigger serializes with Environment promotion. Occurrence identity,
// admission (or its rejected/missed disposition), and the next cursor commit
// together. The activation's pinned definition is never replaced by current defaults.
func EvaluateTrigger(ctx context.Context, pool db.TxBeginner, trust computer.TrustIssuer, env, id uuid.UUID) error {
	if env == uuid.Nil() || id == uuid.Nil() {
		return ErrInvalidInput
	}
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var route *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT slack_channel_id FROM trigger_activations WHERE environment_id=$1 AND id=$2`, env, id).Scan(&route); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		routeActive := true
		if route != nil {
			var err error
			routeActive, err = slackroute.LockChannels(ctx, tx, env, []uuid.UUID{*route})
			if err != nil {
				return err
			}
		}
		policy, err := org.LockAdmission(ctx, tx, env)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		s := triggerActivation{environment: env, id: id, slackChannelID: route, slackRouteActive: routeActive}
		err = tx.QueryRow(ctx, `SELECT a.agent_id,a.deployment_id,a.trigger_key,t.cron,t.timezone,t.input,a.active_from,a.active_until,a.next_fire_at FROM trigger_activations a
 JOIN agent_triggers t ON (t.environment_id,t.agent_id,t.deployment_id,t.key)=(a.environment_id,a.agent_id,a.deployment_id,a.trigger_key)
 WHERE a.environment_id=$1 AND a.id=$2 FOR NO KEY UPDATE OF a`, env, id).Scan(&s.agent, &s.deployment, &s.trigger, &s.cron, &s.timezone, &s.input, &s.from, &s.until, &s.next)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var now time.Time
		if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		evaluation, err := evaluateTrigger(s, now)
		if err != nil {
			return err
		}
		if evaluation.next.IsZero() {
			return nil
		}
		if evaluation.missed != nil {
			reason := "superseded"
			if evaluation.latest == nil {
				reason = "lateness_exceeded"
			}
			if err = recordTriggerOccurrence(ctx, tx, s, *evaluation.missed, session.Admission{}, "missed", reason, now); err != nil {
				return err
			}
		}
		if evaluation.latest != nil {
			var exists bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM trigger_occurrences WHERE environment_id=$1 AND agent_id=$2 AND trigger_key=$3 AND scheduled_at=$4)`, env, s.agent, s.trigger, *evaluation.latest).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				// Terminal admission rejection rolls back fresh work but records a
				// disposition. Temporary shortages leave the whole occurrence retryable.
				admissionTX, err := tx.Begin(ctx)
				if err != nil {
					return err
				}
				receipt, admissionErr := admitScheduledSession(ctx, admissionTX, trust, s, *evaluation.latest, policy)
				disposition, reason := "admitted", ""
				if admissionErr != nil {
					if err = admissionTX.Rollback(ctx); err != nil {
						return err
					}
					switch {
					case errors.Is(admissionErr, slackroute.ErrChannelUnavailable):
						reason = "slack_channel_unavailable"
					case errors.Is(admissionErr, ErrDenied):
						reason = "execution_denied"
					case errors.Is(admissionErr, ErrInvalidInput):
						reason = "invalid_definition"
					default:
						return admissionErr
					}
					receipt = session.Admission{}
					disposition = "rejected"
				} else if err = admissionTX.Commit(ctx); err != nil {
					return err
				}
				if err = recordTriggerOccurrence(ctx, tx, s, *evaluation.latest, receipt, disposition, reason, now); err != nil {
					return err
				}
			}
		}
		_, err = tx.Exec(ctx, `UPDATE trigger_activations SET next_fire_at=$3 WHERE environment_id=$1 AND id=$2`, env, id, evaluation.next)
		return err
	})
}

func recordTriggerOccurrence(ctx context.Context, tx pgx.Tx, s triggerActivation, at time.Time, a session.Admission, disposition, reason string, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO trigger_occurrences(environment_id,activation_id,agent_id,trigger_key,scheduled_at,session_id,turn_id,disposition,reason,evaluated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10) ON CONFLICT(environment_id,agent_id,trigger_key,scheduled_at) DO NOTHING`, s.environment, s.id, s.agent, s.trigger, at, nullableID(a.SessionID), nullableID(a.TurnID), disposition, reason, now)
	return err
}

// admitScheduledSession admits a trigger occurrence's Session and, for a
// Slack destination, its Slack root, in the evaluation transaction.
func admitScheduledSession(ctx context.Context, tx pgx.Tx, trust computer.TrustIssuer, s triggerActivation, at time.Time, policy org.AdmissionPolicy) (session.Admission, error) {
	if s.slackChannelID != nil && !s.slackRouteActive {
		return session.Admission{}, slackroute.ErrChannelUnavailable
	}
	receipt, err := session.AdmitScheduled(ctx, tx, trust, session.ScheduledStart{EnvironmentID: s.environment, ActivationID: s.id, AgentID: s.agent, DeploymentID: s.deployment, Input: s.input, At: at}, policy)
	if err != nil {
		return session.Admission{}, err
	}
	if s.slackChannelID != nil {
		if err = slack.CreateScheduledRoot(ctx, tx, s.environment, receipt.SessionID, receipt.TurnID, *s.slackChannelID, s.agent, s.deployment, at); err != nil {
			return session.Admission{}, err
		}
	}
	return receipt, nil
}
