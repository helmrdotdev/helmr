package pgvalue

import (
	"testing"
	"uuid"

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

func TestUUID(t *testing.T) {
	id := uuid.MustParse("018f47b4-9a54-7cc2-b8e5-7d3291f04973")

	pgID := UUID(id)
	if !pgID.Valid {
		t.Fatal("UUID returned invalid pgtype.UUID")
	}
	if got := MustUUIDValue(pgID); got != id {
		t.Fatalf("MustUUIDValue = %s, want %s", got, id)
	}
	if got := UUIDString(pgID); got != id.String() {
		t.Fatalf("UUIDString = %q, want %q", got, id.String())
	}
	if _, err := UUIDValue(pgtype.UUID{}); err == nil {
		t.Fatal("UUIDValue invalid uuid error = nil")
	}
	if got := UUIDString(pgtype.UUID{}); got != "" {
		t.Fatalf("UUIDString invalid = %q, want empty", got)
	}
}
