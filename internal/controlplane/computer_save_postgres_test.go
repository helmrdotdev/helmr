package controlplane

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestComputerSaveAdmissionUsesLiveExecutionAuthority(t *testing.T) {
	for _, kind := range []string{"actor", "exec"} {
		for _, mode := range []string{"replay", "stale worker claims", "closed runtime", "expired lease", "expired run budget"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				if kind == "exec" && mode == "expired run budget" {
					t.Skip("Run-only budget")
				}
				var s *Server
				var worker workerActor
				request := workerapi.ComputerSaveBeginRequest{SaveID: uuid.NewV7().String(), Sequence: 1}
				if kind == "actor" {
					f := newActorCheckpointFixture(t)
					s, worker = f.server, f.worker
					if mode == "expired run budget" {
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET max_active_duration_ms=5000,active_started_at=clock_timestamp()-interval '6 seconds' WHERE id=$1`, f.runID)
					}
					fence := f.fence()
					request.Lease = &fence
					if mode == "closed runtime" {
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.claim.runtime.ID)
					}
					if mode == "expired lease" {
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, f.claim.workspaceLease.ID)
					}
				} else {
					f := newExecGenerationFixture(t)
					s, worker = f.server, f.worker
					request.OrgID = f.OrgID.String()
					request.WorkspaceMountID = f.mountID.String()
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_processes SET status='running' WHERE id=$1`, f.processID)
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_mounts SET status='mounted',finalization_kind=NULL,finalization_reason_code=NULL,stopped_at=NULL WHERE id=$1`, f.mountID)
					if mode == "closed runtime" {
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.runtimeID)
					}
					if mode == "expired lease" {
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE owner_process_id=$1`, f.processID)
					}
				}
				if mode == "stale worker claims" {
					worker.ClaimVersion++
				}
				first, err := s.beginComputerSave(t.Context(), worker, request)
				if mode != "replay" {
					if err == nil {
						t.Fatal("stale authority admitted save")
					}
					tx, e := s.tx.Begin(t.Context())
					if e != nil {
						t.Fatal(e)
					}
					defer tx.Rollback(t.Context())
					var pending int
					if e = tx.QueryRow(t.Context(), `SELECT count(*) FROM runtime_instances WHERE computer_save_id=$1`, request.SaveID).Scan(&pending); e != nil || pending != 0 {
						t.Fatalf("rejected request persisted: %d %v", pending, e)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				second, err := s.beginComputerSave(t.Context(), worker, request)
				if err != nil || first != second {
					t.Fatalf("replay: %+v %v", second, err)
				}
				if first.WorkspaceLeaseID == "" || first.PredecessorID == "" {
					t.Fatal("missing server-derived authority")
				}
				request.SaveID = uuid.NewV7().String()
				if _, err := s.beginComputerSave(t.Context(), worker, request); err == nil {
					t.Fatal("competing save admitted")
				}
			})
		}
	}
}
