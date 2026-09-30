package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// The host owns encrypted bytes. These tests exercise the Control Plane's
// descriptor, membership, lifecycle and transaction boundary using real storage.
// checkpointPublicationFixture returns a capture whose certified disk root is
// uploaded, the registration that also pins that root for the capture, and
// the upload of each runtime artifact.
func checkpointPublicationFixture(t *testing.T) (*computerCheckpointFixture, workerapi.RegisterCheckpointRequest, func(), func(int)) {
	t.Helper()
	f, req := checkpointRegistrationFixture(t)
	root, inspection := retainedTestGeneration(t, f.Pool, f.server.cas, req.ComputerInstanceID)
	req.Manifest.RuntimeState.Computer.Root = root
	artifacts := []*workerapi.CheckpointArtifact{&req.Manifest.RuntimeState.ConfigArtifact, &req.Manifest.RuntimeState.VMStateArtifact, &req.Manifest.RuntimeState.MemoryArtifacts[0], &req.Manifest.RuntimeState.ScratchDiskArtifact}
	data := make([]string, len(artifacts))
	for i, a := range artifacts {
		data[i] = req.CheckpointID + a.MediaType
		a.Digest = sha256sum.DigestBytes([]byte(data[i]))
		a.SizeBytes = int64(len(data[i]))
	}
	register := func() {
		t.Helper()
		f.workerCall(t, f.server.workerRegisterCheckpoint, req, nil)
		publisher, err := computer.NewPublisher(f.Pool, f.server.cas)
		if err != nil {
			t.Fatal(err)
		}
		ref := computer.CheckpointRef{
			Host:       computer.Host{GroupID: f.worker.GroupID, HostID: f.worker.HostID, Epoch: f.worker.Epoch},
			InstanceID: uuid.MustParse(req.ComputerInstanceID), WorkerEpoch: req.WorkerEpoch, DesiredVersion: req.DesiredVersion, CheckpointID: uuid.MustParse(req.CheckpointID),
		}
		if err = publisher.ReuseCheckpointObject(t.Context(), ref, inspection); err != nil {
			t.Fatalf("pin checkpoint root: %v", err)
		}
	}
	return f, req, register, func(i int) {
		t.Helper()
		a := artifacts[i]
		if _, err := f.server.cas.Put(t.Context(), a.MediaType, strings.NewReader(data[i])); err != nil {
			t.Fatal(err)
		}
	}
}

func readyFromRegistration(req workerapi.RegisterCheckpointRequest) workerapi.CheckpointReadyRequest {
	return workerapi.CheckpointReadyRequest(req)
}

func checkpointReadyStatus(t *testing.T, f *computerCheckpointFixture, req workerapi.CheckpointReadyRequest) int {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	r = r.WithContext(context.WithValue(t.Context(), workerContextKey{}, f.worker))
	w := httptest.NewRecorder()
	f.server.workerMarkCheckpointReady(w, r)
	return w.Code
}

