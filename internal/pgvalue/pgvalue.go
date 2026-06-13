// Package pgvalue contains small constructors and accessors for pgtype values.
package pgvalue

import (
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func Text(value string) pgtype.Text {
	value = strings.TrimSpace(value)
	if value == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: value, Valid: true}
}

func TextPtr(value *string) pgtype.Text {
	if value == nil {
		return pgtype.Text{}
	}
	return Text(*value)
}

func TextValue(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func Time(value pgtype.Timestamptz) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return value.Time
}

func TimePtr(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	valueTime := value.Time
	return &valueTime
}

func Timestamptz(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}

func TimestamptzUTCZeroInvalid(value time.Time) pgtype.Timestamptz {
	if value.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: value.UTC(), Valid: true}
}

func Interval(duration time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: duration.Microseconds(), Valid: true}
}

func Int4Ptr(value *int32) pgtype.Int4 {
	if value == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: *value, Valid: true}
}

func Int4Response(value pgtype.Int4) *int32 {
	if !value.Valid {
		return nil
	}
	return &value.Int32
}
