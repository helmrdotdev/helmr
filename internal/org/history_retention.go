package org

import "github.com/jackc/pgx/v5/pgtype"

// HistoryRetentionPolicy is explicitly selected for each new Environment.
// Sessions snapshot it at admission; later edits never shorten their policy.
type HistoryRetentionPolicy struct {
	Mode    string
	Seconds *int64
}

func (p HistoryRetentionPolicy) Validate() error {
	if p.Mode == "until_environment_deletion" && p.Seconds == nil {
		return nil
	}
	if p.Mode == "duration" && p.Seconds != nil && *p.Seconds > 0 {
		return nil
	}
	return invalidInput("history_retention_mode must be duration with positive history_retention_seconds, or until_environment_deletion without seconds")
}

func (p HistoryRetentionPolicy) seconds() pgtype.Int8 {
	if p.Seconds == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *p.Seconds, Valid: true}
}