func TestCheckpointPublicationCommitsWholeMachineAndReplays(t *testing.T) {
	f, req, register, upload := checkpointPublicationFixture(t)
	register()
	for i := 0; i < 4; i++ {
		upload(i)
	}
	ready := readyFromRegistration(req)
	// Upload timings were unavailable during registration; they are not identity.
	ready.Manifest.Phases = []workerapi.CheckpointPhase{{Name: "upload", DurationMs: 5}}
	var receipt workerapi.ComputerCheckpointResponse
	f.workerCall(t, f.server.workerMarkCheckpointReady, ready, &receipt)
	if receipt.ComputerDiskVersionID == "" {
		t.Fatal("no private Computer version")
	}
	var status, digest, media string
	var logical int64
	err := f.Pool.QueryRow(t.Context(), `SELECT v.status,v.root_pack_digest,v.logical_bytes,a.media_type FROM computer_disk_versions v JOIN computer_disk_version_roots r ON r.version_id=v.id JOIN computer_objects a ON a.digest=r.root_pack_digest AND a.computer_id=r.computer_id AND a.environment_id=r.environment_id WHERE v.id=$1`, receipt.ComputerDiskVersionID).Scan(&status, &digest, &logical, &media)
	if err != nil || status != "private" || digest != req.Manifest.RuntimeState.Computer.Root.Pack.Digest || logical != req.Manifest.RuntimeState.Computer.LogicalBytes || media != "application/octet-stream" {
		t.Fatalf("version %s %s %d %s: %v", status, digest, logical, media, err)
	}
	registered := req.Manifest
	registered.Phases = nil
	registeredJSON, err := json.Marshal(registered)
	if err != nil {
		t.Fatal(err)
	}
	var unchanged bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT manifest=$2::jsonb FROM computer_checkpoints WHERE id=$1`, req.CheckpointID, registeredJSON).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("registered manifest changed: %v", err)
	}
	var storedManifest, phases []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT manifest,phase_timings FROM computer_checkpoints WHERE id=$1`, req.CheckpointID).Scan(&storedManifest, &phases); err != nil {
		t.Fatal(err)
	}
	var stored workerapi.CheckpointManifest
	if err := json.Unmarshal(storedManifest, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Phases) != 0 {
		t.Fatal("timings mutated registered manifest")
	}
	var observed []workerapi.CheckpointPhase
	if err := json.Unmarshal(phases, &observed); err != nil || len(observed) != 1 || observed[0].Name != "upload" || observed[0].DurationMs != 5 {
		t.Fatalf("phase timings=%s: %v", phases, err)
	}
	rows, err := f.server.db.ListCheckpointObjects(t.Context(), pgvalue.UUID(uuid.MustParse(req.CheckpointID)))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.CheckpointStatus != "ready" || row.AvailabilityRequired.Valid {
			t.Fatalf("candidate not committed: %+v", row)
		}
		if n, err := f.server.db.RetireAbandonedCasBlob(t.Context(), row.Digest); err != nil || n != 0 {
			t.Fatalf("live member retired: %d %v", n, err)
		}
	}
	var memberships int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM cas_objects c JOIN computer_checkpoint_objects o ON o.digest=c.digest WHERE o.checkpoint_id=$1`, req.CheckpointID).Scan(&memberships); err != nil || memberships != 4 {
		t.Fatalf("memberships=%d %v", memberships, err)
	}
	var replay workerapi.ComputerCheckpointResponse
	f.workerCall(t, f.server.workerMarkCheckpointReady, ready, &replay)
	if replay != receipt {
		t.Fatal("lost reply created a different publication")
	}
	ready.Manifest.Phases = []workerapi.CheckpointPhase{{Name: "upload", DurationMs: 6}}
	f.workerCall(t, f.server.workerMarkCheckpointReady, ready, &replay)
	if replay != receipt {
		t.Fatal("changed timing created a different publication")
	}
	var retainedTimings []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT phase_timings FROM computer_checkpoints WHERE id=$1`, req.CheckpointID).Scan(&retainedTimings); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(retainedTimings, phases) {
		t.Fatalf("replay replaced original timings: %s -> %s", phases, retainedTimings)
	}
	ready.Manifest.RuntimeState.Config = json.RawMessage(`{"changed":true}`)
	if status := checkpointReadyStatus(t, f, ready); status != http.StatusBadRequest {
		t.Fatalf("changed durable replay status=%d", status)
	}
}

func TestCheckpointPublicationRejectsIncompleteOrChangedCandidate(t *testing.T) {
	for _, scenario := range []string{"missing_registration", "missing_object", "changed_manifest"} {
		t.Run(scenario, func(t *testing.T) {
			f, req, register, upload := checkpointPublicationFixture(t)
			if scenario != "missing_registration" {
				register()
			}
			for i := 0; i < 4; i++ {
				if scenario != "missing_object" || i != 3 {
					upload(i)
				}
			}
			ready := readyFromRegistration(req)
			if scenario == "changed_manifest" {
				ready.Manifest.RuntimeState.Config = json.RawMessage(`{"changed":true}`)
			}
			if status := checkpointReadyStatus(t, f, ready); status < 400 {
				t.Fatalf("accepted %s: %d", scenario, status)
			}
			var status string
			var published bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status,private_computer_disk_version_id IS NOT NULL FROM computer_checkpoints WHERE id=$1`, req.CheckpointID).Scan(&status, &published); err != nil || status != "creating" || published {
				t.Fatalf("partial publication %s %v %v", status, published, err)
			}
			var n int
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM artifacts a JOIN computer_checkpoint_objects o ON o.digest=a.digest WHERE o.checkpoint_id=$1`, req.CheckpointID).Scan(&n); err != nil || n != 0 {
				t.Fatalf("partial artifacts=%d %v", n, err)
			}
		})
	}
}

