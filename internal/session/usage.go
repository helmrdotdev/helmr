package session

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// UsageRanges are the windows, in days, a usage read accepts.
var UsageRanges = map[int]bool{7: true, 30: true, 90: true}

// usageChannelLimit bounds the channels one usage read names.
const usageChannelLimit = 5

// UsageRunBounds are the upper bounds, in seconds, of the turn length buckets;
// the last bucket holds everything longer.
var UsageRunBounds = [4]int64{60, 600, 3600, 14400}

// Usage says how much an organization uses the agents of one Environment over
// a window of days, from the Sessions the caller can read: time agents spent
// on Turns, the people who started them, and how long Turns ran on their own.
// Each daily slice is one day counted back from the read, oldest first.
type Usage struct {
	Days int

	WorkSeconds, PreviousWorkSeconds float64
	Turns, PreviousTurns             int64
	DailyWorkSeconds                 []float64

	WeeklyPeople int64
	// Members is the organization's active headcount, present only for callers
	// who can read members.
	Members                *int64
	SlackPeople, AppPeople int64
	DailyWeeklyPeople      []int64
	Channels               []UsageChannel

	RunBuckets            [len(UsageRunBounds) + 1]int64
	MedianRunSeconds      float64
	Completed, Unassisted int64
	Longest               *UsageRun

	Agents []UsageAgent
}

// UsageChannel is a Slack conversation people started work from. Name is empty
// for a direct message or a channel Helmr has not verified since names were
// kept.
type UsageChannel struct {
	ChannelID string
	Name      string
	People    int64
}

// UsageRun names the longest Turn in the window.
type UsageRun struct {
	Agent     string
	Seconds   float64
	SessionID uuid.UUID
	TurnID    uuid.UUID
}

// UsageAgent is one Agent's share of the window.
type UsageAgent struct {
	Agent       string
	WorkSeconds float64
	People      int64
}

// usageTurns reads the caller's Turns admitted within `span` of the read, plus
// any still running, as `turns_in`. The window is $9 days back from the read;
// the previous window is the same length before it. A Turn belongs to the
// window it was admitted in for counts and run lengths (`current`, `seconds`);
// its work is the part of its run inside each window (`work`, `previous_work`),
// so a long run is split across them and a Turn still running from earlier
// counts too. A running Turn runs up to the read. Each query reads only the
// span it needs.
func usageTurns(span string) string {
	return `WITH visible AS (SELECT id,agent_id,origin_slack_connection_id FROM sessions WHERE ` + overviewReadable + `),
 turns_raw AS (
  SELECT t.id,t.session_id,t.status,t.caller_kind,t.caller_id,t.created_at,v.agent_id,v.origin_slack_connection_id,
   t.started_at,COALESCE(t.terminal_at,now()) AS ended
  FROM turns t JOIN visible v ON v.id=t.session_id
  WHERE t.environment_id=$1 AND (t.created_at>=now()-` + span + ` OR t.status='running')
 ), turns_in AS MATERIALIZED (
  SELECT r.*,
   CASE WHEN started_at IS NULL THEN 0 ELSE extract(epoch FROM ended-started_at) END AS seconds,
   created_at>=now()-make_interval(days => $9::int) AS current,
   CASE WHEN started_at IS NULL THEN 0 ELSE GREATEST(0,extract(epoch FROM LEAST(ended,now())-GREATEST(started_at,now()-make_interval(days => $9::int)))) END AS work,
   CASE WHEN started_at IS NULL THEN 0 ELSE GREATEST(0,extract(epoch FROM LEAST(ended,now()-make_interval(days => $9::int))-GREATEST(started_at,now()-make_interval(days => 2*$9::int)))) END AS previous_work
  FROM turns_raw r
 ) `
}

const (
	usageWindow   = `make_interval(days => $9::int)`
	usageTwoSpans = `make_interval(days => 2*$9::int)`
	usageWeekSpan = `make_interval(days => $9::int+7)`
)

var usageWorkSQL = usageTurns(usageTwoSpans) + `SELECT COALESCE(sum(work),0),COALESCE(sum(previous_work),0),
 count(*) FILTER (WHERE current),count(*) FILTER (WHERE NOT current AND created_at>=now()-` + usageTwoSpans + `) FROM turns_in`

