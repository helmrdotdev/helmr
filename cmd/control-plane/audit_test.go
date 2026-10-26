package main

import (
	"bytes"
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/jackc/pgx/v5"
)

func TestAuditCommandsRequireDedicatedAuthority(t *testing.T) {
	d := dbtest.Open(t)
	ctx := t.Context()
	if err := schema.Up(ctx, d.DSN); err != nil {
		t.Fatal(err)
	}
	runtimeRole, maintenanceRole := "runtime_"+uuid.NewV7().String(), "maintenance_"+uuid.NewV7().String()
	roles := pgx.Identifier{runtimeRole}.Sanitize() + ", " + pgx.Identifier{maintenanceRole}.Sanitize()
	for _, role := range []string{runtimeRole, maintenanceRole} {
		dbtest.MustExec(t, ctx, d.Pool, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN")
	}
	t.Cleanup(func() {
		// The isolated fixture owns no relations through either operational role.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := d.Pool.Exec(cleanupCtx, "DROP OWNED BY "+roles); err != nil {
			t.Error(err)
		}
		if _, err := d.Pool.Exec(cleanupCtx, "DROP ROLE "+roles); err != nil {
			t.Error(err)
		}
	})
	dbtest.MustExec(t, ctx, d.Pool, "REVOKE CREATE ON SCHEMA public FROM PUBLIC; GRANT USAGE ON SCHEMA public TO "+roles)
	if err := schema.ConfigurePrivileges(ctx, d.DSN, runtimeRole, maintenanceRole); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUDIT_RETENTION_SECONDS", "3600")
	t.Setenv("AUDIT_MAINTENANCE_TIMEOUT_SECONDS", "30")
	t.Setenv("AUDIT_PURGE_MAX_BATCHES", "2")
	var output bytes.Buffer
	t.Setenv("DATABASE_AUDIT_MAINTENANCE_URL", d.DSN)
	if err := runAuditCommand(ctx, &output, []string{"purge"}); err == nil {
		t.Fatal("migration owner accepted as maintenance")
	}
	parsed, err := url.Parse(d.DSN)
	if err != nil {
		t.Fatal(err)
	}
	parsed.User = url.User(runtimeRole)
	t.Setenv("DATABASE_AUDIT_MAINTENANCE_URL", parsed.String())
	if err := runAuditCommand(ctx, &output, []string{"purge"}); err == nil {
		t.Fatal("append-only runtime accepted as maintenance")
	}
	id := uuid.NewV7()
	if err := db.RunTx(ctx, d.Pool, func(tx pgx.Tx) error {
		return audit.Append(ctx, tx, auditpolicy.Policy{RetentionSeconds: 3600}, audit.Event{ID: id, Evidence: audit.Internal(audit.ControlPlane), Action: "audit.retention_configured", TargetKind: "audit", Details: audit.RetentionObservedDetails{RetentionSeconds: 3600}})
	}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, d.Pool, `UPDATE audit_events SET recorded_at=statement_timestamp()-interval '2 hours',expires_at=statement_timestamp()-interval '1 hour' WHERE id=$1`, id)
	parsed.User = url.User(maintenanceRole)
	t.Setenv("DATABASE_AUDIT_MAINTENANCE_URL", parsed.String())
	if err := runAuditCommand(ctx, &output, []string{"purge"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte(`"deleted":1`)) {
		t.Fatalf("purge result: %s", output.String())
	}
	output.Reset()
	if err := runAuditCommand(ctx, &output, []string{"record-restore", "--backup-reference", "test-backup", "--unknown-from", time.Now().Add(-2 * time.Hour).Format(time.RFC3339), "--unknown-to", time.Now().Add(-time.Hour).Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action IN ('audit.purge_completed','audit.restore_recorded') AND actor_component='audit_maintenance'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("maintenance provenance: %d %v", count, err)
	}
}

func TestAuditCommandRejectsInsufficientBatchDeadline(t *testing.T) {
	t.Setenv("AUDIT_RETENTION_SECONDS", "3600")
	t.Setenv("AUDIT_MAINTENANCE_TIMEOUT_SECONDS", "300")
	t.Setenv("AUDIT_PURGE_MAX_BATCHES", "60")
	var output bytes.Buffer
	err := runAuditCommand(t.Context(), &output, []string{"purge"})
	if err == nil || !strings.Contains(err.Error(), "five-second purge batch deadlines") {
		t.Fatalf("invalid work budget accepted: %v", err)
	}
}
