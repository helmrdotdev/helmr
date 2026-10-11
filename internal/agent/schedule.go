package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/conversation"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/jackc/pgx/v5"
)

type scheduleActivation struct {
	slackChannelID                     *uuid.UUID
	slackRouteActive                   bool
	environment, id, agent, deployment uuid.UUID
	trigger, cron, timezone            string
	input                              json.RawMessage
	from, next                         time.Time
	until                              *time.Time
	graceMS                            int64
}

// scheduleEvaluation selects only the latest due instant inside the activation
// and recovery grace. At most the stored cursor and that instant are evaluated;
// outages never expand into an unbounded sequence of admissions.
type scheduleEvaluation struct {
	latest *time.Time
	missed *time.Time
	next   time.Time
}

func evaluateSchedule(s scheduleActivation, now time.Time) (scheduleEvaluation, error) {
	if s.graceMS <= 0 || s.graceMS > math.MaxInt64/int64(time.Millisecond) {
		return scheduleEvaluation{}, ErrInvalidInput
	}
	end := now
	if s.until != nil && !end.Before(*s.until) {
		end = s.until.Add(-time.Microsecond)
	}
	if s.next.After(end) {
		return scheduleEvaluation{}, nil
	}
	result := scheduleEvaluation{}
	lower := now.Add(-time.Duration(s.graceMS) * time.Millisecond)
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

// EvaluateSchedule serializes with Environment promotion. Occurrence identity,
// admission (or its rejected/missed disposition), and the next cursor commit
// together. The activation's pinned definition is never replaced by current defaults.
func EvaluateSchedule(ctx context.Context, pool db.TxBeginner, trust ComputerTrustIssuer, env, id uuid.UUID) error {
	if env == uuid.Nil() || id == uuid.Nil() {
		return ErrInvalidInput
	}
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var route *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT slack_channel_id FROM agent_schedules WHERE environment_id=$1 AND id=$2`, env, id).Scan(&route); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		routeActive := true
		if route != nil {
			var err error
			routeActive, err = LockSlackChannels(ctx, tx, env, []uuid.UUID{*route})
			if err != nil {
				return err
			}
		}
		policy, err := lockAdmissionEnvironment(ctx, tx, env)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		s := scheduleActivation{environment: env, id: id, slackChannelID: route, slackRouteActive: routeActive}
		err = tx.QueryRow(ctx, `SELECT agent_id,deployment_id,trigger_key,cron,timezone,input,active_from,active_until,next_fire_at,lateness_tolerance_ms FROM agent_schedules WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, id).Scan(&s.agent, &s.deployment, &s.trigger, &s.cron, &s.timezone, &s.input, &s.from, &s.until, &s.next, &s.graceMS)
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
		evaluation, err := evaluateSchedule(s, now)
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
			if err = recordScheduleOccurrence(ctx, tx, s, *evaluation.missed, Admission{}, "missed", reason, now); err != nil {
				return err
			}
		}
		if evaluation.latest != nil {
			var exists bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_schedule_occurrences WHERE environment_id=$1 AND agent_id=$2 AND trigger_key=$3 AND scheduled_at=$4)`, env, s.agent, s.trigger, *evaluation.latest).Scan(&exists); err != nil {
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
					case errors.Is(admissionErr, ErrSlackChannelUnavailable):
						reason = "slack_channel_unavailable"
					case errors.Is(admissionErr, ErrDenied):
						reason = "execution_denied"
					case errors.Is(admissionErr, ErrInvalidInput):
						reason = "invalid_definition"
					default:
						return admissionErr
					}
					receipt = Admission{}
					disposition = "rejected"
				} else if err = admissionTX.Commit(ctx); err != nil {
					return err
				}
				if err = recordScheduleOccurrence(ctx, tx, s, *evaluation.latest, receipt, disposition, reason, now); err != nil {
					return err
				}
			}
		}
		_, err = tx.Exec(ctx, `UPDATE agent_schedules SET next_fire_at=$3 WHERE environment_id=$1 AND id=$2`, env, id, evaluation.next)
		return err
	})
}

func admitScheduledSession(ctx context.Context, tx pgx.Tx, trust ComputerTrustIssuer, s scheduleActivation, at time.Time, policy admissionEnvironment) (Admission, error) {
	if s.slackChannelID != nil && !s.slackRouteActive {
		return Admission{}, ErrSlackChannelUnavailable
	}
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT execution_revoked_at IS NULL FROM deployments WHERE environment_id=$1 AND id=$2 FOR SHARE`, s.environment, s.deployment).Scan(&allowed); err != nil {
		return Admission{}, err
	}
	if !allowed {
		return Admission{}, ErrDenied
	}
	input, err := conversation.Input(s.input)
	if err != nil {
		return Admission{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	s.input = input
	if err := consumeAdmission(ctx, tx, s.environment, policy); err != nil {
		return Admission{}, err
	}
	var key string
	if err := tx.QueryRow(ctx, `SELECT computer_definition_key FROM agent_definitions WHERE environment_id=$1 AND agent_id=$2 AND deployment_id=$3`, s.environment, s.agent, s.deployment).Scan(&key); err != nil {
		return Admission{}, err
	}
	computer, err := createAdmissionComputer(ctx, tx, trust, s.environment, s.deployment, key, policy)
	if err != nil {
		return Admission{}, err
	}
	if err = computerAcceptsAdmission(ctx, tx, s.environment, computer, true); err != nil {
		return Admission{}, err
	}
	receipt := Admission{SessionID: uuid.NewV7(), TurnID: uuid.NewV7(), Sequence: 1, Created: true}
	if _, err = tx.Exec(ctx, `INSERT INTO sessions(environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth,next_turn_seq,slack_channel_id,history_retention_mode,history_retention_seconds) SELECT $1,$2,$3,$4,$5,$2,0,2,$6,history_retention_mode,history_retention_seconds FROM environments WHERE id=$1`, s.environment, receipt.SessionID, s.agent, s.deployment, computer, s.slackChannelID); err != nil {
		return Admission{}, err
	}
	digest, err := digestJSON(s.input)
	if err != nil {
		return Admission{}, fmt.Errorf("schedule input: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO turns(environment_id,id,session_id,computer_id,seq,caller_kind,caller_id,admission_method,target_id,retry_key,request_digest,input) VALUES($1,$2,$3,$4,1,'schedule',$5,'schedule',$6,$7,$8,$9)`, s.environment, receipt.TurnID, receipt.SessionID, computer, s.id, s.agent, at.UTC().Format(time.RFC3339Nano), digest[:], s.input); err != nil {
		return Admission{}, err
	}
	if s.slackChannelID != nil {
		var channel string
		var publication uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT publication_id,slack_channel_id FROM slack_channels WHERE environment_id=$1 AND id=$2`, s.environment, *s.slackChannelID).Scan(&publication, &channel); err != nil {
			return Admission{}, err
		}
		connection, err := ReadSlackConnection(ctx, tx, s.environment, s.agent, channel)
		if err != nil {
			return Admission{}, err
		}
		if connection.PublicationID != publication || at.Before(connection.AuthorizedAt) {
			return Admission{}, ErrSlackChannelUnavailable
		}
		opening, _ := json.Marshal(map[string]any{"text": "Started " + connection.AgentName + ".", "mrkdwn": false, "parse": "none", "link_names": false, "unfurl_links": false, "unfurl_media": false})
		if err = createSlackRoot(ctx, tx, s.environment, receipt.SessionID, receipt.TurnID, *s.slackChannelID, SlackStartRoute{Target: connection, DeploymentID: s.deployment, Opening: opening}); err != nil {
			return Admission{}, err
		}
	}
	if err = event(ctx, tx, s.environment, receipt.SessionID, receipt.TurnID, "turn.queued"); err != nil {
		return Admission{}, err
	}
	return receipt, nil
}
func recordScheduleOccurrence(ctx context.Context, tx pgx.Tx, s scheduleActivation, at time.Time, a Admission, disposition, reason string, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO agent_schedule_occurrences(environment_id,schedule_id,agent_id,trigger_key,scheduled_at,session_id,turn_id,disposition,reason,evaluated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10) ON CONFLICT(environment_id,agent_id,trigger_key,scheduled_at) DO NOTHING`, s.environment, s.id, s.agent, s.trigger, at, nullableID(a.SessionID), nullableID(a.TurnID), disposition, reason, now)
	return err
}
