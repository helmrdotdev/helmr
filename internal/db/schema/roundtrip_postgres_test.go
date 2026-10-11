package schema

import (
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"testing"
)

func TestSchemaCleanRoundTrip(t *testing.T) {
	database := dbtest.Open(t)
	for range 2 {
		if err := Up(t.Context(), database.DSN); err != nil {
			t.Fatal(err)
		}
		var exists bool
		if err := database.Pool.QueryRow(t.Context(), `SELECT to_regclass('public.agent_schedule_occurrences') IS NOT NULL AND to_regclass('public.control_outbox') IS NOT NULL`).Scan(&exists); err != nil || !exists {
			t.Fatalf("owners missing: %v %v", exists, err)
		}
		if err := Down(t.Context(), database.DSN); err != nil {
			t.Fatal(err)
		}
		var tables int
		if err := database.Pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_tables WHERE schemaname='public' AND tablename<>'schema_migrations'`).Scan(&tables); err != nil || tables != 0 {
			t.Fatalf("remaining tables=%d: %v", tables, err)
		}
	}
}
