package pgvalue

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestText(t *testing.T) {
	if got := Text("  hello  "); !got.Valid || got.String != "hello" {
		t.Fatalf("Text() = %#v", got)
	}
	if got := Text("   "); got.Valid {
		t.Fatalf("Text(blank).Valid = true")
	}
}

func TestTextPtrNormalizesLikeText(t *testing.T) {
	value := "  hello  "
	if got := TextPtr(&value); !got.Valid || got.String != "hello" {
		t.Fatalf("TextPtr() = %#v", got)
	}
	blank := "  "
	if got := TextPtr(&blank); got.Valid {
		t.Fatalf("TextPtr(blank).Valid = true")
	}
}

func TestTimestamptzUTCZeroInvalid(t *testing.T) {
	if got := TimestamptzUTCZeroInvalid(time.Time{}); got.Valid {
		t.Fatalf("zero time Valid = true")
	}
	loc := time.FixedZone("test", 9*60*60)
	value := time.Date(2026, 6, 13, 12, 0, 0, 0, loc)
	got := TimestamptzUTCZeroInvalid(value)
	if !got.Valid || got.Time.Location() != time.UTC || !got.Time.Equal(value) {
		t.Fatalf("TimestamptzUTCZeroInvalid() = %#v", got)
	}
}

func TestInt4Response(t *testing.T) {
	if got := Int4Response(pgtype.Int4{}); got != nil {
		t.Fatalf("Int4Response(invalid) = %v", *got)
	}
	value := Int4Response(pgtype.Int4{Int32: 7, Valid: true})
	if value == nil || *value != 7 {
		t.Fatalf("Int4Response(valid) = %v", value)
	}
}