// Each day takes the part of every run that overlaps it, including runs
// admitted before the window.
var usageDailySQL = usageTurns(usageTwoSpans) + `SELECT d,sum(GREATEST(0,extract(epoch FROM LEAST(t.ended,now()-make_interval(days => d))-GREATEST(t.started_at,now()-make_interval(days => d+1)))))
 FROM generate_series(0,$9::int-1) d JOIN turns_in t ON t.started_at IS NOT NULL AND t.started_at<now()-make_interval(days => d) AND t.ended>now()-make_interval(days => d+1)
 GROUP BY d`

// People are active members who started Turns themselves; API keys, other
// Sessions and removed members are not counted. A person who used Slack at
// all in the week counts as a Slack person, otherwise as using the CLI,
// Console or API.
const usagePeople = `, people AS (
  SELECT t.caller_id,bool_or(t.origin_slack_connection_id IS NOT NULL) AS slack,floor(extract(epoch FROM now()-t.created_at)/86400)::int AS age
  FROM turns_in t JOIN environments e ON e.id=$1 JOIN org_members m ON m.org_id=e.org_id AND m.user_id=t.caller_id AND m.removed_at IS NULL
  WHERE t.caller_kind='user' GROUP BY t.caller_id,age
 ) `

var usagePeopleSQL = usageTurns(usageWindow) + usagePeople + `, week AS (SELECT caller_id,bool_or(slack) AS slack FROM people WHERE age<7 GROUP BY caller_id)
 SELECT count(*),count(*) FILTER (WHERE slack),count(*) FILTER (WHERE NOT slack),
 (SELECT count(*) FROM org_members m JOIN environments e ON e.org_id=m.org_id WHERE e.id=$1 AND m.removed_at IS NULL) FROM week`

// Each day counts the distinct people of the seven days ending with it, from
// one pass over the people active on each day.
var usageDailyPeopleSQL = usageTurns(usageWeekSpan) + usagePeople + `SELECT d,count(DISTINCT p.caller_id) FROM generate_series(0,$9::int-1) d JOIN people p ON p.age BETWEEN d AND d+6 GROUP BY d`

// Conversations people started work from in the last seven days, matching the
// weekly people count above them. Direct messages count as one, "D".
var usageChannelsSQL = usageTurns(usageWindow) + `SELECT CASE WHEN b.slack_channel_id LIKE 'D%' THEN 'D' ELSE b.slack_channel_id END,CASE WHEN b.slack_channel_id LIKE 'D%' THEN '' ELSE COALESCE(n.name,'') END,count(DISTINCT t.caller_id)
 FROM turns_in t JOIN slack_threads st ON st.environment_id=$1 AND st.front_session_id=t.session_id
 JOIN slack_channel_bindings b ON b.id=st.slack_channel_binding_id
 LEFT JOIN slack_channels n ON n.installation_id=b.installation_id AND n.slack_channel_id=b.slack_channel_id
 WHERE t.created_at>=now()-interval '7 days' AND t.caller_kind='user'
 GROUP BY 1,2 ORDER BY 3 DESC,1 LIMIT $10::int`

var usageRunsSQL = usageTurns(usageWindow) + `SELECT count(*) FILTER (WHERE seconds<$10),count(*) FILTER (WHERE seconds>=$10 AND seconds<$11),count(*) FILTER (WHERE seconds>=$11 AND seconds<$12),
 count(*) FILTER (WHERE seconds>=$12 AND seconds<$13),count(*) FILTER (WHERE seconds>=$13),
 COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY seconds),0),
 count(*) FILTER (WHERE status='completed'),
 count(*) FILTER (WHERE status='completed' AND NOT EXISTS(SELECT 1 FROM turn_asks a WHERE a.environment_id=$1 AND a.session_id=turns_in.session_id AND a.turn_id=turns_in.id))
 FROM turns_in WHERE current AND started_at IS NOT NULL`

var usageLongestSQL = usageTurns(usageWindow) + `SELECT a.key,t.seconds,t.session_id,t.id FROM turns_in t JOIN agents a ON (a.environment_id,a.id)=($1,t.agent_id)
 WHERE t.current AND t.started_at IS NOT NULL ORDER BY t.seconds DESC,t.id LIMIT 1`

