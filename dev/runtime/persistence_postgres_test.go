package verification

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
)

// This checks the operator observation against the real current schema and psql
// variable syntax. It does not replace the real checkpoint/VM execution case.
func TestPersistenceObservationQuery(t *testing.T) {
	database := dbtest.Open(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := schema.Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	query, err := os.ReadFile("persistence.sql")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, "psql", database.DSN, "-X", "-v", "ON_ERROR_STOP=1", "-v", "run_id=01900000-0000-7000-8000-000000000001", "-At")
	command.Env = append(os.Environ(), "PGOPTIONS=-c default_transaction_read_only=on -c statement_timeout=5000")
	command.Stdin = strings.NewReader(string(query))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("observe persistence: %v\n%s", err, output)
	}
	if strings.TrimSpace(string(output)) != "" {
		t.Fatalf("absent Run unexpectedly produced evidence: %s", output)
	}
}