func TestCheckpointPublicationRollsBackAllMembershipsOnWriteFailure(t *testing.T) {
	f, req, register, upload := checkpointPublicationFixture(t)
	register()
	for i := 0; i < 4; i++ {
		upload(i)
	}
	// Fail after inserting the Computer version and some runtime memberships.
	_, err := f.Pool.Exec(t.Context(), `CREATE FUNCTION reject_checkpoint_memory() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='computer_checkpoint_memory' THEN RAISE EXCEPTION 'injected storage failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_checkpoint_memory BEFORE INSERT ON artifacts FOR EACH ROW EXECUTE FUNCTION reject_checkpoint_memory()`)
	if err != nil {
		t.Fatal(err)
	}
	if status := checkpointReadyStatus(t, f, readyFromRegistration(req)); status != 500 {
		t.Fatalf("status=%d", status)
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM artifacts WHERE digest IN (SELECT digest FROM computer_checkpoint_objects WHERE checkpoint_id=$1)`, req.CheckpointID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial memberships=%d %v", count, err)
	}
	var ready bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='ready' OR private_computer_disk_version_id IS NOT NULL FROM computer_checkpoints WHERE id=$1`, req.CheckpointID).Scan(&ready); err != nil || ready {
		t.Fatalf("partial ready=%v %v", ready, err)
	}
	rows, err := f.server.db.ListCheckpointObjects(t.Context(), pgvalue.UUID(uuid.MustParse(req.CheckpointID)))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if !row.AvailabilityRequired.Valid || !row.AvailabilityRequired.Bool {
			t.Fatal("failed publication released candidate pin")
		}
	}
	if _, err := f.Pool.Exec(t.Context(), `DROP TRIGGER reject_checkpoint_memory ON artifacts`); err != nil {
		t.Fatal(err)
	}
	// Retry the very same candidate; no new capture, registration or identity.
	f.workerCall(t, f.server.workerMarkCheckpointReady, readyFromRegistration(req), nil)
}

func TestCheckpointPublicationConcurrentReplay(t *testing.T) {
	f, req, register, upload := checkpointPublicationFixture(t)
	register()
	for i := 0; i < 4; i++ {
		upload(i)
	}
	ready := readyFromRegistration(req)
	statuses := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() { statuses <- checkpointReadyStatus(t, f, ready) }()
	}
	for i := 0; i < 2; i++ {
		if status := <-statuses; status != 200 {
			t.Fatalf("concurrent ready=%d", status)
		}
	}
	var n int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_disk_versions WHERE source_computer_instance_id=$1 AND status='private'`, req.ComputerInstanceID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("versions=%d %v", n, err)
	}
}

func TestCheckpointPublicationExpiresDuringMembershipWrite(t *testing.T) {
	f, req, register, upload := checkpointPublicationFixture(t)
	register()
	for i := 0; i < 4; i++ {
		upload(i)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	locker, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(context.Background())
	dbtest.MustExec(t, ctx, locker, `SELECT digest FROM cas_blobs WHERE digest=$1 FOR UPDATE`, req.Manifest.RuntimeState.MemoryArtifacts[0].Digest)
	var expiry time.Time
	if err := f.Pool.QueryRow(ctx, `UPDATE computer_checkpoints SET expires_at=clock_timestamp()+interval '3 seconds' WHERE id=$1 RETURNING expires_at`, req.CheckpointID).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	ready := readyFromRegistration(req)
	result := make(chan error, 1)
	go func() { _, err := f.server.commitCheckpointReady(ctx, f.worker, ready); result <- err }()
	for {
		var blocked bool
		if err := f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, locker.Conn().PgConn().PID()).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("did not reach membership lock: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-time.After(time.Until(expiry) + 50*time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := locker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired publication=%v", err)
	}
	var n int
	if err := f.Pool.QueryRow(ctx, `SELECT count(*) FROM artifacts a JOIN computer_checkpoint_objects o ON o.digest=a.digest WHERE o.checkpoint_id=$1`, req.CheckpointID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("expired publication leaked %d memberships: %v", n, err)
	}
}
