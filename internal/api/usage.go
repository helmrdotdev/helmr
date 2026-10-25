package api

// Usage says how much the organization uses an Environment's agents over a
// window of Days, from the Sessions the caller can read. Daily slices hold one
// value per day counted back from the read, oldest first.
type Usage struct {
	Days   int          `json:"days"`
	Work   UsageWork    `json:"work"`
	People UsagePeople  `json:"people"`
	Runs   UsageRuns    `json:"runs"`
	Agents []UsageAgent `json:"agents"`
}

// UsageWork is the time agents spent on Turns, in seconds, in the window and
// the window before.
type UsageWork struct {
	Seconds         float64   `json:"seconds"`
	PreviousSeconds float64   `json:"previous_seconds"`
	Turns           int64     `json:"turns"`
	PreviousTurns   int64     `json:"previous_turns"`
	Daily           []float64 `json:"daily"`
}

// UsagePeople counts active members who started Turns in the last seven days,
// each once: Slack if they used it at all, otherwise App (CLI, Console or
// API). Members is the active headcount, present only for callers who can read
// members. Daily holds the seven-day count ending with each day.
type UsagePeople struct {
	Weekly   int64          `json:"weekly"`
	Members  *int64         `json:"members"`
	Slack    int64          `json:"slack"`
	App      int64          `json:"app"`
	Daily    []int64        `json:"daily"`
	Channels []UsageChannel `json:"channels"`
}

// UsageChannel is a Slack conversation people started work from in the
// window. Name is absent for a direct message or a channel not verified since
// names were kept.
type UsageChannel struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	People int64  `json:"people"`
}

// UsageRuns describes how long Turns in the window ran: counts below each of
// Bounds (seconds) and above the last, the median, and how many completed
// Turns finished without asking a person a question.
type UsageRuns struct {
	Bounds        []int64   `json:"bounds"`
	Buckets       []int64   `json:"buckets"`
	MedianSeconds float64   `json:"median_seconds"`
	Completed     int64     `json:"completed"`
	Unassisted    int64     `json:"unassisted"`
	Longest       *UsageRun `json:"longest"`
}

type UsageRun struct {
	Agent     string  `json:"agent"`
	Seconds   float64 `json:"seconds"`
	SessionID string  `json:"session_id"`
	TurnID    string  `json:"turn_id"`
}

type UsageAgent struct {
	Agent   string  `json:"agent"`
	Seconds float64 `json:"seconds"`
	People  int64   `json:"people"`
}
