package agent

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
	"uuid"
)

func sourceAbort(t *testing.T, f fixture) (*agentv1.ComputerSessionInstallation, uuid.UUID) {
	t.Helper()
	return sourceAbortForCheckpoint(t, f, uuid.NewV7())
}

func sourceAbortForCheckpoint(t *testing.T, f fixture, checkpoint uuid.UUID) (*agentv1.ComputerSessionInstallation, uuid.UUID) {
	t.Helper()
	req := f.captureRequest()
	req.CheckpointID = checkpoint
	result, capture := beginCapture(t, f, req)
	envelope := proto.Clone(capture.Envelope).(*computerv0.ComputerOperationEnvelope)
	envelope.OperationExpiresAtUnixNano = time.Now().Add(20 * time.Second).UnixNano()
	p := &agentv1.ComputerSessionInstallation{Capture: capture, Envelope: envelope, DesiredVersion: capture.DesiredVersion + 1, SourceAbort: true}
	if err := f.pool.QueryRow(t.Context(), `SELECT COALESCE(l.restored_from_save_id::text,'sha256:'||encode(c.initial_root_digest,'hex')) FROM computer_leases l JOIN computers c ON (c.environment_id,c.id)=(l.environment_id,l.computer_id) WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=1`, f.env, f.computer).Scan(&p.BaseComputerDiskVersionId); err != nil {
		t.Fatal(err)
	}
	for _, m := range capture.Sessions {
		var generation int64
		if err := f.pool.QueryRow(t.Context(), `SELECT authority_generation FROM sessions WHERE environment_id=$1 AND id=$2`, f.env, m.SessionId).Scan(&generation); err != nil {
			t.Fatal(err)
		}
		p.Grants = append(p.Grants, &agentv1.SessionGrant{Identity: proto.Clone(m).(*agentv1.SessionIdentity), ComputerId: envelope.ComputerId, ComputerInstanceId: envelope.ComputerInstanceId, WriterGeneration: 1, WorkerHostId: f.host().HostID.String(), ComputerLeaseEpoch: 1, AuthorityGeneration: generation, ExpiresAtUnixNano: envelope.OperationExpiresAtUnixNano, ChannelCredential: envelope.ChannelCredential})
	}
	return p, result.SaveID
}

