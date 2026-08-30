package ids

import (
	"errors"
	"strings"
	"testing"
	"uuid"
)

func TestParse(t *testing.T) {
	valid := uuid.NewV7().String()
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

func TestParseRejectsWrongVersionAndVariant(t *testing.T) {
	for name, mutate := range map[string]func(*uuid.UUID){
		"version": func(id *uuid.UUID) { id[6] = id[6]&0x0f | 0x40 },
		"variant": func(id *uuid.UUID) { id[8] &= 0x3f },
	} {
		t.Run(name, func(t *testing.T) {
			id := uuid.NewV7()
			mutate(&id)
			if _, err := Parse(id.String()); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Parse(%q) error = %v", id, err)
			}
		})
	}
}
