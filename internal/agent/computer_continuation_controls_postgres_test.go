package agent

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func TestComputerContinuationControlsObservePostInstallChanges(t *testing.T) {
	for _, kind := range []string{"source-abort", "restore"} {
		t.Run(kind, func(t *testing.T) {
			var f fixture
			var p *agentv1.ComputerSessionInstallation
			var host workergroup.HostPrincipal
			if kind == "source-abort" {
				f = newFixture(t)
				f.peer(t)
				p, _ = sourceAbort(t, f)
				host = *f.host()
			} else {
				r := newComputerRestoreFixture(t)
				f = r.f
				p = r.prepare(t)
				host = r.host
			}
			ctx := t.Context()
			initial := restoreReceipt(p, false, false)
			if _, err := ReadComputerContinuationControls(ctx, f.pool, host, f.env, p, initial); !errors.Is(err, ErrNotReady) && !errors.Is(err, ErrConflict) {
				t.Fatalf("uncommitted controls: %v", err)
			}
			installed := restoreReceipt(p, true, false)
			if p.SourceAbort {
				if err := ValidateComputerSourceAbort(ctx, f.pool, host, f.env, p, initial); err != nil {
					t.Fatal(err)
				}
				if err := CommitComputerSourceAbort(ctx, f.pool, host, f.env, p, installed); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := ValidateComputerRestore(ctx, f.pool, host, f.env, p, initial); err != nil {
					t.Fatal(err)
				}
				if err := CommitComputerRestore(ctx, f.pool, host, f.env, p, installed); err != nil {
					t.Fatal(err)
				}
			}
			peer := uuid.MustParse(p.Grants[0].Identity.SessionId)
			if peer == f.session {
				peer = uuid.MustParse(p.Grants[1].Identity.SessionId)
			}
			dbtest.MustExec(t, ctx, f.pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$2,$3,'local','new human hold')`, f.env, uuid.NewV7(), f.session)
			dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET authority_generation=authority_generation+1 WHERE environment_id=$1; UPDATE sessions SET status='cancelled' WHERE environment_id=$1 AND id=$2`, pgx.QueryExecModeSimpleProtocol, f.env, peer)
			controls, err := ReadComputerContinuationControls(ctx, f.pool, host, f.env, p, installed)
			if err != nil {
				t.Fatal(err)
			}
			if len(controls.Sessions) != len(p.Grants) || controls.DesiredVersion != p.DesiredVersion {
				t.Fatal("control set changed")
			}
			for _, control := range controls.Sessions {
				if !control.Held || control.Stopped != (control.Identity.SessionId == peer.String()) {
					t.Fatal("late hold/cancel missing")
				}
				for _, grant := range p.Grants {
					if grant.Identity.SessionId == control.Identity.SessionId && control.AuthorityGeneration != grant.AuthorityGeneration+1 {
						t.Fatal("stale generation")
					}
				}
			}
			// The read neither acknowledges physical controls nor releases the seal.
			var reconciled bool
			if err := f.pool.QueryRow(ctx, `SELECT controls_reconciled_at IS NOT NULL FROM computer_checkpoints WHERE id=$1`, p.Capture.CheckpointId).Scan(&reconciled); err != nil || reconciled {
				t.Fatalf("read admitted business: %v", err)
			}
			installed.ActivationStarted = true
			installed.Activated = true
			installed.Frozen = false
			if _, err := ReadComputerContinuationControls(ctx, f.pool, host, f.env, p, installed); !errors.Is(err, ErrNotReady) {
				t.Fatalf("activated guest used pre-activation controls: %v", err)
			}
		})
	}
}