func abortReceipt(p *agentv1.ComputerSessionInstallation, installed, activated bool) *agentv1.ComputerSessionReceipt {
	version := p.Capture.DesiredVersion
	if installed {
		version = p.DesiredVersion
	}
	return &agentv1.ComputerSessionReceipt{CheckpointId: p.Capture.CheckpointId, DesiredVersion: version, Installed: installed, ActivationStarted: activated, Activated: activated}
}
func TestSourceAbortCommitConsumesImageAndWaitsForControls(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	p, save := sourceAbort(t, f)
	for range 2 {
		if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, false, false)); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM computer_saves WHERE id=$1`, save).Scan(&state); err != nil || state != "requested" {
		t.Fatalf("save %s %v", state, err)
	}
	initial := abortReceipt(p, false, false)
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, initial); err != nil {
		t.Fatal(err)
	}
	if err := CommitComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, initial); !errors.Is(err, ErrNotReady) {
		t.Fatalf("uninstalled commit: %v", err)
	}
	installed := abortReceipt(p, true, false)
	for range 2 {
		if err := CommitComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, installed); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, initial); !errors.Is(err, ErrNotReady) {
		t.Fatalf("obsolete RAM accepted: %v", err)
	}
	f.enqueue(t, "queued")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); !errors.Is(err, ErrNotReady) {
		t.Fatalf("dispatch before controls: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$2,$3,'local','explicit human hold')`, f.env, uuid.NewV7(), peer.session)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET authority_generation=authority_generation+1 WHERE id=$1`, peer.session)
	if err := CompleteComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, true, true)); !errors.Is(err, ErrNotReady) {
		t.Fatalf("stale controls accepted: %v", err)
	}
	acknowledgeComputerMembers(t, f, *f.host(), p)
	for range 2 {
		if err := CompleteComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, true, true)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatalf("reconciled source dispatch: %v", err)
	}
	peer.enqueue(t, "held-peer")
	currentPeer := peer.execution()
	currentPeer.AuthorityGeneration++
	if _, err := Dispatch(t.Context(), f.pool, currentPeer); !errors.Is(err, ErrNotReady) {
		t.Fatalf("source abort released peer hold: %v", err)
	}
}

func TestSourceAbortRejectsChangedPhysicalInstallation(t *testing.T) {
	for _, kind := range []string{"base", "instance", "channel", "membership", "generation", "expiry", "version"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			p, _ := sourceAbort(t, f)
			switch kind {
			case "base":
				p.BaseComputerDiskVersionId = "another-base"
			case "instance":
				p.Envelope.ComputerInstanceId = uuid.NewV7().String()
			case "channel":
				p.Envelope.ChannelCredential = "another-channel"
			case "membership":
				p.Grants[0].Identity.ProcessEpoch++
			case "generation":
				p.Grants[0].AuthorityGeneration++
			case "expiry":
				p.Envelope.OperationExpiresAtUnixNano = time.Now().Add(-time.Minute).UnixNano()
			case "version":
				p.DesiredVersion++
			}
			if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, false, false)); err == nil {
				t.Fatal("changed installation accepted")
			}
			var state string
			if err := f.pool.QueryRow(t.Context(), `SELECT status FROM computer_checkpoints WHERE id=$1`, p.Capture.CheckpointId).Scan(&state); err != nil || state != "capturing" {
				t.Fatalf("mutation leaked: %s %v", state, err)
			}
		})
	}
}

func TestSourceAbortCancellationAndLossPreserveRecoveryHolds(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	p, _ := sourceAbort(t, f)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET status='cancelled',authority_generation=authority_generation+1 WHERE id=$1`, peer.session)
	for _, g := range p.Grants {
		if g.Identity.SessionId == peer.session.String() {
			g.AuthorityGeneration++
			p.StoppedSessions = append(p.StoppedSessions, proto.Clone(g.Identity).(*agentv1.SessionIdentity))
		}
	}
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, false, false)); err != nil {
		t.Fatal(err)
	}
	if err := CommitComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, true, false)); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence='physical owner destroyed' WHERE computer_id=$1`, f.computer)
	for range 2 {
		if err := LoseUnreadyComputerCapture(t.Context(), f.pool, f.env, uuid.MustParse(p.Capture.CheckpointId)); err != nil {
			t.Fatal(err)
		}
	}
	for session, want := range map[uuid.UUID]int{f.session: 1, peer.session: 0} {
		var count int
		if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE session_id=$1`, session).Scan(&count); err != nil || count != want {
			t.Fatalf("Session %s holds %d want %d: %v", session, count, want, err)
		}
		var lost bool
		if err := f.pool.QueryRow(t.Context(), `SELECT status='lost' AND failure_recorded_at IS NOT NULL AND fenced_at IS NOT NULL FROM session_processes WHERE session_id=$1`, session).Scan(&lost); err != nil || !lost {
			t.Fatalf("loss/fence omitted %v %v", lost, err)
		}
	}
	var cancelled bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='cancelled' FROM sessions WHERE id=$1`, peer.session).Scan(&cancelled); err != nil || !cancelled {
		t.Fatalf("loss changed cancellation %v %v", cancelled, err)
	}
}

func TestSourceAbortPreservesRecordedDiskCutAndLaterCancelledPeer(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	storage := newSaveStorageFixture(t, f)
	p, save := sourceAbort(t, f)
	cut, root := storage.cut(t, 4)
	if err := RecordCapture(t.Context(), f.pool, CaptureEvidence{EnvironmentID: f.env, SaveID: save, LeaseEpoch: 1, DiskRoot: root, Evidence: "coherent frozen cut"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, false, false)); err != nil {
		t.Fatal(err)
	}
	if err := CommitComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, true, false)); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET status='cancelled',authority_generation=authority_generation+1 WHERE id=$1`, peer.session)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE session_id=$1`, peer.session)
	acknowledgeComputerMembers(t, f, *f.host(), p)
	if err := CompleteComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, true, true)); err != nil {
		t.Fatal(err)
	}
	if err := storage.publish(t, save, cut); err != nil {
		t.Fatal(err)
	}
	f.enqueue(t, "healthy-peer")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
}

func TestSourceAbortRequiresRetainedGuestAndBoundsUninstalledRetries(t *testing.T) {
	f := newFixture(t)
	p, _ := sourceAbort(t, f)
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing guest receipt: %v", err)
	}
	initial := abortReceipt(p, false, false)
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, initial); err != nil {
		t.Fatal(err)
	}
	p.Envelope.OperationExpiresAtUnixNano = time.Now().Add(-time.Minute).UnixNano()
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, initial); !errors.Is(err, ErrNotReady) {
		t.Fatalf("expired retry: %v", err)
	}
	p.Envelope.OperationExpiresAtUnixNano = time.Now().Add(time.Hour).UnixNano()
	p.Grants[0].ExpiresAtUnixNano = p.Envelope.OperationExpiresAtUnixNano
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, initial); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unbounded retry: %v", err)
	}
	p.Envelope.OperationExpiresAtUnixNano = time.Now().Add(20 * time.Second).UnixNano()
	p.Grants[0].ExpiresAtUnixNano = p.Envelope.OperationExpiresAtUnixNano
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET authority_generation=authority_generation+1 WHERE id=$1`, f.session)
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, initial); !errors.Is(err, ErrNotReady) {
		t.Fatalf("stale generation retry: %v", err)
	}
	p.Grants[0].AuthorityGeneration++
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, initial); err != nil {
		t.Fatalf("bounded fresh uninstalled grants: %v", err)
	}
	impossible := abortReceipt(p, true, true)
	if err := CommitComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, impossible); !errors.Is(err, ErrConflict) {
		t.Fatalf("activation before commit accepted: %v", err)
	}
}

