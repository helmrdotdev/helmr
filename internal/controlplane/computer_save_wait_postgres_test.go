package controlplane

import (
	"encoding/json"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestComputerSaveSettlementAcrossManagedWait(t *testing.T) {
	for _, stage := range []string{"hot", "checkpointing"} {
		for _, published := range []bool{false, true} {
			name := stage + "/unpublished"
			if published {
				name = stage + "/published"
			}
			t.Run(name, func(t *testing.T) {
				f := newActorCheckpointFixture(t)
				fence := f.fence()
				request := workerapi.ComputerSaveBeginRequest{Lease: &fence, SaveID: uuid.NewV7().String(), Sequence: 1}
				client := computerSaveHTTPClient(t, f.server, f.worker)
				admission, err := client.BeginComputerSave(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				root := retainedTestGeneration(t, f.Pool, f.server, pgvalue.UUIDString(f.claim.runtime.ID), make([]byte, 32))
				var raw []byte
				if err := f.Pool.QueryRow(t.Context(), `SELECT inspection FROM computer_objects WHERE digest=$1`, root.Pack.Digest).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var inspection blockformat.ObjectInspection
				if err := json.Unmarshal(raw, &inspection); err != nil {
					t.Fatal(err)
				}
				object := workerapi.ComputerSaveObjectRequest{Save: request, Inspection: inspection}
				if err := client.ReuseComputerSaveObject(t.Context(), object); err != nil {
					t.Fatal(err)
				}
				publication := workerapi.ComputerSavePublicationRequest{Save: request, Root: root}
				if published {
					if _, err := client.PublishComputerSave(t.Context(), publication); err != nil {
						t.Fatal(err)
					}
				}

				reconciler, registration := actorTokenWait(t, f, session.TurnScope{})
				registration.TurnID = pgtype.UUID{}
				registration.RunGeneration = pgtype.Int8{}
				registration.ActorSpeculativeInputSequence = pgtype.Int8{Int64: 0, Valid: true}
				if _, err := reconciler.RegisterWait(t.Context(), registration); err != nil {
					t.Fatal(err)
				}
				if stage == "checkpointing" {
					parsed, err := parseRunLeaseFence(fence)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.server.requestWorkerRunWaitCheckpoint(t.Context(), f.worker, fence, parsed, registration.WaitID); err != nil {
						t.Fatal(err)
					}
				}
				replay, err := client.BeginComputerSave(t.Context(), request)
				if err != nil || replay != admission {
					t.Fatalf("lost admission replay: %+v %v", replay, err)
				}
				next := request
				next.SaveID = uuid.NewV7().String()
				next.Sequence++
				if _, err := client.BeginComputerSave(t.Context(), next); err == nil {
					t.Fatal("new save admitted during wait")
				}
				if err := client.ReuseComputerSaveObject(t.Context(), object); err == nil {
					t.Fatal("fresh object mutation during wait")
				}
				wrong := request
				wrong.SaveID = uuid.NewV7().String()
				if err := client.AbandonComputerSave(t.Context(), wrong); err == nil {
					t.Fatal("wrong ID released pending save")
				}

				var expiry pgtype.Timestamptz
				if err := f.Pool.QueryRow(t.Context(), `SELECT expires_at FROM workspace_leases WHERE id=$1`, admission.WorkspaceLeaseID).Scan(&expiry); err != nil {
					t.Fatal(err)
				}
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, admission.WorkspaceLeaseID)
				if published {
					if err := client.AdoptComputerSave(t.Context(), publication); err == nil {
						t.Fatal("expired lease accepted adoption during wait")
					}
				} else {
					if err := client.AbandonComputerSave(t.Context(), request); err == nil {
						t.Fatal("expired lease accepted abandonment during wait")
					}
				}
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_leases SET expires_at=$2 WHERE id=$1`, admission.WorkspaceLeaseID, expiry)

				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_version=desired_version+1 WHERE id=$1`, f.claim.runtime.ID)
				if published {
					if err := client.AdoptComputerSave(t.Context(), publication); err == nil {
						t.Fatal("changed Runtime accepted adoption")
					}
				} else {
					if err := client.AbandonComputerSave(t.Context(), request); err == nil {
						t.Fatal("changed Runtime accepted abandonment")
					}
				}
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_version=$2 WHERE id=$1`, f.claim.runtime.ID, admission.DesiredVersion)
				if published {
					if _, err := client.PublishComputerSave(t.Context(), publication); err != nil {
						t.Fatal(err)
					}
					if err := client.AdoptComputerSave(t.Context(), publication); err != nil {
						t.Fatal(err)
					}
					if err := client.AdoptComputerSave(t.Context(), publication); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := client.PublishComputerSave(t.Context(), publication); err == nil {
						t.Fatal("new publication during wait")
					}
					if err := client.AbandonComputerSave(t.Context(), request); err != nil {
						t.Fatal(err)
					}
					if err := client.AbandonComputerSave(t.Context(), request); err != nil {
						t.Fatal(err)
					}
				}
				var pending bool
				if err := f.Pool.QueryRow(t.Context(), `SELECT computer_save_id IS NOT NULL FROM runtime_instances WHERE id=$1`, f.claim.runtime.ID).Scan(&pending); err != nil || pending {
					t.Fatalf("slot not settled %v %v", pending, err)
				}
				// Retired IDs and future requests cannot revive execution in waiting state.
				if _, err := client.BeginComputerSave(t.Context(), next); err == nil {
					t.Fatal("new save after settlement during wait")
				}
			})
		}
	}
}