var usageAgentsSQL = usageTurns(usageTwoSpans) + `SELECT a.key,sum(t.work),count(DISTINCT t.caller_id) FILTER (WHERE t.caller_kind='user' AND t.current)
 FROM turns_in t JOIN agents a ON (a.environment_id,a.id)=($1,t.agent_id) WHERE t.current OR t.work>0 GROUP BY 1 ORDER BY 2 DESC,1`

// ReadUsage summarizes the window with the same authority as listing the
// Environment's Sessions, in one snapshot. Days must be one of UsageRanges.
func ReadUsage(ctx context.Context, pool db.TxBeginner, caller Caller, env uuid.UUID, days int) (Usage, error) {
	result := Usage{Days: days, Channels: []UsageChannel{}, Agents: []UsageAgent{}}
	if !UsageRanges[days] {
		return result, ErrInvalidInput
	}
	result.DailyWorkSeconds, result.DailyWeeklyPeople = make([]float64, days), make([]int64, days)
	err := readTransaction(ctx, pool, caller, env, func(tx pgx.Tx) error {
		filter, err := readScopeFor(ctx, tx, caller, env)
		if err != nil {
			return err
		}
		if !filter.All && !filter.AllAgents && len(filter.Agents) == 0 && len(filter.ViewAgents) == 0 {
			return ErrDenied
		}
		args := func(extra ...any) []any {
			return append([]any{env, filter.All, filter.User, filter.Key, filter.AllAgents, filter.Agents, filter.ViewAgents, nullableID(filter.SharedConnection), days}, extra...)
		}
		if err := tx.QueryRow(ctx, usageWorkSQL, args()...).Scan(&result.WorkSeconds, &result.PreviousWorkSeconds, &result.Turns, &result.PreviousTurns); err != nil {
			return err
		}
		if err := usageEach(ctx, tx, usageDailySQL, args(), func(row pgx.Rows) error {
			var age int
			var seconds float64
			if err := row.Scan(&age, &seconds); err != nil {
				return err
			}
			result.DailyWorkSeconds[days-1-age] += seconds
			return nil
		}); err != nil {
			return err
		}
		var members int64
		if err := tx.QueryRow(ctx, usagePeopleSQL, args()...).Scan(&result.WeeklyPeople, &result.SlackPeople, &result.AppPeople, &members); err != nil {
			return err
		}
		if filter.MembersRead {
			result.Members = &members
		}
		if err := usageEach(ctx, tx, usageDailyPeopleSQL, args(), func(row pgx.Rows) error {
			var age int
			var people int64
			if err := row.Scan(&age, &people); err != nil {
				return err
			}
			result.DailyWeeklyPeople[days-1-age] = people
			return nil
		}); err != nil {
			return err
		}
		if err := usageEach(ctx, tx, usageChannelsSQL, args(usageChannelLimit), func(row pgx.Rows) error {
			var c UsageChannel
			if err := row.Scan(&c.ChannelID, &c.Name, &c.People); err != nil {
				return err
			}
			result.Channels = append(result.Channels, c)
			return nil
		}); err != nil {
			return err
		}
		runArgs := args(UsageRunBounds[0], UsageRunBounds[1], UsageRunBounds[2], UsageRunBounds[3])
		b := &result.RunBuckets
		if err := tx.QueryRow(ctx, usageRunsSQL, runArgs...).Scan(&b[0], &b[1], &b[2], &b[3], &b[4], &result.MedianRunSeconds, &result.Completed, &result.Unassisted); err != nil {
			return err
		}
		var longest UsageRun
		switch err := tx.QueryRow(ctx, usageLongestSQL, args()...).Scan(&longest.Agent, &longest.Seconds, &longest.SessionID, &longest.TurnID); err {
		case nil:
			result.Longest = &longest
		case pgx.ErrNoRows:
		default:
			return err
		}
		return usageEach(ctx, tx, usageAgentsSQL, args(), func(row pgx.Rows) error {
			var a UsageAgent
			if err := row.Scan(&a.Agent, &a.WorkSeconds, &a.People); err != nil {
				return err
			}
			result.Agents = append(result.Agents, a)
			return nil
		})
	})
	return result, err
}

func usageEach(ctx context.Context, tx pgx.Tx, sql string, args []any, scan func(pgx.Rows) error) error {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
