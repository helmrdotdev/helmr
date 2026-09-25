package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/workerclient"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestComputerSavePublicationAndHistoricalReplay(t *testing.T) {
	f := newExecGenerationFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_processes SET status='running' WHERE id=$1`, f.processID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_mounts SET status='mounted',finalization_action=NULL,finalization_reason_code=NULL,stopped_at=NULL WHERE id=$1`, f.mountID)
	request := workerapi.ComputerSaveBeginRequest{OrgID: f.OrgID.String(), WorkspaceMountID: f.mountID.String(), SaveID: uuid.NewV7().String(), Sequence: 1}
	admission, err := f.server.beginComputerSave(t.Context(), f.worker, request)
	if err != nil {
		t.Fatal(err)
	}
	// This root was certified by the shared object owner, but is not yet retained
	// by this save. A different publication's pin must not authorize it.
	if _, err = f.server.publishComputerSave(t.Context(), f.worker, request, f.root); err == nil {
		t.Fatal("foreign operation pin accepted")
	}
	var raw []byte
	if err = f.Pool.QueryRow(t.Context(), `SELECT inspection FROM computer_objects WHERE digest=$1`, f.root.Pack.Digest).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var inspection blockformat.ObjectInspection
	if err = json.Unmarshal(raw, &inspection); err != nil {
		t.Fatal(err)
	}
	if err = f.server.recordComputerSaveObject(t.Context(), f.worker, request, inspection, "reuse"); err != nil {
		t.Fatal(err)
	}
	first, err := f.server.publishComputerSave(t.Context(), f.worker, request, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.UUIDString(first.VersionID) != request.SaveID {
		t.Fatal("unstable receipt identity")
	}
	var parent, head, origin, pending string
	if err = f.Pool.QueryRow(t.Context(), `SELECT v.parent_version_id::text,c.head_version_id::text,p.base_workspace_version_id::text,r.computer_save_version_id::text FROM computer_versions v JOIN computers c ON c.id=v.computer_id JOIN workspace_processes p ON p.workspace_id=c.id JOIN runtime_instances r ON r.id=v.publisher_runtime_instance_id WHERE v.id=$1`, first.VersionID).Scan(&parent, &head, &origin, &pending); err != nil {
		t.Fatal(err)
	}
	if parent != admission.PredecessorID || head != request.SaveID || origin != f.baseID.String() || pending != request.SaveID {
		t.Fatal("publication changed execution source or released pending ownership")
	}
	if again, e := f.server.beginComputerSave(t.Context(), f.worker, request); e != nil || again != admission {
		t.Fatalf("pending admission replay after publication: %+v %v", again, e)
	}
	if err = f.server.recordComputerSaveObject(t.Context(), f.worker, request, inspection, "reuse"); err == nil {
		t.Fatal("committed save accepted late producer")
	}
	var originalReservation, originalSource pgtype.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT reserved_workspace_version_id,computer_source_version_id FROM runtime_instances WHERE id=$1`, f.runtimeID).Scan(&originalReservation, &originalSource); err != nil {
		t.Fatal(err)
	}
	changedAdoption := f.root
	changedAdoption.LogicalBytes += 4096
	if err = f.server.adoptComputerSave(t.Context(), f.worker, request, changedAdoption); err == nil {
		t.Fatal("adopted different root")
	}
	foreignAdoption := f.worker
	foreignAdoption.WorkerEpoch++
	if err = f.server.adoptComputerSave(t.Context(), foreignAdoption, request, f.root); err == nil {
		t.Fatal("foreign Worker adopted save")
	}
	var expires pgtype.Timestamptz
	if err = f.Pool.QueryRow(t.Context(), `SELECT expires_at FROM workspace_leases WHERE id=$1`, admission.WorkspaceLeaseID).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, admission.WorkspaceLeaseID)
	if err = f.server.adoptComputerSave(t.Context(), f.worker, request, f.root); err == nil {
		t.Fatal("expired lease adopted save")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_leases SET expires_at=$2 WHERE id=$1`, admission.WorkspaceLeaseID, expires)
	// Failure after the source update must roll back slot clearing and pin
	// release together; a lost/failed acknowledgement can then retry safely.
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_save_pin_release() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected pin release failure'; END $$; CREATE TRIGGER reject_save_pin_release BEFORE DELETE ON runtime_computer_object_pins FOR EACH ROW EXECUTE FUNCTION reject_save_pin_release()`)
	if err = f.server.adoptComputerSave(t.Context(), f.worker, request, f.root); err == nil {
		t.Fatal("failed pin release accepted")
	}
	var rollbackSource pgtype.UUID
	var rollbackPending string
	if err = f.Pool.QueryRow(t.Context(), `SELECT computer_source_version_id,computer_save_version_id::text FROM runtime_instances WHERE id=$1`, f.runtimeID).Scan(&rollbackSource, &rollbackPending); err != nil {
		t.Fatal(err)
	}
	if rollbackSource != originalSource || rollbackPending != request.SaveID {
		t.Fatal("failed handoff partially committed")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DROP TRIGGER reject_save_pin_release ON runtime_computer_object_pins; DROP FUNCTION reject_save_pin_release()`)
	// The host acknowledgement follows local durable adoption (tested below).
	if err = f.server.adoptComputerSave(t.Context(), f.worker, request, f.root); err != nil {
		t.Fatal(err)
	}
	if err = f.server.adoptComputerSave(t.Context(), f.worker, request, f.root); err != nil {
		t.Fatalf("adoption replay: %v", err)
	}
	var source string
	var reserved pgtype.UUID
	var savePending bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT computer_source_version_id::text,reserved_workspace_version_id,computer_save_version_id IS NOT NULL FROM runtime_instances WHERE id=$1`, f.runtimeID).Scan(&source, &reserved, &savePending); err != nil {
		t.Fatal(err)
	}
	if source != request.SaveID || reserved != originalReservation || savePending {
		t.Fatal("adoption did not transfer only read source retention")
	}
	next := request
	next.Sequence++
	if _, err = f.server.beginComputerSave(t.Context(), f.worker, next); err == nil {
		t.Fatal("committed generation ID admitted for a new save")
	}
	next.SaveID = uuid.NewV7().String()
	if _, err = f.server.beginComputerSave(t.Context(), f.worker, next); err != nil {
		t.Fatal(err)
	}
	if err = f.server.recordComputerSaveObject(t.Context(), f.worker, next, inspection, "reuse"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.server.publishComputerSave(t.Context(), f.worker, next, f.root); err != nil {
		t.Fatal(err)
	}
	if err = f.server.adoptComputerSave(t.Context(), f.worker, request, f.root); err != nil {
		t.Fatalf("historical adoption after later publication: %v", err)
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT computer_save_version_id::text FROM runtime_instances WHERE id=$1`, f.runtimeID).Scan(&pending); err != nil || pending != next.SaveID {
		t.Fatalf("old adoption cleared successor: %s %v", pending, err)
	}
	replay, err := f.server.publishComputerSave(t.Context(), f.worker, request, f.root)
	if err != nil || replay != first {
		t.Fatalf("historical replay %+v %v", replay, err)
	}
	changed := f.root
	changed.LogicalBytes += 4096
	if _, err = f.server.publishComputerSave(t.Context(), f.worker, request, changed); err == nil {
		t.Fatal("changed request replay accepted")
	}
	foreign := f.worker
	foreign.WorkerEpoch++
	if _, err = f.server.publishComputerSave(t.Context(), foreign, request, f.root); err == nil {
		t.Fatal("foreign worker replay accepted")
	}
}

type saveTestPublisher struct {
	fixture *execGenerationFixture
	client  *workerclient.Client
	request workerapi.ComputerSaveBeginRequest
}

func (p saveTestPublisher) Register(ctx context.Context, i blockformat.ObjectInspection) error {
	return p.client.RegisterComputerSaveObject(ctx, workerapi.ComputerSaveObjectRequest{Save: p.request, Inspection: i})
}
func (p saveTestPublisher) Certify(ctx context.Context, i blockformat.ObjectInspection) error {
	return p.client.CertifyComputerSaveObject(ctx, workerapi.ComputerSaveObjectRequest{Save: p.request, Inspection: i})
}
func (p saveTestPublisher) Reuse(ctx context.Context, i blockformat.ObjectInspection) error {
	return p.client.ReuseComputerSaveObject(ctx, workerapi.ComputerSaveObjectRequest{Save: p.request, Inspection: i})
}
func (p saveTestPublisher) Upload(ctx context.Context, d cas.Descriptor, f *os.File) (cas.Object, error) {
	return initialTestObjectPublisher{p.fixture.server.cas.(*cas.File)}.Publish(ctx, d, f)
}

func TestComputerSavePublishesNewEncryptedGeneration(t *testing.T) {
	f := newExecGenerationFixture(t)
	client := computerSaveHTTPClient(t, f.server, f.worker)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_processes SET status='running' WHERE id=$1`, f.processID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_mounts SET status='mounted',finalization_action=NULL,finalization_reason_code=NULL,stopped_at=NULL WHERE id=$1`, f.mountID)
	request := workerapi.ComputerSaveBeginRequest{OrgID: f.OrgID.String(), WorkspaceMountID: f.mountID.String(), SaveID: uuid.NewV7().String(), Sequence: 1}
	if _, err := client.BeginComputerSave(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	key[0] = 42
	cfg := computer.LocalGenerationConfig{Directory: filepath.Join(t.TempDir(), "owner"), Base: f.root, BaseSource: f.server.cas.(*cas.File), Scope: "fixture", ActiveKey: f.root.Page.KeyID, Keys: map[string][]byte{f.root.Page.KeyID: key}, DirtyBlocks: 8, StagedBytes: 32 << 20, PackLimit: blockformat.MinPackLimit}
	local, err := computer.CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	want := []byte("saved while execution remains active")
	if _, err = local.WriteAt(t.Context(), want, 4096); err != nil {
		t.Fatal(err)
	}
	capture, err := local.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Release()
	if err = capture.Publish(t.Context(), saveTestPublisher{f, client, request}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.PublishComputerSave(t.Context(), workerapi.ComputerSavePublicationRequest{Save: request, Root: capture.Root()}); err != nil {
		t.Fatal(err)
	}
	// A committed receipt precedes local source adoption. This does not yet
	// acknowledge the Runtime slot or release its remote publication pins.
	if err = capture.Adopt(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	if n, e := local.Collect(t.Context(), 1000); e != nil || n <= 0 {
		t.Fatalf("published local bytes not reclaimed: %d %v", n, e)
	}
	fromRemote := make([]byte, len(want))
	if _, err = local.ReadAt(t.Context(), fromRemote, 4096); err != nil || !bytes.Equal(fromRemote, want) {
		t.Fatalf("adopted remote source: %q %v", fromRemote, err)
	}
	if err = client.AdoptComputerSave(t.Context(), workerapi.ComputerSavePublicationRequest{Save: request, Root: capture.Root()}); err != nil {
		t.Fatal(err)
	}

	publication := workerapi.ComputerSavePublicationRequest{Save: request, Root: capture.Root()}
	if receipt, err := client.PublishComputerSave(t.Context(), publication); err != nil || receipt.ComputerID != f.computerID.String() || receipt.VersionID != request.SaveID {
		t.Fatalf("committed HTTP replay: %+v %v", receipt, err)
	}
	if err := client.AdoptComputerSave(t.Context(), publication); err != nil {
		t.Fatal(err)
	}
	if err := client.AbandonComputerSave(t.Context(), request); err == nil {
		t.Fatal("abandoned committed save")
	}
	// A successor write remains local and must not silently enter the published cut.
	if _, err = local.WriteAt(t.Context(), []byte("later"), 4096); err != nil {
		t.Fatal(err)
	}
	if err = local.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(cfg.Directory); err != nil {
		t.Fatal(err)
	}
	cfg.Directory = filepath.Join(t.TempDir(), "restored")
	cfg.Base = capture.Root()
	restored, err := computer.CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got := make([]byte, len(want))
	if _, err = restored.ReadAt(t.Context(), got, 4096); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("remote cut: %q %v", got, err)
	}
}

type saveMutatingStore struct {
	cas.Store
	mutate func()
}

func (s saveMutatingStore) Stat(ctx context.Context, digest string) (cas.Object, error) {
	s.mutate()
	return s.Store.Stat(ctx, digest)
}

func TestComputerSaveRechecksAuthorityAfterAdmission(t *testing.T) {
	for _, mode := range []string{"closed", "failed", "releasing", "stopped", "during stat"} {
		t.Run(mode, func(t *testing.T) {
			f := newExecGenerationFixture(t)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_processes SET status='running' WHERE id=$1`, f.processID)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_mounts SET status='mounted',finalization_action=NULL,finalization_reason_code=NULL,stopped_at=NULL WHERE id=$1`, f.mountID)
			request := workerapi.ComputerSaveBeginRequest{OrgID: f.OrgID.String(), WorkspaceMountID: f.mountID.String(), SaveID: uuid.NewV7().String(), Sequence: 1}
			if _, err := f.server.beginComputerSave(t.Context(), f.worker, request); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err := f.Pool.QueryRow(t.Context(), `SELECT inspection FROM computer_objects WHERE digest=$1`, f.root.Pack.Digest).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var inspection blockformat.ObjectInspection
			if err := json.Unmarshal(raw, &inspection); err != nil {
				t.Fatal(err)
			}
			if err := f.server.recordComputerSaveObject(t.Context(), f.worker, request, inspection, "reuse"); err != nil {
				t.Fatal(err)
			}
			closeRuntime := func() {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.runtimeID)
			}
			switch mode {
			case "closed":
				closeRuntime()
			case "failed":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET observed_state='failed',terminal_at=now(),terminal_reason_code='fixture' WHERE id=$1`, f.runtimeID)
			case "releasing":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_leases SET status='releasing' WHERE owner_process_id=$1`, f.processID)
			case "stopped":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET desired_state='stopped' WHERE id=$1`, f.computerID)
			case "during stat":
				f.server.cas = saveMutatingStore{f.server.cas, closeRuntime}
			}
			if err := f.server.recordComputerSaveObject(t.Context(), f.worker, request, inspection, "certify"); err == nil {
				t.Fatal("stale authority certified")
			}
			if _, err := f.server.publishComputerSave(t.Context(), f.worker, request, f.root); err == nil {
				t.Fatal("stale authority published")
			}
			var head string
			if err := f.Pool.QueryRow(t.Context(), `SELECT head_version_id::text FROM computers WHERE id=$1`, f.computerID).Scan(&head); err != nil || head != f.baseID.String() {
				t.Fatalf("head changed: %s %v", head, err)
			}
		})
	}
}

