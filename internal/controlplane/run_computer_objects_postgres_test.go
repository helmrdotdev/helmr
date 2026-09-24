package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func runObjectStatus(t *testing.T, f *actorCheckpointFixture, request workerapi.RunComputerObjectRequest, operation string) int {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(context.WithValue(t.Context(), workerContextKey{}, f.worker))
	w := httptest.NewRecorder()
	f.server.log = slog.Default()
	f.server.workerRunComputerObject(w, r, operation)
	return w.Code
}
func runObjectFixture(t *testing.T) (*actorCheckpointFixture, workerapi.RunComputerObjectRequest) {
	t.Helper()
	f, completion := finalizingActorRequest(t)
	root := retainedTestGeneration(t, f.Pool, f.server, pgvalue.UUIDString(f.claim.runtime.ID))
	completion.Workspace.Captured.Disk.Root = root
	if err := f.server.registerRunFinalization(t.Context(), f.worker, workerapi.RegisterRunFinalizationRequest{Lease: completion.Lease, OperationID: completion.Workspace.Captured.Receipt.OperationID, Disk: completion.Workspace.Captured.Disk}); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT inspection FROM computer_objects WHERE digest=$1`, root.Pack.Digest).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var e blockformat.ObjectInspection
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	return f, workerapi.RunComputerObjectRequest{Lease: completion.Lease, OperationID: completion.Workspace.Captured.Receipt.OperationID, Inspection: e}
}
func TestRunComputerObjectPublicationAuthority(t *testing.T) {
	for _, mode := range []string{"exact replay", "missing pin", "missing write pin", "foreign key closure", "stale operation", "database unavailable"} {
		t.Run(mode, func(t *testing.T) {
			f, request := runObjectFixture(t)
			observed := &objectStatObserver{Store: f.server.cas}
			f.server.cas = observed
			want := http.StatusConflict
			operation := "certify"
			switch mode {
			case "exact replay":
				want = http.StatusOK
			case "missing pin":
				dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM runtime_computer_object_pins WHERE runtime_instance_id=$1`, f.claim.runtime.ID)
			case "missing write pin":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET write_key_id=(SELECT computer_write_key_id FROM runtime_instances WHERE id=$1) WHERE id=$2`, f.claim.runtime.ID, f.workspaceID)
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET computer_write_key_id=NULL WHERE id=$1`, f.claim.runtime.ID)
			case "foreign key closure":
				key := pgvalue.NewUUIDv7()
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'fixture',decode('01','hex'))`, key, f.EnvironmentID, f.workspaceID)
				object, _ := describeComputerObject(request.Inspection)
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id,is_direct) VALUES($1,$2,$3,$4,false)`, f.EnvironmentID, f.workspaceID, object.digest, key)
				operation = "reuse"
			case "stale operation":
				request.OperationID = pgvalue.UUIDString(pgvalue.NewUUIDv7())
			case "database unavailable":
				f.server.tx = testTxBeginner{beginErr: errors.New("connection interrupted")}
				want = http.StatusInternalServerError
			}
			if got := runObjectStatus(t, f, request, operation); got != want {
				t.Fatalf("status=%d want=%d", got, want)
			}
			if mode == "missing pin" {
				if observed.calls != 0 {
					t.Fatal("unregistered certify reached storage")
				}
				var pins int
				if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM runtime_computer_object_pins WHERE runtime_instance_id=$1`, f.claim.runtime.ID).Scan(&pins); err != nil || pins != 0 {
					t.Fatalf("verify mutated registration: %d %v", pins, err)
				}
			}
			if mode == "exact replay" {
				if got := runObjectStatus(t, f, request, operation); got != want {
					t.Fatalf("replay=%d", got)
				}
			}
		})
	}
}
