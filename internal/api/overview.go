package api

import "time"

// Overview summarizes the Sessions the caller can read in one Environment.
// Turn counts cover Turns admitted in the 24 hours before the read; Previous
// covers the 24 hours before that.
type Overview struct {
	Turns      OverviewTurns       `json:"turns"`
	Previous   OverviewTurns       `json:"previous"`
	Agents     []OverviewAgent     `json:"agents"`
	Active     OverviewActive      `json:"active"`
	Failures   []OverviewFailure   `json:"failures"`
	Deployment *OverviewDeployment `json:"deployment"`
}

type OverviewTurns struct {
	Total  int64 `json:"total"`
	Failed int64 `json:"failed"`
}

// OverviewAgent is one Agent's activity. Hourly and HourlyFailed hold 24
// one-hour buckets counted back from the read, oldest first.
type OverviewAgent struct {
	Agent        string     `json:"agent"`
	Active       int64      `json:"active"`
	Turns        int64      `json:"turns"`
	Failed       int64      `json:"failed"`
	LastTurnAt   *time.Time `json:"last_turn_at"`
	Hourly       []int64    `json:"hourly"`
	HourlyFailed []int64    `json:"hourly_failed"`
}

// OverviewActive counts every Session with work in progress, by state, and
// lists the first few, held first.
type OverviewActive struct {
	Count    int64             `json:"count"`
	Running  int64             `json:"running"`
	Queued   int64             `json:"queued"`
	Held     int64             `json:"held"`
	Sessions []OverviewSession `json:"sessions"`
}

// OverviewSession is a Session that is held, running a Turn or has Turns
// queued. Since is when that state began. Origin is slack, user, api_key,
// trigger or session.
type OverviewSession struct {
	ID             string    `json:"id"`
	Agent          string    `json:"agent"`
	State          string    `json:"state"`
	Since          time.Time `json:"since"`
	Origin         string    `json:"origin"`
	SlackChannelID *string   `json:"slack_channel_id,omitempty"`
}

// OverviewFailure groups the window's failed Turns of one Agent with one error
// code; LastHour counts those in the last hour.
type OverviewFailure struct {
	Agent            string    `json:"agent"`
	ErrorCode        string    `json:"error_code"`
	Count            int64     `json:"count"`
	LastHour         int64     `json:"last_hour"`
	FirstAt          time.Time `json:"first_at"`
	LastAt           time.Time `json:"last_at"`
	LastSessionID    string    `json:"last_session_id"`
	LastTurnID       string    `json:"last_turn_id"`
	LastTurnSequence int64     `json:"last_turn_sequence"`
}

// OverviewDeployment counts the window's Turns of Sessions started on the
// current deployment and the open Sessions still on older ones. It is present
// only for callers who can read deployments.
type OverviewDeployment struct {
	ID                string        `json:"id"`
	Turns             OverviewTurns `json:"turns"`
	OlderOpenSessions int64         `json:"older_open_sessions"`
}
