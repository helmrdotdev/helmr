package session

import (
	"context"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// OverviewHours is the window the overview counts Turns over, in buckets of
// one hour each counted back from the read.
const OverviewHours = 24

// overviewActiveLimit bounds the Sessions with work in progress one overview
// lists; the counts still cover all of them.
const overviewActiveLimit = 5

// overviewFailureLimit bounds the failure groups one overview returns.
const overviewFailureLimit = 10

// Overview summarizes the Sessions a caller can read in one Environment: Turns
// admitted in the last OverviewHours and the window before, activity by Agent,
// Sessions with work in progress, and failures grouped by Agent and error code.
type Overview struct {
	Turns, Failed                 int64
	PreviousTurns, PreviousFailed int64
	Agents                        []OverviewAgent
	Active                        []OverviewSession
	ActiveCount                   int64
	Running, Queued, Held         int64
	Failures                      []OverviewFailure
}

// OverviewAgent is one Agent's activity. Hourly and HourlyFailed hold one
// bucket per hour, oldest first; the last covers the hour before the read.
type OverviewAgent struct {
	Agent                string
	Active               int64
	Turns, Failed        int64
	Hourly, HourlyFailed [OverviewHours]int64
}

// OverviewSession is a Session with work in progress: held (by its own hold
// or its parent's subtree hold), running a Turn, or with Turns queued. Since is
// when that state began.
type OverviewSession struct {
	ID             uuid.UUID
	Agent          string
	State          string
	Since          time.Time
	Origin         string
	SlackChannelID *string
}

// OverviewFailure groups the window's failed Turns of one Agent with one error
// code. LastHour counts those that ended in the last hour; the Last fields name
// the most recent.
type OverviewFailure struct {
	Agent            string
	ErrorCode        string
	Count, LastHour  int64
	FirstAt, LastAt  time.Time
	LastSessionID    uuid.UUID
	LastTurnID       uuid.UUID
	LastTurnSequence int64
}

// overviewReadable restricts a query on `sessions` to those the caller can
// read, with the parameters of sessionReadPredicate.
const overviewReadable = `environment_id=$1 AND ` + sessionReadPredicate

const overviewWindow = `now()-interval '24 hours'`

const overviewTotalsSQL = `SELECT count(*) FILTER (WHERE t.created_at>=` + overviewWindow + `),count(*) FILTER (WHERE t.created_at>=` + overviewWindow + ` AND t.status='failed'),
 count(*) FILTER (WHERE t.created_at<` + overviewWindow + `),count(*) FILTER (WHERE t.created_at<` + overviewWindow + ` AND t.status='failed')
 FROM turns t JOIN (SELECT id FROM sessions WHERE ` + overviewReadable + `) v ON v.id=t.session_id
 WHERE t.environment_id=$1 AND t.created_at>=now()-interval '48 hours'`

// A Turn admitted after the transaction began still falls in the latest bucket.
const overviewAgentsSQL = `SELECT a.key,GREATEST(0,LEAST(23,floor(extract(epoch FROM now()-t.created_at)/3600)))::int,count(*),count(*) FILTER (WHERE t.status='failed')
 FROM turns t JOIN (SELECT id,agent_id FROM sessions WHERE ` + overviewReadable + `) v ON v.id=t.session_id JOIN agents a ON (a.environment_id,a.id)=(t.environment_id,v.agent_id)
 WHERE t.environment_id=$1 AND t.created_at>=` + overviewWindow + `
 GROUP BY 1,2`

// Work in progress starts from the small sets of running and queued Turns (one
// arm each, so each uses its partial index) and unreleased holds. A subtree hold on a Session covers its children.
const overviewActiveCTE = `WITH candidates AS (
  SELECT session_id FROM turns WHERE environment_id=$1 AND status='running'
  UNION SELECT session_id FROM turns WHERE environment_id=$1 AND status='queued'
  UNION SELECT session_id FROM session_holds WHERE environment_id=$1 AND released_at IS NULL
  UNION SELECT s.id FROM session_holds h JOIN sessions s ON s.environment_id=h.environment_id AND s.parent_session_id=h.session_id WHERE h.environment_id=$1 AND h.released_at IS NULL AND h.scope='subtree'
 ), visible AS (
  SELECT id,agent_id,parent_session_id,requester_session_id,origin_slack_connection_id,origin_activation_id,origin_api_key_id FROM sessions
  WHERE ` + overviewReadable + ` AND status IN ('open','closing') AND id IN (SELECT session_id FROM candidates)
 ), active AS (
  SELECT v.id,a.key AS agent,
   CASE WHEN h.at IS NOT NULL THEN 'held' WHEN r.at IS NOT NULL THEN 'running' ELSE 'queued' END AS state,
   COALESCE(h.at,r.at,q.at) AS since,
   CASE WHEN v.requester_session_id IS NOT NULL THEN 'session' WHEN v.origin_slack_connection_id IS NOT NULL THEN 'slack' WHEN v.origin_activation_id IS NOT NULL THEN 'trigger' WHEN v.origin_api_key_id IS NOT NULL THEN 'api_key' ELSE 'user' END AS origin
  FROM visible v JOIN agents a ON (a.environment_id,a.id)=($1,v.agent_id)
  CROSS JOIN LATERAL (SELECT min(created_at) AS at FROM session_holds WHERE environment_id=$1 AND released_at IS NULL AND (session_id=v.id OR (scope='subtree' AND session_id=v.parent_session_id))) h
  CROSS JOIN LATERAL (SELECT min(started_at) AS at FROM turns WHERE environment_id=$1 AND session_id=v.id AND status='running') r
  CROSS JOIN LATERAL (SELECT min(created_at) AS at FROM turns WHERE environment_id=$1 AND session_id=v.id AND status='queued') q
  WHERE COALESCE(h.at,r.at,q.at) IS NOT NULL
 ) `

const overviewActiveSQL = overviewActiveCTE + `SELECT id,agent,state,since,origin,
 (SELECT c.slack_channel_id FROM slack_threads st JOIN slack_channel_bindings c ON c.id=st.slack_channel_binding_id WHERE st.environment_id=$1 AND st.front_session_id=active.id)
 FROM active ORDER BY CASE state WHEN 'held' THEN 0 WHEN 'running' THEN 1 ELSE 2 END,since DESC,id LIMIT $9::int`

const overviewActiveCountsSQL = overviewActiveCTE + `SELECT agent,count(*) FILTER (WHERE state='running'),count(*) FILTER (WHERE state='queued'),count(*) FILTER (WHERE state='held') FROM active GROUP BY agent`

const overviewFailuresSQL = `SELECT a.key,COALESCE(t.error_code,''),count(*),count(*) FILTER (WHERE t.terminal_at>=now()-interval '1 hour'),min(t.terminal_at),max(t.terminal_at),
 (array_agg(t.session_id ORDER BY t.terminal_at DESC,t.id DESC))[1],(array_agg(t.id ORDER BY t.terminal_at DESC,t.id DESC))[1],(array_agg(t.seq ORDER BY t.terminal_at DESC,t.id DESC))[1]
 FROM turns t JOIN (SELECT id,agent_id FROM sessions WHERE ` + overviewReadable + `) v ON v.id=t.session_id JOIN agents a ON (a.environment_id,a.id)=(t.environment_id,v.agent_id)
 WHERE t.environment_id=$1 AND t.status='failed' AND t.created_at>=` + overviewWindow + `
 GROUP BY 1,2 ORDER BY 4 DESC,3 DESC,6 DESC LIMIT $9::int`

// ReadOverview summarizes the Environment with the same authority as listing
// its Sessions, in one snapshot.
func ReadOverview(ctx context.Context, pool db.TxBeginner, caller Caller, env uuid.UUID) (Overview, error) {
	result := Overview{Agents: []OverviewAgent{}, Active: []OverviewSession{}, Failures: []OverviewFailure{}}
	err := readTransaction(ctx, pool, caller, env, func(tx pgx.Tx) error {
		filter, err := readScopeFor(ctx, tx, caller, env)
		if err != nil {
			return err
		}
		if !filter.All && !filter.AllAgents && len(filter.Agents) == 0 && len(filter.ViewAgents) == 0 {
			return ErrDenied
		}
		args := func(extra ...any) []any {
			return append([]any{env, filter.All, filter.User, filter.Key, filter.AllAgents, filter.Agents, filter.ViewAgents, nullableID(filter.SharedConnection)}, extra...)
		}
		if err := tx.QueryRow(ctx, overviewTotalsSQL, args()...).Scan(&result.Turns, &result.Failed, &result.PreviousTurns, &result.PreviousFailed); err != nil {
			return err
		}
		if result.Agents, err = overviewAgents(ctx, tx, args()); err != nil {
			return err
		}
		if err := overviewActiveCounts(ctx, tx, args(), &result); err != nil {
			return err
		}
		if result.Active, err = overviewActive(ctx, tx, args(overviewActiveLimit)); err != nil {
			return err
		}
		if result.Failures, err = overviewFailures(ctx, tx, args(overviewFailureLimit)); err != nil {
			return err
		}
		return err
	})
	return result, err
}

func overviewAgents(ctx context.Context, tx pgx.Tx, args []any) ([]OverviewAgent, error) {
	agents := []OverviewAgent{}
	index := map[string]int{}
	rows, err := tx.Query(ctx, overviewAgentsSQL, args...)
	if err != nil {
		return agents, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var age int
		var total, failed int64
		if err := rows.Scan(&key, &age, &total, &failed); err != nil {
			return agents, err
		}
		agent := &agents[agentIndex(&agents, index, key)]
		bucket := OverviewHours - 1 - age
		agent.Hourly[bucket] += total
		agent.HourlyFailed[bucket] += failed
		agent.Turns += total
		agent.Failed += failed
	}
	return agents, rows.Err()
}

func agentIndex(agents *[]OverviewAgent, index map[string]int, key string) int {
	if i, ok := index[key]; ok {
		return i
	}
	*agents = append(*agents, OverviewAgent{Agent: key})
	index[key] = len(*agents) - 1
	return index[key]
}

// overviewActiveCounts counts every Session with work in progress by state and
// by Agent; an Agent with work but no Turns in the window still appears.
func overviewActiveCounts(ctx context.Context, tx pgx.Tx, args []any, result *Overview) error {
	index := map[string]int{}
	for i, agent := range result.Agents {
		index[agent.Agent] = i
	}
	rows, err := tx.Query(ctx, overviewActiveCountsSQL, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var running, queued, held int64
		if err := rows.Scan(&key, &running, &queued, &held); err != nil {
			return err
		}
		result.Running += running
		result.Queued += queued
		result.Held += held
		result.Agents[agentIndex(&result.Agents, index, key)].Active += running + queued + held
	}
	result.ActiveCount = result.Running + result.Queued + result.Held
	return rows.Err()
}

func overviewActive(ctx context.Context, tx pgx.Tx, args []any) ([]OverviewSession, error) {
	sessions := []OverviewSession{}
	rows, err := tx.Query(ctx, overviewActiveSQL, args...)
	if err != nil {
		return sessions, err
	}
	defer rows.Close()
	for rows.Next() {
		var s OverviewSession
		if err := rows.Scan(&s.ID, &s.Agent, &s.State, &s.Since, &s.Origin, &s.SlackChannelID); err != nil {
			return sessions, err
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

func overviewFailures(ctx context.Context, tx pgx.Tx, args []any) ([]OverviewFailure, error) {
	failures := []OverviewFailure{}
	rows, err := tx.Query(ctx, overviewFailuresSQL, args...)
	if err != nil {
		return failures, err
	}
	defer rows.Close()
	for rows.Next() {
		var f OverviewFailure
		if err := rows.Scan(&f.Agent, &f.ErrorCode, &f.Count, &f.LastHour, &f.FirstAt, &f.LastAt, &f.LastSessionID, &f.LastTurnID, &f.LastTurnSequence); err != nil {
			return failures, err
		}
		failures = append(failures, f)
	}
	return failures, rows.Err()
}