func TestComputerSaveAbandonmentOnlyReleasesExactOperation(t *testing.T) {
	f := newExecGenerationFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_processes SET status='running' WHERE id=$1`, f.processID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_mounts SET status='mounted',finalization_action=NULL,finalization_reason_code=NULL,stopped_at=NULL WHERE id=$1`, f.mountID)
	request := workerapi.ComputerSaveBeginRequest{OrgID: f.OrgID.String(), WorkspaceMountID: f.mountID.String(), SaveID: uuid.NewV7().String(), Sequence: 1}
	if _, err := f.server.beginComputerSave(t.Context(), f.worker, request); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT inspection FROM computer_objects WHERE digest=$1`, f.root.Pack.Digest).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var inspection blockformat.ObjectInspection
	if err := json.Unmarshal(raw, &inspection); err != nil {
		t.Fatal(err)
	}
	if err := f.server.recordComputerSaveObject(t.Context(), f.worker, request, inspection, "reuse"); err != nil {
		t.Fatal(err)
	}
	if err := f.server.abandonComputerSave(t.Context(), f.worker, request); err != nil {
		t.Fatal(err)
	}
	var abandonedPins int
	key := computerSavePublicationKey(db.RuntimeInstance{ID: pgvalue.UUID(f.runtimeID), ComputerSaveSequence: request.Sequence, ComputerSaveVersionID: pgvalue.UUID(uuid.MustParse(request.SaveID))})
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM runtime_computer_object_pins WHERE runtime_instance_id=$1 AND publication_key=$2`, f.runtimeID, key).Scan(&abandonedPins); err != nil || abandonedPins != 0 {
		t.Fatalf("abandoned pins: %d %v", abandonedPins, err)
	}
	var oldPins int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM runtime_computer_object_pins WHERE runtime_instance_id=$1 AND publication_key=$2`, f.runtimeID, computerPublicationKey("exec", pgvalue.UUID(f.processID), pgvalue.UUID(f.processID))).Scan(&oldPins); err != nil || oldPins == 0 {
		t.Fatalf("other publication lost: %d %v", oldPins, err)
	}
	if err := f.server.recordComputerSaveObject(t.Context(), f.worker, request, inspection, "register"); err == nil {
		t.Fatal("abandoned producer revived")
	}
	next := request
	next.Sequence++
	next.SaveID = uuid.NewV7().String()
	if _, err := f.server.beginComputerSave(t.Context(), f.worker, next); err != nil {
		t.Fatal(err)
	}
	if err := f.server.abandonComputerSave(t.Context(), f.worker, request); err != nil {
		t.Fatalf("old absence acknowledgement: %v", err)
	}
	var pending string
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_save_version_id::text FROM runtime_instances WHERE id=$1`, f.runtimeID).Scan(&pending); err != nil || pending != next.SaveID {
		t.Fatalf("old abandon changed successor: %s %v", pending, err)
	}
	if err := f.server.recordComputerSaveObject(t.Context(), f.worker, next, inspection, "reuse"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.publishComputerSave(t.Context(), f.worker, next, f.root); err != nil {
		t.Fatal(err)
	}
	if err := f.server.abandonComputerSave(t.Context(), f.worker, next); err == nil {
		t.Fatal("published save abandoned before source adoption")
	}
}

