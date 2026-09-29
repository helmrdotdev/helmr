package deployment

import (
	"testing"
	"uuid"
)

func TestVersionUsesCreationDateAndID(t *testing.T) {
	id := uuid.MustParse("019b76da-a800-7000-8000-000000000000")

	if got, want := version(id), "20260101."+id.String(); got != want {
		t.Fatalf("version() = %q, want %q", got, want)
	}
}
