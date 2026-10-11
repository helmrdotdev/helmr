package agent

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

func TestSourceAbortPreparationUsesCurrentAuthorityAndPreservesState(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	prior, _ := sourceAbort(t, f)
	checkpoint := uuid.MustParse(prior.Capture.CheckpointId)
	observed := abortReceipt(prior, false, false)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET status='cancelled',authority_generation=authority_generation+1 WHERE id=$1`, peer.session)
	p, err := PrepareComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, checkpoint, 1, prior.Envelope.ChannelCredential, observed)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(p.Capture, prior.Capture) || !p.SourceAbort || p.BaseComputerDiskVersionId != prior.BaseComputerDiskVersionId || len(p.StoppedSessions) != 1 || p.StoppedSessions[0].SessionId != peer.session.String() {
		t.Fatalf("changed source identity or lost terminal member: %v", p)
	}
	for _, grant := range p.Grants {
		if grant.Identity.SessionId == peer.session.String() && grant.AuthorityGeneration != 2 {
			t.Fatal("stale authority")
		}
	}
	var state string
	var next int64
	if err := f.pool.QueryRow(t.Context(), `SELECT p.status,c.next_control_version FROM computer_checkpoints p JOIN computers c ON c.id=p.computer_id WHERE p.id=$1`, checkpoint).Scan(&state, &next); err != nil || state != "capturing" || next != p.DesiredVersion {
		t.Fatalf("prepare mutated capture: %s %d %v", state, next, err)
	}
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, observed); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, checkpoint, 1, prior.Envelope.ChannelCredential, abortReceipt(p, true, false)); !errors.Is(err, ErrConflict) {
		t.Fatalf("regenerated installed authority: %v", err)
	}
	if err := CommitComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, true, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, checkpoint, 1, prior.Envelope.ChannelCredential, observed); !errors.Is(err, ErrNotReady) {
		t.Fatalf("regenerated consumed image: %v", err)
	}
}

func TestSourceAbortPreparationRefreshesTerminalSetOnlyBeforeInstallation(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	original, _ := sourceAbort(t, f)
	observed := abortReceipt(original, false, false)
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, original, observed); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET status='cancelled',authority_generation=authority_generation+1 WHERE id=$1`, peer.session)
	p, err := PrepareComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, uuid.MustParse(original.Capture.CheckpointId), 1, original.Envelope.ChannelCredential, observed)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.StoppedSessions) != 1 || p.DesiredVersion != original.DesiredVersion {
		t.Fatal("uninstalled refresh lost terminal set or changed version")
	}
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, true, false)); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed installed identity accepted: %v", err)
	}
	stale := proto.Clone(original).(*agentv1.ComputerSessionInstallation)
	// Preserve the old stop set but refresh authority, isolating terminal-set
	// validation from the independent stale-generation refusal.
	stale.Grants = p.Grants
	stale.Envelope = p.Envelope
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, stale, observed); !errors.Is(err, ErrNotReady) {
		t.Fatalf("stale uninstalled terminal set accepted: %v", err)
	}
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, observed); err != nil {
		t.Fatalf("fresh uninstalled terminal state rejected: %v", err)
	}
	var state string
	var next int64
	if err := f.pool.QueryRow(t.Context(), `SELECT p.status,c.next_control_version FROM computer_checkpoints p JOIN computers c ON c.id=p.computer_id WHERE p.id=$1`, p.Capture.CheckpointId).Scan(&state, &next); err != nil || state != "aborting" || next != p.DesiredVersion+1 {
		t.Fatalf("refresh changed state or version: %s %d %v", state, next, err)
	}

	if err := CommitComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, original, abortReceipt(original, true, false)); !errors.Is(err, ErrConflict) {
		t.Fatalf("obsolete stop set committed: %v", err)
	}
	if err := CommitComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, true, false)); err != nil {
		t.Fatal(err)
	}
}

func TestSourceAbortPreparationRejectsChangedOwnerAndReceipt(t *testing.T) {
	for _, kind := range []string{"host", "epoch", "credential", "expired", "receipt", "version", "installed", "activation-started", "activated", "error", "process-fence"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			p, _ := sourceAbort(t, f)
			host, epoch, credential := *f.host(), int64(1), p.Envelope.ChannelCredential
			observed := abortReceipt(p, false, false)
			expected := ErrConflict
			switch kind {
			case "host":
				host.HostID = uuid.NewV7()
				expected = ErrDenied
				dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO worker_hosts SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$2::uuid,'resource_id','other-host','current_service_id',$2::uuid))).* FROM worker_hosts h WHERE id=$1`, f.worker, host.HostID)
			case "epoch":
				expected = ErrDenied
				epoch++
			case "credential":
				expected = ErrDenied
				credential = "other"
			case "expired":
				expected = ErrDenied
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'`)
			case "receipt":
				observed.CheckpointId = uuid.NewV7().String()
			case "version":
				observed.DesiredVersion++
			case "installed":
				observed.Installed = true
			case "activation-started":
				observed.ActivationStarted = true
			case "activated":
				observed.Activated = true
			case "error":
				observed.Error = "failed"
			case "process-fence":
				expected = ErrNotReady
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET fenced_at=clock_timestamp()`)
			}
			if _, err := PrepareComputerSourceAbort(t.Context(), f.pool, host, f.env, uuid.MustParse(p.Capture.CheckpointId), epoch, credential, observed); !errors.Is(err, expected) {
				t.Fatalf("wrong rejection: got %v want %v", err, expected)
			}
			var state string
			if err := f.pool.QueryRow(t.Context(), `SELECT status FROM computer_checkpoints WHERE id=$1`, p.Capture.CheckpointId).Scan(&state); err != nil || state != "capturing" {
				t.Fatalf("rejection mutated state: %s %v", state, err)
			}
		})
	}
}
