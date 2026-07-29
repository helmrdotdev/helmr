package ids

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestParse(t *testing.T) {
	valid := uuid.Must(uuid.NewV7()).String()
	if got, err := Parse(valid); err != nil || got.String() != valid {
		t.Fatalf("Parse(%q) = %v, %v", valid, got, err)
	}

	for _, value := range []string{
		"",
		uuid.New().String(),
		strings.ToUpper(valid),
		" " + valid,
		valid + " ",
		strings.ReplaceAll(valid, "-", ""),
		"run_" + valid,
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := Parse(value); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Parse(%q) error = %v", value, err)
			}
		})
	}
}
