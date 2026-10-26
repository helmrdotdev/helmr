package session_test

import (
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func TestMutationOwnersWakeDiscovery(t *testing.T) {
	for _, kind := range []string{"control", "save", "lease", "image", "host"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			var build uuid.UUID
			var save computer.SaveRequest
			if kind == "save" {
				_, save = f.finalize(t, "save")
			}
			if kind == "image" {
				checkpointImageExposure(t, f)
				if err := f.pool.QueryRow(t.Context(), `SELECT id FROM image_builds WHERE environment_id=$1`, f.env).Scan(&build); err != nil {
					t.Fatal(err)
				}
			}
			topics := dbtest.ListenChanges(t, f.pool)
			var err error
			switch kind {
			case "control":
				_, err = session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "interrupt", "interrupt"))
			case "save":
				err = db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
					return computer.FailSave(t.Context(), tx, f.env, save.ID, "test publication absence")
				})
			case "lease":
				err = db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error { return computer.MarkLeaseLost(t.Context(), tx, f.env, f.computer, 1) })
			case "image":
				err = db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
					return computer.TerminateImageBuild(t.Context(), tx, f.env, build, "test_failure")
				})
			case "host":
				_, err = workergroup.MarkHostLost(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, audit.Internal(audit.ControlPlane), f.group, "host", 1)
			}
			if err != nil {
				t.Fatal(err)
			}
			got := topics()
			if !got["dispatch"] {
				t.Fatalf("%s did not wake dispatcher: %v", kind, got)
			}
			if kind != "host" && !got["worker:"+f.worker.String()] {
				t.Fatalf("%s did not wake worker: %v", kind, got)
			}
			if kind == "control" && !got["session:"+f.session.String()] {
				t.Fatalf("control did not wake reader: %v", got)
			}
		})
	}
}
