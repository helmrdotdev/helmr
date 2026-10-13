package api

import (
	"errors"
	"math"
	"regexp"
	"strconv"
)

var historyRetentionPattern = regexp.MustCompile(`^([1-9][0-9]*)(s|m|h|d)$`)

// ParseHistoryRetention accepts whole seconds, minutes, hours, days, or never.
// A nil result disables automatic expiry. Durations cannot overflow bigint seconds.
func ParseHistoryRetention(value string) (*int64, error) {
	if value == "never" {
		return nil, nil
	}
	match := historyRetentionPattern.FindStringSubmatch(value)
	if match != nil {
		n, err := strconv.ParseInt(match[1], 10, 64)
		unit := map[string]int64{"s": 1, "m": 60, "h": 3600, "d": 86400}[match[2]]
		if err == nil && n <= math.MaxInt64/unit {
			seconds := n * unit
			return &seconds, nil
		}
	}
	return nil, errors.New("history_retention must be a positive integer with s, m, h, or d (for example 30d), or never")
}

// FormatHistoryRetention uses the largest exact unit, preserving every second.
func FormatHistoryRetention(seconds *int64) string {
	if seconds == nil {
		return "never"
	}
	for _, unit := range []struct {
		suffix  string
		seconds int64
	}{{"d", 86400}, {"h", 3600}, {"m", 60}, {"s", 1}} {
		if *seconds%unit.seconds == 0 {
			return strconv.FormatInt(*seconds/unit.seconds, 10) + unit.suffix
		}
	}
	panic("unreachable retention duration")
}
