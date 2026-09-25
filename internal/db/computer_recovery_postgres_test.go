package db

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func TestComputerLossEpisodeIsStablePostgres(t *testing.T) {
	for _, owner := range []string{"run", "process"} {
		t.Run(owner, func(t *testing.T) {
			ctx := t.Context()
			f := newRunLeaseClaimFixture(t, ctx)
			w := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
			a := startTaskCompletionWork(t, ctx, f, w)
			q := New(f.pool)
			var ownership, writer int64
			if err := f.pool.QueryRow(ctx, `SELECT ownership_generation,writer_generation FROM computers WHERE id=$1`, a.workspaceID).Scan(&ownership, &writer); err != nil {
				t.Fatal(err)
			}
			type result struct {
				id  uuid.UUID
				won bool
				err error
			}
			results := make(chan result, 2)
			for range 2 {
				go func() {
					id := uuid.NewV7()
					r := result{id: id}
					if owner == "run" {
						n, err := q.RequireLostRunComputerRecovery(ctx, RequireLostRunComputerRecoveryParams{
							WorkspaceID: pgvalue.UUID(a.workspaceID), RunLeaseID: pgvalue.UUID(w.leaseID),
							RecoveryID: pgvalue.UUID(id), RecoveryReason: pgvalue.Text("lease_expired"),
						})
						r.won, r.err = n == 1, err
					} else {
						_, err := q.MarkWorkspaceExecRecoveryRequired(ctx, MarkWorkspaceExecRecoveryRequiredParams{
							WorkspaceID: pgvalue.UUID(a.workspaceID), ExpectedHeadVersionID: pgvalue.UUID(a.baseWorkspaceVersionID),
							OwnershipGeneration: ownership, WriterGeneration: writer,
							RecoveryID: pgvalue.UUID(id), RecoveryReason: pgvalue.Text("workspace_exec_worker_lost"),
						})
						r.won, r.err = err == nil, err
						if errors.Is(err, pgx.ErrNoRows) {
							r.err = nil
						}
					}
					results <- r
				}()
			}
			var winner uuid.UUID
			wins := 0
			for range 2 {
				r := <-results
				if r.err != nil {
					t.Fatal(r.err)
				}
				if r.won {
					wins++
					winner = r.id
				}
			}
			if wins != 1 {
				t.Fatalf("loss transitions=%d", wins)
			}
			var id, source uuid.UUID
			var reason, status string
			var started, retained bool
			if err := f.pool.QueryRow(ctx, `SELECT recovery_id,recovery_version_id,recovery_reason,recovery_started_at IS NOT NULL,status,
				c.recovery_payload_required IS TRUE
				FROM computers c WHERE id=$1`, a.workspaceID).Scan(&id, &source, &reason, &started, &status, &retained); err != nil {
				t.Fatal(err)
			}
			wantReason := "lease_expired"
			if owner == "process" {
				wantReason = "workspace_exec_worker_lost"
			}
			if id != winner || source != a.baseWorkspaceVersionID || reason != wantReason || !started || !retained || status != "recovery_required" {
				t.Fatalf("episode=%s source=%s reason=%s started=%v retained=%v status=%s", id, source, reason, started, retained, status)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE computers SET recovery_version_id=NULL WHERE id=$1`, a.workspaceID); err == nil {
				t.Fatal("partial episode accepted")
			}
			other := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
			b := startTaskCompletionWork(t, ctx, f, other)
			if _, err := f.pool.Exec(ctx, `UPDATE computers SET recovery_version_id=$2 WHERE id=$1`, a.workspaceID, b.baseWorkspaceVersionID); err == nil {
				t.Fatal("foreign Computer source accepted")
			}
		})
	}
}
