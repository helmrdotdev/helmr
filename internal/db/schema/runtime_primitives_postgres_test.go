package schema_test

import (
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
)

func TestRuntimePrimitivesMigrationPreservesStateAndRejectsConflictingPauses(t *testing.T) {
	for _, mode := range []string{"ready", "paused", "run-only", "vm-only", "different"} {
		t.Run(mode, func(t *testing.T) {
			f := agenttest.New(t)
			dsn := f.Pool.Config().ConnString()
			down, err := schema.FS.ReadFile("migrations/000002_runtime_primitives.down.sql")
			if err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, string(down))
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE schema_migrations SET version=1`)
			var run, vm *string
			reason, other := "maintenance", "disk_floor"
			switch mode {
			case "paused":
				run, vm = &reason, &reason
			case "run-only":
				run = &reason
			case "vm-only":
				vm = &reason
			case "different":
				run, vm = &reason, &other
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET run_paused_reason=$2,vm_paused_reason=$3 WHERE id=$1`, f.Worker, run, vm)
			// Retained successful and pruned receipts must both survive upgrade.
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO platform_retry_keys(environment_id,id,operation,slot_hash,request_fingerprint,deployment_id,accepted_at,receipt_expires_at,receipt)
                VALUES($1,$2,'deployment.finalize',decode(repeat('01',32),'hex'),decode(repeat('02',32),'hex'),$3,statement_timestamp(),statement_timestamp()+interval '720 hours','{}')`, f.Environment, uuid.NewV7(), f.Deployment)
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO platform_retry_keys(environment_id,id,operation,slot_hash,request_fingerprint,deployment_id,accepted_at,receipt_expires_at,receipt_pruned_at)
                VALUES($1,$2,'deployment.finalize',decode(repeat('03',32),'hex'),decode(repeat('02',32),'hex'),$3,statement_timestamp()-interval '721 hours',statement_timestamp()-interval '1 hour',statement_timestamp())`, f.Environment, uuid.NewV7(), f.Deployment)
			err = schema.Up(t.Context(), dsn)
			if mode != "ready" && mode != "paused" {
				if err == nil || !strings.Contains(err.Error(), "worker pause reasons differ") {
					t.Fatalf("conflicting pause migration: %v", err)
				}
				var oldColumns int
				if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_name='worker_hosts' AND column_name IN ('run_paused_reason','vm_paused_reason')`).Scan(&oldColumns); err != nil || oldColumns != 2 {
					t.Fatalf("failed preflight changed columns: %d, %v", oldColumns, err)
				}
				var version int
				var dirty bool
				if err := f.Pool.QueryRow(t.Context(), `SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 2 || !dirty {
					t.Fatalf("rejected migration marker: version=%d dirty=%t error=%v", version, dirty, err)
				}
				// Exercise the documented recovery after verifying no DDL was applied.
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET run_paused_reason=$2,vm_paused_reason=$2 WHERE id=$1`, f.Worker, reason)
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE schema_migrations SET version=1,dirty=false`)
				run = &reason
				err = schema.Up(t.Context(), dsn)
			}
			if err != nil {
				t.Fatal(err)
			}
			var pause *string
			if err := f.Pool.QueryRow(t.Context(), `SELECT admission_paused_reason FROM worker_hosts WHERE id=$1`, f.Worker).Scan(&pause); err != nil {
				t.Fatal(err)
			}
			if (pause == nil) != (run == nil) || (pause != nil && *pause != *run) {
				t.Fatalf("pause changed: %v -> %v", run, pause)
			}
			var receipts, deferredFKs int
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM platform_retry_keys WHERE environment_id=$1 AND deployment_id=$2`, f.Environment, f.Deployment).Scan(&receipts); err != nil || receipts != 2 {
				t.Fatalf("retained receipts: %d, %v", receipts, err)
			}
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_constraint WHERE conrelid='platform_retry_keys'::regclass AND contype='f' AND condeferrable`).Scan(&deferredFKs); err != nil || deferredFKs != 0 {
				t.Fatalf("deferred retry references: %d, %v", deferredFKs, err)
			}
			if _, err := f.Pool.Exec(t.Context(), `INSERT INTO platform_retry_keys(environment_id,id,operation,slot_hash,request_fingerprint,accepted_at,receipt_expires_at) VALUES($1,$2,'deployment.finalize',decode(repeat('04',32),'hex'),decode(repeat('02',32),'hex'),statement_timestamp(),statement_timestamp()+interval '720 hours')`, f.Environment, uuid.NewV7()); err == nil {
				t.Fatal("incomplete retry record was accepted")
			}
		})
	}
}
