package audit_test

import (
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAuditDatabaseRolesAndAtomicMaintenance(t *testing.T) {
	d := dbtest.Open(t)
	if err := schema.Up(t.Context(), d.DSN); err != nil {
		t.Fatal(err)
	}
	// Runtime and maintenance connect as actual roles, not as the migration owner.
	suffix := uuid.NewV7().String()
	runtimeRole, maintenanceRole := "runtime_"+suffix, "maintenance_"+suffix
	for _, role := range []string{runtimeRole, maintenanceRole} {
		dbtest.MustExec(t, t.Context(), d.Pool, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN")
	}
	t.Cleanup(func() {
		for _, role := range []string{runtimeRole, maintenanceRole} {
			_, _ = d.Pool.Exec(t.Context(), "DROP OWNED BY "+pgx.Identifier{role}.Sanitize())
			_, _ = d.Pool.Exec(t.Context(), "DROP ROLE "+pgx.Identifier{role}.Sanitize())
		}
	})
	dbtest.MustExec(t, t.Context(), d.Pool, "REVOKE CREATE ON SCHEMA public FROM PUBLIC")
	dbtest.MustExec(t, t.Context(), d.Pool, "GRANT USAGE ON SCHEMA public TO "+pgx.Identifier{runtimeRole}.Sanitize()+", "+pgx.Identifier{maintenanceRole}.Sanitize())
	if err := schema.ConfigurePrivileges(t.Context(), d.DSN, runtimeRole, maintenanceRole); err != nil {
		t.Fatal(err)
	}
	connect := func(role string) *pgxpool.Pool {
		t.Helper()
		cfg, err := pgxpool.ParseConfig(d.DSN)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.User = role
		p, err := pgxpool.NewWithConfig(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	}
	runtime, maintenance := connect(runtimeRole), connect(maintenanceRole)
	if _, err := runtime.Exec(t.Context(), "SELECT * FROM allocation_usage"); err != nil {
		t.Fatalf("runtime cannot read physical occupancy: %v", err)
	}
	if _, err := maintenance.Exec(t.Context(), "SELECT * FROM allocation_usage"); err == nil {
		t.Fatal("audit maintenance can read physical occupancy")
	}
	// The view uses the caller's underlying-table privileges, not its owner's.
	dbtest.MustExec(t, t.Context(), d.Pool, "REVOKE SELECT ON computer_leases FROM "+pgx.Identifier{runtimeRole}.Sanitize())
	if _, err := runtime.Exec(t.Context(), "SELECT * FROM allocation_usage"); err == nil {
		t.Fatal("occupancy view bypassed the caller's table privileges")
	}
	dbtest.MustExec(t, t.Context(), d.Pool, "GRANT SELECT ON computer_leases TO "+pgx.Identifier{runtimeRole}.Sanitize())
	if err := audit.ValidatePrivileges(t.Context(), runtime, false); err != nil {
		t.Fatal(err)
	}
	if err := audit.ValidatePrivileges(t.Context(), maintenance, true); err != nil {
		t.Fatal(err)
	}
	if err := audit.ValidatePrivileges(t.Context(), d.Pool, false); err == nil {
		t.Fatal("owner accepted as runtime")
	}
	if err := audit.ValidatePrivileges(t.Context(), maintenance, false); err == nil {
		t.Fatal("maintenance accepted as runtime")
	}
	for _, statement := range []string{"UPDATE audit_events SET outcome='failed'", "DELETE FROM audit_events", "TRUNCATE audit_events", "ALTER TABLE audit_events ADD COLUMN tampered text", "UPDATE schema_migrations SET dirty=true", "CREATE TABLE public.forbidden(id integer)"} {
		if _, err := runtime.Exec(t.Context(), statement); err == nil {
			t.Fatalf("runtime executed %s", statement)
		}
	}
	for _, statement := range []string{"UPDATE audit_events SET outcome='failed'", "TRUNCATE audit_events", "ALTER TABLE audit_events ADD COLUMN tampered text"} {
		if _, err := maintenance.Exec(t.Context(), statement); err == nil {
			t.Fatalf("maintenance executed %s", statement)
		}
	}
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	if err := audit.ObservePolicy(t.Context(), runtime, policy, audit.ControlPlane); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), d.Pool, `UPDATE audit_events SET occurred_at=statement_timestamp()-interval '2 hours', recorded_at=statement_timestamp()-interval '2 hours', expires_at=statement_timestamp()-interval '1 hour'`)
	// Force receipt failure: deletion must roll back with it.
	dbtest.MustExec(t, t.Context(), d.Pool, "ALTER TABLE audit_events ADD CONSTRAINT reject_purge CHECK(action<>'audit.purge_completed')")
	if _, err := audit.Purge(t.Context(), maintenance, policy, 1); err == nil {
		t.Fatal("purge succeeded without receipt")
	}
	var count int
	if err := d.Pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_events").Scan(&count); err != nil || count != 1 {
		t.Fatal("failed purge lost facts", count, err)
	}
	dbtest.MustExec(t, t.Context(), d.Pool, "ALTER TABLE audit_events DROP CONSTRAINT reject_purge")
	result, err := audit.Purge(t.Context(), maintenance, policy, 1)
	if err != nil || result.Deleted != 1 || result.Batches != 1 {
		t.Fatal(result, err)
	}
	var receipt bool
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*)=1 AND bool_and(action='audit.purge_completed' AND (details->>'count')::int=1) FROM audit_events`).Scan(&receipt); err != nil || !receipt {
		t.Fatal(receipt, err)
	}
	result, err = audit.Purge(t.Context(), maintenance, policy, 1)
	if err != nil || result.Deleted != 0 || result.Batches != 0 {
		t.Fatal("idle purge", result, err)
	}
	if err := audit.RecordRestore(t.Context(), maintenance, policy, audit.RestoreDetails{BackupReference: "backup-42", UnknownFrom: time.Now().Add(-time.Hour), UnknownTo: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// A reachable NOINHERIT owner is still a SET ROLE bypass.
	dbtest.MustExec(t, t.Context(), d.Pool, "ALTER ROLE "+pgx.Identifier{runtimeRole}.Sanitize()+" NOINHERIT")
	dbtest.MustExec(t, t.Context(), d.Pool, "GRANT "+pgx.Identifier{d.Pool.Config().ConnConfig.User}.Sanitize()+" TO "+pgx.Identifier{runtimeRole}.Sanitize())
	if err := audit.ValidatePrivileges(t.Context(), runtime, false); err == nil {
		t.Fatal("reachable owner accepted")
	}
}
