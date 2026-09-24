package controlplane

import (
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"net/http"
	"testing"
)

func TestExecComputerObjectPublicationAuthority(t *testing.T) {
	for _, mode := range []string{"replay", "missing pin", "missing write key", "expired lease", "stale worker", "foreign closure", "database unavailable", "desired version transition"} {
		t.Run(mode, func(t *testing.T) {
			f := newExecGenerationFixture(t)
			var raw []byte
			if err := f.Pool.QueryRow(t.Context(), `SELECT inspection FROM computer_objects WHERE digest=$1`, f.root.Pack.Digest).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var inspection blockformat.ObjectInspection
			if err := json.Unmarshal(raw, &inspection); err != nil {
				t.Fatal(err)
			}
			request := workerapi.ExecComputerObjectRequest{OrgID: f.OrgID.String(), WorkspaceMountID: f.mountID.String(), Inspection: inspection}
			observer := &objectStatObserver{Store: f.server.cas}
			f.server.cas = observer
			handler := f.server.workerCertifyExecComputerObject
			want := http.StatusConflict
			switch mode {
			case "replay":
				want = 200
			case "missing pin":
				dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM runtime_computer_object_pins WHERE runtime_instance_id=$1`, f.runtimeID)
			case "missing write key":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET computer_write_key_id=NULL WHERE id=$1`, f.runtimeID)
			case "expired lease":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE owner_process_id=$1`, f.processID)
			case "stale worker":
				f.worker.WorkerEpoch++
			case "foreign closure":
				key := pgvalue.NewUUIDv7()
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'fixture',decode('01','hex'))`, key, f.EnvironmentID, f.computerID)
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id,is_direct) VALUES($1,$2,$3,$4,false)`, f.EnvironmentID, f.computerID, f.root.Pack.Digest, key)
				handler = f.server.workerReuseExecComputerObject
			case "database unavailable":
				f.server.tx = testTxBeginner{beginErr: errors.New("connection unavailable")}
				want = 500
			case "desired version transition":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_version=desired_version+1 WHERE id=$1`, f.runtimeID)
				handler = f.server.workerReuseExecComputerObject
				want = 200
			}
			w := f.call(t, handler, request)
			if w.Code != want {
				t.Fatalf("got %d %s", w.Code, w.Body)
			}
			if mode == "missing pin" && observer.calls != 0 {
				t.Fatal("unregistered certification reached storage")
			}
			if mode == "desired version transition" {
				var matches bool
				if err := f.Pool.QueryRow(t.Context(), `SELECT p.runtime_desired_version=r.desired_version FROM runtime_computer_object_pins p JOIN runtime_instances r ON r.id=p.runtime_instance_id WHERE r.id=$1 AND p.digest=$2`, f.runtimeID, f.root.Pack.Digest).Scan(&matches); err != nil || !matches {
					t.Fatalf("pin not rebound: %v", err)
				}
			}
		})
	}
}