func TestComputerSaveAbandonmentReplayAfterAuthorityExpires(t *testing.T) {
	for _, kind := range []string{"actor", "exec"} {
		t.Run(kind, func(t *testing.T) {
			var server *Server
			var worker workerActor
			var expire func()
			request := workerapi.ComputerSaveBeginRequest{SaveID: uuid.NewV7().String(), Sequence: 1}
			if kind == "actor" {
				f := newActorCheckpointFixture(t)
				server, worker = f.server, f.worker
				fence := f.fence()
				request.Lease = &fence
				expire = func() {
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.claim.runtime.ID)
				}
			} else {
				f := newExecGenerationFixture(t)
				server, worker = f.server, f.worker
				request.OrgID, request.WorkspaceMountID = f.OrgID.String(), f.mountID.String()
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_processes SET status='running' WHERE id=$1`, f.processID)
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_mounts SET status='mounted',finalization_action=NULL,finalization_reason_code=NULL,stopped_at=NULL WHERE id=$1`, f.mountID)
				expire = func() {
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.runtimeID)
				}
			}
			if _, err := server.beginComputerSave(t.Context(), worker, request); err != nil {
				t.Fatal(err)
			}
			// Drop the first acknowledgement after a real committed abandonment.
			if err := server.abandonComputerSave(t.Context(), worker, request); err != nil {
				t.Fatal(err)
			}
			expire()
			if err := server.abandonComputerSave(t.Context(), worker, request); err != nil {
				t.Fatalf("lost reply could not reconcile: %v", err)
			}
			foreign := worker
			foreign.WorkerEpoch++
			if err := server.abandonComputerSave(t.Context(), foreign, request); err == nil {
				t.Fatal("foreign Worker saw successful absence")
			}
			future := request
			future.Sequence++
			if err := server.abandonComputerSave(t.Context(), worker, future); err == nil {
				t.Fatal("unretired sequence acknowledged")
			}
		})
	}
}

