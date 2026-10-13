package sqlpolicy

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
)

// TestStatementsPrepareAgainstSchema parses every constant statement a
// production package sends to PostgreSQL against the fresh schema, so a
// renamed column or table fails here instead of at runtime.
func TestStatementsPrepareAgainstSchema(t *testing.T) {
	statements := collectStatements(t)
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	conn, err := database.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	prepared := 0
	for i, s := range statements {
		if s.sql == "" {
			continue
		}
		pieces := splitStatements(s.sql)
		for j, piece := range pieces {
			if len(pieces) > 1 {
				piece = renumberParameters(piece)
			}
			if _, err := conn.Conn().Prepare(t.Context(), fmt.Sprintf("policy_%d_%d", i, j), piece); err != nil {
				t.Errorf("%s (%s): %v", s.position, s.owner, err)
				continue
			}
			prepared++
		}
	}
	if prepared == 0 {
		t.Fatal("no statements were prepared")
	}
	t.Logf("prepared %d statements", prepared)
}

var parameterReference = regexp.MustCompile(`\$(\d+)`)

// renumberParameters makes one statement of a simple-protocol batch
// preparable on its own: the batch shares one parameter list, so a statement
// using $2 and $9 becomes $1 and $2. Explicit casts in the batch keep the types.
func renumberParameters(piece string) string {
	next := 1
	numbers := map[string]string{}
	return parameterReference.ReplaceAllStringFunc(piece, func(match string) string {
		if replacement, ok := numbers[match]; ok {
			return replacement
		}
		numbers[match] = fmt.Sprintf("$%d", next)
		next++
		return numbers[match]
	})
}