func TestCompletedSourceAbortHistoryCannotOwnLaterLoss(t *testing.T) {
	f := newFixture(t)
	var history []uuid.UUID
	for range 2 {
		p, _ := sourceAbort(t, f)
		id := uuid.MustParse(p.Capture.CheckpointId)
		history = append(history, id)
		if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, false, false)); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := RecordCheckpointSaveAbsence(t.Context(), f.pool, *f.host(), f.env, id, "capture pipeline joined before any disk cut"); err != nil {
				t.Fatal(err)
			}
		}
		if err := CommitComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, true, false)); err != nil {
			t.Fatal(err)
		}
		acknowledgeComputerMembers(t, f, *f.host(), p)
		if err := CompleteComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, true, true)); err != nil {
			t.Fatal(err)
		}
		var scrubbed bool
		if err := f.pool.QueryRow(t.Context(), `SELECT capture_request IS NULL FROM computer_checkpoints WHERE id=$1`, id).Scan(&scrubbed); err != nil || !scrubbed {
			t.Fatalf("completed secret retained: %v", err)
		}
		if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, true, true)); err != nil {
			t.Fatalf("scrubbed retry: %v", err)
		}
	}
	req := f.captureRequest()
	beginCapture(t, f, req)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence='source destroyed' WHERE computer_id=$1`, f.computer)
	for _, id := range history {
		if err := LoseUnreadyComputerCapture(t.Context(), f.pool, f.env, id); !errors.Is(err, ErrNotReady) {
			t.Fatalf("historical abort owns loss: %v", err)
		}
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE environment_id=$1`, f.env).Scan(&count); err != nil || count != 0 {
		t.Fatalf("history added holds %d: %v", count, err)
	}
	if err := LoseUnreadyComputerCapture(t.Context(), f.pool, f.env, req.CheckpointID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE environment_id=$1`, f.env).Scan(&count); err != nil || count != 1 {
		t.Fatalf("current loss holds %d: %v", count, err)
	}
}

func TestSourceAbortSaveAbsenceRacesCapture(t *testing.T) {
	f := newFixture(t)
	p, save := sourceAbort(t, f)
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, false, false)); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		results <- RecordCheckpointSaveAbsence(t.Context(), f.pool, *f.host(), f.env, uuid.MustParse(p.Capture.CheckpointId), "all attempts joined without a cut")
	}()
	go func() {
		<-start
		results <- RecordCapture(t.Context(), f.pool, CaptureEvidence{EnvironmentID: f.env, SaveID: save, LeaseEpoch: 1, DiskRoot: p.BaseComputerDiskVersionId, Evidence: "original disk cut receipt"})
	}()
	close(start)
	a, b := <-results, <-results
	if (a == nil) == (b == nil) {
		t.Fatalf("contradictory evidence both won/lost: %v %v", a, b)
	}
}

func TestSourceAbortSealedCaptureAndForeignOwner(t *testing.T) {
	f := newFixture(t)
	p, _ := sourceAbort(t, f)
	captured := abortReceipt(p, false, false)
	captured.Frozen = true
	if err := RecordComputerSealed(t.Context(), f.pool, *f.host(), f.env, uuid.MustParse(p.Capture.CheckpointId), captured); err != nil {
		t.Fatal(err)
	}
	wrong := *f.host()
	wrong.Epoch++
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, wrong, f.env, p, captured); err == nil {
		t.Fatal("foreign worker epoch accepted")
	}
	extra := proto.Clone(p).(*agentv1.ComputerSessionInstallation)
	extra.StoppedSessions = append(extra.StoppedSessions, proto.Clone(extra.Grants[0].Identity).(*agentv1.SessionIdentity))
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, extra, captured); !errors.Is(err, ErrNotReady) {
		t.Fatalf("healthy member marked stopped: %v", err)
	}
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, captured); err != nil {
		t.Fatal(err)
	}
}

func TestSourceAbortArbitratesUnsealedCancellation(t *testing.T) {
	f := newFixture(t)
	p, _ := sourceAbort(t, f)
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		results <- ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, false, false))
	}()
	go func() {
		<-start
		results <- CancelUnsealedComputerCapture(t.Context(), f.pool, *f.host(), f.env, uuid.MustParse(p.Capture.CheckpointId), UnsealedCaptureEvidence{Rejected: true})
	}()
	close(start)
	a, b := <-results, <-results
	if (a == nil) == (b == nil) {
		t.Fatalf("abort and absence both won/lost: %v %v", a, b)
	}
}

func TestSourceAbortRejectsAddedResident(t *testing.T) {
	f := newFixture(t)
	p, _ := sourceAbort(t, f)
	f.peer(t)
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, false, false)); !errors.Is(err, ErrNotReady) {
		t.Fatalf("added resident accepted: %v", err)
	}
}

func TestCheckpointSaveAbsenceRequiresCurrentAbortOwner(t *testing.T) {
	for _, state := range []string{"capturing", "expired", "fenced"} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t)
			p, save := sourceAbort(t, f)
			if state != "capturing" {
				if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, false, false)); err != nil {
					t.Fatal(err)
				}
			}
			switch state {
			case "expired":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE computer_id=$1`, f.computer)
			case "fenced":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence='source gone' WHERE computer_id=$1`, f.computer)
			}
			if err := RecordCheckpointSaveAbsence(t.Context(), f.pool, *f.host(), f.env, uuid.MustParse(p.Capture.CheckpointId), "joined no-cut receipt"); err == nil {
				t.Fatal("unavailable source voided save")
			}
			var actual string
			if err := f.pool.QueryRow(t.Context(), `SELECT status FROM computer_saves WHERE id=$1`, save).Scan(&actual); err != nil || actual != "requested" {
				t.Fatalf("save changed: %s %v", actual, err)
			}
		})
	}
}