// Authentication is installed by this fixture; route authentication is exercised
// separately by the production-router tests. Payloads use real Worker transport.
func computerSaveHTTPClient(t *testing.T, s *Server, worker workerActor) *workerclient.Client {
	t.Helper()
	handlers := map[string]http.HandlerFunc{
		"begin": s.workerBeginComputerSave, "abandon": s.workerAbandonComputerSave,
		"publish": s.workerPublishComputerSave, "adopt": s.workerAdoptComputerSave,
		"objects/register": s.workerRegisterComputerSaveObject, "objects/certify": s.workerCertifyComputerSaveObject, "objects/reuse": s.workerReuseComputerSaveObject,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/worker/v1/instance/token" {
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{Token: "save-fixture", ExpiresInSeconds: 3600})
			return
		}
		if r.Header.Get("Authorization") != "Bearer save-fixture" {
			http.Error(w, "missing Worker token", 401)
			return
		}
		for path, handler := range handlers {
			if r.URL.Path == "/worker/v1/run/computer-saves/"+path {
				handler(w, r.WithContext(context.WithValue(r.Context(), workerContextKey{}, worker)))
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := workerclient.New(server.URL, workerclient.WithAuth(worker.WorkerInstanceID.String(), "fixture"), workerclient.WithService("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestComputerSaveHTTPReplayAndAbandonment(t *testing.T) {
	f := newActorCheckpointFixture(t)
	client := computerSaveHTTPClient(t, f.server, f.worker)
	fence := f.fence()
	request := workerapi.ComputerSaveBeginRequest{Lease: &fence, SaveID: uuid.NewV7().String(), Sequence: 1}
	first, err := client.BeginComputerSave(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := client.BeginComputerSave(t.Context(), request)
	if err != nil || replay != first {
		t.Fatalf("admission replay: %+v %v", replay, err)
	}
	if err := client.AbandonComputerSave(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if err := client.AbandonComputerSave(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := client.BeginComputerSave(t.Context(), request); err == nil {
		t.Fatal("revived abandoned operation")
	}
}

func TestComputerSaveReclamationDoesNotFabricateAdoption(t *testing.T) {
	f := newExecGenerationFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_processes SET status='running' WHERE id=$1`, f.processID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_mounts SET status='mounted',finalization_action=NULL,finalization_reason_code=NULL,stopped_at=NULL WHERE id=$1`, f.mountID)
	request := workerapi.ComputerSaveBeginRequest{OrgID: f.OrgID.String(), WorkspaceMountID: f.mountID.String(), SaveID: uuid.NewV7().String(), Sequence: 1}
	if _, err := f.server.beginComputerSave(t.Context(), f.worker, request); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT inspection FROM computer_objects WHERE digest=$1`, f.root.Pack.Digest).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var inspection blockformat.ObjectInspection
	if err := json.Unmarshal(raw, &inspection); err != nil {
		t.Fatal(err)
	}
	if err := f.server.recordComputerSaveObject(t.Context(), f.worker, request, inspection, "reuse"); err != nil {
		t.Fatal(err)
	}
	receipt, err := f.server.publishComputerSave(t.Context(), f.worker, request, f.root)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1,observed_state='closed',terminal_at=now(),terminal_reason_code='fixture',reserved_run_id=NULL,reserved_attempt_number=NULL,reserved_process_id=NULL,reserved_workspace_version_id=NULL,reservation_expires_at=NULL,reclaimed_at=now(),reclaim_evidence='{"fixture":"physical exclusion"}' WHERE id=$1`, f.runtimeID)
	q := db.New(f.Pool)
	if n, err := q.AbandonReclaimedComputerSaves(t.Context(), 100); err != nil || n != 0 {
		t.Fatalf("committed slot abandoned: %d %v", n, err)
	}
	if _, err := q.ReleaseReclaimedComputerObjects(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	if err := f.server.adoptComputerSave(t.Context(), f.worker, request, f.root); err == nil {
		t.Fatal("physical reclaim fabricated local adoption")
	}
	replay, err := f.server.publishComputerSave(t.Context(), f.worker, request, f.root)
	if err != nil || replay != receipt {
		t.Fatalf("lost immutable receipt: %+v %v", replay, err)
	}
	var retained bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM computer_version_roots WHERE version_id=$1)`, request.SaveID).Scan(&retained); err != nil || !retained {
		t.Fatalf("lost committed root: %v", err)
	}
}
