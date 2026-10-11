package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type preparationFixture struct {
	fixture
	secrets  *secret.Store
	secretID uuid.UUID
}

func newPreparationFixture(t *testing.T) preparationFixture {
	t.Helper()
	return preparationFixtureFor(t, newFixture(t))
}
func preparationFixtureFor(t *testing.T, resident fixture) preparationFixture {
	t.Helper()
	f := preparationFixture{fixture: resident}
	var err error
	f.secrets, err = secret.New(db.New(f.pool), f.pool, bytes.Repeat([]byte{17}, 32))
	if err != nil {
		t.Fatal(err)
	}
	record, err := f.secrets.Create(t.Context(), f.env, "BUILD_TOKEN_"+f.deployment.String(), []byte("v1"), "create:"+f.deployment.String())
	if err != nil {
		t.Fatal(err)
	}
	f.secretID = pgvalue.MustUUIDValue(record.ID)
	dbtest.MustExec(t, t.Context(), f.pool, `DELETE FROM computer_secret_bindings WHERE environment_id=$1 AND preparation_spec_id=$2; INSERT INTO computer_secret_bindings(environment_id,preparation_spec_id,secret_id,placement_kind,placement_target,mode) VALUES($1,$2,$3,'env','TOKEN','raw'),($1,$2,$3,'env','TOKEN_ALIAS','raw')`, pgx.QueryExecModeSimpleProtocol, f.env, f.deployment, f.secretID)
	return f
}
func (f preparationFixture) waiter(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computers(environment_id,id,preparation_spec_id,preparation_deadline_at) VALUES($1,$2,$3,clock_timestamp()+interval '10 minutes')`, f.env, id, f.deployment)
	return id
}
func (f preparationFixture) attach(t *testing.T, id uuid.UUID) Preparation {
	t.Helper()
	p, err := AttachComputerPreparation(t.Context(), f.pool, f.env, id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func (f preparationFixture) claim(t *testing.T, p Preparation) PreparationExecutor {
	t.Helper()
	ref := PreparationExecutor{EnvironmentID: f.env, PreparationID: p.ID, InstanceID: uuid.NewV7(), Epoch: 1, ChannelCredential: bytes.Repeat([]byte{5}, 32)}
	if _, err := claimPreparationFixture(t.Context(), f.pool, *f.host(), ref); err != nil {
		t.Fatal(err)
	}
	return ref
}
func TestPreparationDemandCoalescingRetainsTerminalAttachment(t *testing.T) {
	f := newPreparationFixture(t)
	const clients = 8
	computers := make([]uuid.UUID, clients)
	for i := range computers {
		computers[i] = f.waiter(t)
	}
	results := make(chan Preparation, clients)
	errs := make(chan error, clients)
	var wg sync.WaitGroup
	for _, id := range computers {
		wg.Go(func() { p, err := AttachComputerPreparation(t.Context(), f.pool, f.env, id); results <- p; errs <- err })
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var p Preparation
	for got := range results {
		if p.ID != uuid.Nil() && p.ID != got.ID {
			t.Fatal("coalesced demand forked")
		}
		p = got
	}
	ref := f.claim(t, p)
	if err := FailPreparation(t.Context(), f.pool, *f.host(), ref, "command_failed"); err != nil {
		t.Fatal(err)
	}
	// Logical failure blocks a new writer without poisoning fresh demand.
	fresh := f.waiter(t)
	if _, err := AttachComputerPreparation(t.Context(), f.pool, f.env, fresh); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unfenced writer: %v", err)
	}
	if err := ObservePreparationStopped(t.Context(), f.pool, *f.host(), ref.Identity()); err != nil {
		t.Fatal(err)
	}
	next := f.attach(t, fresh)
	if next.ID == p.ID {
		t.Fatal("fenced attempt blocked new demand")
	}
	for _, id := range computers {
		if got := f.attach(t, id); got.ID != p.ID || got.Status != "failed" {
			t.Fatalf("lost acknowledgement replayed code: %+v", got)
		}
	}
}
func TestPreparationClaimsDoNotReplaceOrResurrectExecutors(t *testing.T) {
	f := newPreparationFixture(t)
	p := f.attach(t, f.waiter(t))
	ref := f.claim(t, p)
	if _, err := claimPreparationFixture(t.Context(), f.pool, *f.host(), ref); err != nil {
		t.Fatalf("same claim: %v", err)
	}
	wrong := ref
	wrong.InstanceID = uuid.NewV7()
	if _, err := claimPreparationFixture(t.Context(), f.pool, *f.host(), wrong); !errors.Is(err, ErrDenied) {
		t.Fatalf("changed instance: %v", err)
	}
	wrong = ref
	wrong.ChannelCredential = bytes.Repeat([]byte{6}, 32)
	if _, err := RenewPreparation(t.Context(), f.pool, *f.host(), wrong); !errors.Is(err, ErrDenied) {
		t.Fatalf("wrong credential: %v", err)
	}
	host := *f.host()
	host.Epoch++
	if _, err := RenewPreparation(t.Context(), f.pool, host, ref); !errors.Is(err, ErrDenied) {
		t.Fatalf("wrong host epoch: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_preparations SET executor_expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, f.env, p.ID)
	if _, err := RenewPreparation(t.Context(), f.pool, *f.host(), ref); !errors.Is(err, ErrDenied) {
		t.Fatalf("expired renewal: %v", err)
	}
	if _, err := claimPreparationFixture(t.Context(), f.pool, *f.host(), ref); !errors.Is(err, ErrDenied) {
		t.Fatalf("expired reclaim: %v", err)
	}
	if err := ObservePreparationStopped(t.Context(), f.pool, *f.host(), ref.Identity()); err != nil {
		t.Fatalf("late physical stop: %v", err)
	}
}
func TestPreparationExposurePinsBeforeDeliveryAndNeverSwapsVersions(t *testing.T) {
	f := newPreparationFixture(t)
	p := f.attach(t, f.waiter(t))
	ref := f.claim(t, p)
	// Rotation while queued or before first delivery does not expose stale bytes.
	if _, err := f.secrets.Rotate(t.Context(), f.env, f.secretID, []byte("v2"), "rotate-2"); err != nil {
		t.Fatal(err)
	}
	exposed, err := RecordPreparationExposure(t.Context(), f.pool, *f.host(), ref)
	if err != nil || len(exposed) != 2 || exposed[0].Version != 2 || exposed[0].VersionID != exposed[1].VersionID {
		t.Fatalf("exposure: %+v %v", exposed, err)
	}
	var pins int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM secret_exposures WHERE environment_id=$1 AND preparation_id=$2`, f.env, p.ID).Scan(&pins); err != nil || pins != 1 {
		t.Fatalf("alias pins=%d: %v", pins, err)
	}
	if _, err = f.secrets.Rotate(t.Context(), f.env, f.secretID, []byte("v3"), "rotate-3"); err != nil {
		t.Fatal(err)
	}
	replay, err := RecordPreparationExposure(t.Context(), f.pool, *f.host(), ref)
	if err != nil || len(replay) != 2 || replay[0].VersionID != exposed[0].VersionID {
		t.Fatalf("retry replaced pins: %+v %v", replay, err)
	}
	if _, err = f.secrets.Revoke(t.Context(), f.env, f.secretID, "revoke"); err != nil {
		t.Fatal(err)
	}
	if values, err := RecordPreparationExposure(t.Context(), f.pool, *f.host(), ref); !errors.Is(err, ErrDenied) || values != nil {
		t.Fatalf("revoked exposure: %+v %v", values, err)
	}
}

type failedPreparationCommit struct{ db.TxBeginner }
type rejectedPreparationTx struct{ pgx.Tx }

var errPreparationCommit = errors.New("commit rejected")

func (p failedPreparationCommit) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := p.TxBeginner.Begin(ctx)
	return rejectedPreparationTx{tx}, err
}
func (tx rejectedPreparationTx) Commit(context.Context) error { return errPreparationCommit }
func TestPreparationExposureFailureReturnsNoMaterial(t *testing.T) {
	f := newPreparationFixture(t)
	p := f.attach(t, f.waiter(t))
	ref := f.claim(t, p)
	values, err := RecordPreparationExposure(t.Context(), failedPreparationCommit{f.pool}, *f.host(), ref)
	if !errors.Is(err, errPreparationCommit) || values != nil {
		t.Fatalf("failed commit released material: %+v %v", values, err)
	}
	var count int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM secret_exposures WHERE environment_id=$1 AND preparation_id=$2`, f.env, p.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback count=%d: %v", count, err)
	}
	// Expiry is checked after the spec lock, not at the earlier metadata read.
	blocker, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), blocker, `SELECT id FROM computer_preparation_specs WHERE environment_id=$1 AND id=$2 FOR UPDATE`, f.env, f.deployment)
	done := make(chan error, 1)
	go func() {
		values, err := RecordPreparationExposure(t.Context(), f.pool, *f.host(), ref)
		if values != nil {
			err = errors.New("expired operation returned material")
		}
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err = f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%computer_preparation_specs%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exposure did not reach deciding lock")
		}
	}
	dbtest.MustExec(t, t.Context(), blocker, `UPDATE computer_preparations SET deadline_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, f.env, p.ID)
	if err = blocker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, ErrDenied) {
		t.Fatalf("late exposure: %v", err)
	}
}

func TestPreparationDeadlineAndRevocationKeepPhysicalBlocker(t *testing.T) {
	for _, kind := range []string{"queued", "running", "revoked"} {
		t.Run(kind, func(t *testing.T) {
			f := newPreparationFixture(t)
			waiter := f.waiter(t)
			p := f.attach(t, waiter)
			var ref PreparationExecutor
			if kind != "queued" {
				ref = f.claim(t, p)
			}
			if kind == "revoked" {
				if _, err := RecordPreparationExposure(t.Context(), f.pool, *f.host(), ref); err != nil {
					t.Fatal(err)
				}
				if _, err := f.secrets.Revoke(t.Context(), f.env, f.secretID, "revoke"); err != nil {
					t.Fatal(err)
				}
				if _, err := RenewPreparation(t.Context(), f.pool, *f.host(), ref); !errors.Is(err, ErrDenied) {
					t.Fatalf("revoked renewal: %v", err)
				}
				if err := reconcilePreparationRevocation(t.Context(), f.pool, f.env, p.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_preparations SET deadline_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, f.env, p.ID)
				if err := expirePreparation(t.Context(), f.pool, f.env, p.ID); err != nil {
					t.Fatal(err)
				}
			}
			var state string
			var fenced bool
			if err := f.pool.QueryRow(t.Context(), `SELECT status,fenced_at IS NOT NULL FROM computer_preparations WHERE environment_id=$1 AND id=$2`, f.env, p.ID).Scan(&state, &fenced); err != nil || state != "failed" || fenced {
				t.Fatalf("state=%s fence=%v: %v", state, fenced, err)
			}
			fresh := f.waiter(t)
			if kind != "queued" {
				if _, err := AttachComputerPreparation(t.Context(), f.pool, f.env, fresh); !errors.Is(err, ErrNotReady) {
					t.Fatalf("unfenced blocker: %v", err)
				}
				if err := ObservePreparationStopped(t.Context(), f.pool, *f.host(), ref.Identity()); err != nil {
					t.Fatal(err)
				}
			}
			if next := f.attach(t, fresh); next.ID == p.ID {
				t.Fatal("physical stop did not release fresh demand")
			}
			if original := f.attach(t, waiter); original.ID != p.ID {
				t.Fatal("retry created new code execution")
			}
		})
	}
}
func TestPreparationProviderAbsenceAndHostRecoveryRequirePhysicalProof(t *testing.T) {
	t.Run("provider", func(t *testing.T) {
		f := newPreparationFixture(t)
		p := f.attach(t, f.waiter(t))
		f.claim(t, p)
		if err := confirmComputerHostAbsent(t.Context(), f.fixture, f.worker); err != nil {
			t.Fatal(err)
		}
		var stopped bool
		if err := f.pool.QueryRow(t.Context(), `SELECT fenced_at IS NOT NULL AND status='failed' FROM computer_preparations WHERE environment_id=$1 AND id=$2`, f.env, p.ID).Scan(&stopped); err != nil || !stopped {
			t.Fatalf("provider stop=%v: %v", stopped, err)
		}
	})
	t.Run("recovery", func(t *testing.T) {
		f := newPreparationFixture(t)
		p := f.attach(t, f.waiter(t))
		ref := f.claim(t, p)
		host := recoveringComputerHost(t, f.fixture)
		if err := expirePreparation(t.Context(), f.pool, f.env, p.ID); err != nil {
			t.Fatal(err)
		}
		if err := ObserveRecoveredHostComputers(t.Context(), f.pool, host, []uuid.UUID{ref.InstanceID}); err != nil {
			t.Fatal(err)
		}
		var stopped bool
		if err := f.pool.QueryRow(t.Context(), `SELECT fenced_at IS NOT NULL FROM computer_preparations WHERE environment_id=$1 AND id=$2`, f.env, p.ID).Scan(&stopped); err != nil || stopped {
			t.Fatalf("quarantine inferred stop=%v: %v", stopped, err)
		}
		if err := ObserveRecoveredHostComputers(t.Context(), f.pool, host, []uuid.UUID{}); err != nil {
			t.Fatal(err)
		}
		if err := f.pool.QueryRow(t.Context(), `SELECT fenced_at IS NOT NULL FROM computer_preparations WHERE environment_id=$1 AND id=$2`, f.env, p.ID).Scan(&stopped); err != nil || !stopped {
			t.Fatalf("recovered stop=%v: %v", stopped, err)
		}
	})
}

func TestPreparationWaiterDeadlineFailsOnlyPendingWorkAndSealsSessions(t *testing.T) {
	f := newPreparationFixture(t)
	computer := f.waiter(t)
	p := f.attach(t, computer)
	ref := f.claim(t, p)
	session := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO sessions(history_retention_mode,environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth) VALUES('until_environment_deletion',$1,$2,$3,$4,$5,$2,0)`, f.env, session, f.agent, f.deployment, computer)
	request := EnqueueRequest{EnvironmentID: f.env, SessionID: session, RetryKey: "pending", Input: []byte(`[{"type":"text","text":"{\"input\":1}"}]`)}
	admitted, err := Enqueue(t.Context(), f.pool, f.caller(), request)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET preparation_deadline_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, f.env, computer)
	if _, more, err := reconcilePreparationLifecycle(t.Context(), f.pool, preparationLifecyclePosition{}); err != nil || more {
		t.Fatalf("lifecycle more=%v: %v", more, err)
	}
	var state, lifecycle string
	var failed, started bool
	if err = f.pool.QueryRow(t.Context(), `SELECT t.status,t.started_at IS NOT NULL,s.status,c.preparation_failed_at IS NOT NULL FROM turns t JOIN sessions s ON (s.environment_id,s.id)=(t.environment_id,t.session_id) JOIN computers c ON (c.environment_id,c.id)=(s.environment_id,s.computer_id) WHERE t.environment_id=$1 AND t.id=$2`, f.env, admitted.TurnID).Scan(&state, &started, &lifecycle, &failed); err != nil || state != "failed" || started || lifecycle != "closed" || !failed {
		t.Fatalf("turn=%s started=%v session=%s failed=%v: %v", state, started, lifecycle, failed, err)
	}
	if _, err = Enqueue(t.Context(), f.pool, f.caller(), request); err != nil {
		t.Fatalf("original admission identity disappeared: %v", err)
	}
	request.RetryKey = "new"
	if _, err = Enqueue(t.Context(), f.pool, f.caller(), request); !errors.Is(err, ErrNotReady) {
		t.Fatalf("failed Computer admitted new work: %v", err)
	}
	// Failing a waiter never claims its shared preparation writer stopped.
	var fenced bool
	if err = f.pool.QueryRow(t.Context(), `SELECT fenced_at IS NOT NULL FROM computer_preparations WHERE environment_id=$1 AND id=$2`, f.env, p.ID).Scan(&fenced); err != nil || fenced {
		t.Fatalf("false fence=%v: %v", fenced, err)
	}
	if _, err = RenewPreparation(t.Context(), f.pool, *f.host(), ref); err != nil {
		t.Fatalf("waiter failure rewrote shared executor: %v", err)
	}
	if err = expirePreparationWaiter(t.Context(), f.pool, f.env, computer); err != nil {
		t.Fatal(err)
	}
	var events int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_events WHERE environment_id=$1 AND session_id=$2 AND kind='turn.failed'`, f.env, session).Scan(&events); err != nil || events != 1 {
		t.Fatalf("duplicate events=%d: %v", events, err)
	}
}
func TestPreparationLifecycleConvergesRevocationWithoutGuestConnection(t *testing.T) {
	f := newPreparationFixture(t)
	p := f.attach(t, f.waiter(t))
	ref := f.claim(t, p)
	if _, err := RecordPreparationExposure(t.Context(), f.pool, *f.host(), ref); err != nil {
		t.Fatal(err)
	}
	if _, err := f.secrets.Revoke(t.Context(), f.env, f.secretID, "revoke"); err != nil {
		t.Fatal(err)
	}
	if _, more, err := reconcilePreparationLifecycle(t.Context(), f.pool, preparationLifecyclePosition{}); err != nil || more {
		t.Fatalf("lifecycle more=%v: %v", more, err)
	}
	var code string
	if err := f.pool.QueryRow(t.Context(), `SELECT error_code FROM computer_preparations WHERE environment_id=$1 AND id=$2`, f.env, p.ID).Scan(&code); err != nil || code != "secret_revoked" {
		t.Fatalf("code=%s: %v", code, err)
	}
}

func TestPreparationUnclaimedExecutorOperationsAreDenied(t *testing.T) {
	f := newPreparationFixture(t)
	p := f.attach(t, f.waiter(t))
	ref := PreparationExecutor{EnvironmentID: f.env, PreparationID: p.ID, InstanceID: uuid.NewV7(), Epoch: 1, ChannelCredential: bytes.Repeat([]byte{5}, 32)}
	for name, operation := range map[string]func() error{
		"renew":  func() error { _, err := RenewPreparation(t.Context(), f.pool, *f.host(), ref); return err },
		"expose": func() error { _, err := RecordPreparationExposure(t.Context(), f.pool, *f.host(), ref); return err },
		"fail":   func() error { return FailPreparation(t.Context(), f.pool, *f.host(), ref, "failed") },
		"stop":   func() error { return ObservePreparationStopped(t.Context(), f.pool, *f.host(), ref.Identity()) },
	} {
		if err := operation(); !errors.Is(err, ErrDenied) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestPreparationExpiredAttemptDoesNotAttachFreshDemand(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		t.Run(fmt.Sprint(claimed), func(t *testing.T) {
			f := newPreparationFixture(t)
			p := f.attach(t, f.waiter(t))
			var ref PreparationExecutor
			if claimed {
				ref = f.claim(t, p)
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_preparations SET deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, p.ID)
			fresh := f.waiter(t)
			if _, err := AttachComputerPreparation(t.Context(), f.pool, f.env, fresh); !errors.Is(err, ErrNotReady) {
				t.Fatalf("expired attach: %v", err)
			}
			if err := expirePreparation(t.Context(), f.pool, f.env, p.ID); err != nil {
				t.Fatal(err)
			}
			if claimed {
				if err := ObservePreparationStopped(t.Context(), f.pool, *f.host(), ref.Identity()); err != nil {
					t.Fatal(err)
				}
			}
			if next := f.attach(t, fresh); next.ID == p.ID {
				t.Fatal("fresh demand poisoned")
			}
		})
	}
}

func TestPreparationOnlyHostCustodyBlocksActivationAndEnrollment(t *testing.T) {
	f := newPreparationFixture(t)
	p := f.attach(t, f.waiter(t))
	ref := f.claim(t, p)
	host := recoveringComputerHost(t, f.fixture)
	// Remove the fixture Computer's blocker so the preparation is the only custody.
	if err := ObserveRecoveredHostComputers(t.Context(), f.pool, host, []uuid.UUID{ref.InstanceID}); err != nil {
		t.Fatal(err)
	}
	q := db.New(f.pool)
	assertCustody := func(want int64) {
		t.Helper()
		row, err := q.GetCapacityWorkerHost(t.Context(), pgvalue.UUID(host.HostID))
		if err != nil || row.UnreclaimedInstances != want {
			t.Fatalf("custody=%d want %d: %v", row.UnreclaimedInstances, want, err)
		}
		rows, err := q.ListCapacityWorkerHosts(t.Context(), db.ListCapacityWorkerHostsParams{WorkerGroupID: pgvalue.UUID(host.GroupID), HasUnreclaimedInstance: true, ResourceIds: []string{}, Statuses: []string{}, RowLimit: 10})
		if err != nil || int64(len(rows)) != want {
			t.Fatalf("custody-filter rows=%d want %d: %v", len(rows), want, err)
		}
	}
	assertCustody(1)
	activation := db.ActivateWorkerHostParams{
		VMPlatformID:   pgvalue.Text("sha256:" + strings.Repeat("1", 64)),
		EpochCPUMillis: 1000, EpochMemoryBytes: 1 << 30, EpochGuestEphemeralDiskBytes: 1 << 30,
		PerVMCPUMillis: 1000, PerVMMemoryBytes: 1 << 30, PerVMGuestEphemeralDiskBytes: 1 << 30,
		MaxVMSlots: 1, MaxVMStarts: 1, CPUEnvironment: []byte(`{}`), CPUEnvironmentDigest: pgvalue.Text("sha256:" + strings.Repeat("1", 64)),
		WorkerHostID: pgvalue.UUID(host.HostID), WorkerGroupID: pgvalue.UUID(host.GroupID), WorkerEpoch: pgtype.Int8{Int64: host.Epoch, Valid: true},
	}
	recovery := db.CompleteWorkerStartupRecoveryParams{WorkerHostID: activation.WorkerHostID, WorkerGroupID: activation.WorkerGroupID, WorkerEpoch: activation.WorkerEpoch, RecoveryEvidence: []byte(`{"quarantined":[]}`)}
	if _, err := q.CompleteWorkerStartupRecovery(t.Context(), recovery); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unobserved preparation stop: %v", err)
	}
	recovery.RecoveryEvidence = []byte(`{"quarantined":["` + ref.InstanceID.String() + `"]}`)
	if _, err := q.CompleteWorkerStartupRecovery(t.Context(), recovery); err != nil {
		t.Fatal(err)
	}
	if _, err := q.ActivateWorkerHost(t.Context(), activation); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("quarantined preparation activation: %v", err)
	}
	cfg, err := workergroup.NewHostAuthConfig(bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	enrollment := workergroup.Enrollment{TokenHash: make([]byte, 32), PoolName: "pool", ResourceID: "host"}
	if _, err := workergroup.EnrollHost(t.Context(), f.pool, cfg, enrollment); !errors.Is(err, workergroup.ErrHostCustody) {
		t.Fatalf("custody credential replacement: %v", err)
	}
	if err := ObserveRecoveredHostComputers(t.Context(), f.pool, host, []uuid.UUID{}); err != nil {
		t.Fatal(err)
	}
	assertCustody(0)
	recovery.RecoveryEvidence = []byte(`{"quarantined":[]}`)
	if _, err := q.CompleteWorkerStartupRecovery(t.Context(), recovery); err != nil {
		t.Fatal(err)
	}
	if _, err := workergroup.EnrollHost(t.Context(), f.pool, cfg, enrollment); err != nil {
		t.Fatalf("fenced preparation blocked enrollment: %v", err)
	}
	if _, err := q.ActivateWorkerHost(t.Context(), activation); err != nil {
		t.Fatalf("fenced preparation blocked activation: %v", err)
	}
}

func TestPreparationConcurrentClaimsSelectOneExecutor(t *testing.T) {
	f := newPreparationFixture(t)
	p := f.attach(t, f.waiter(t))
	refs := []PreparationExecutor{
		{EnvironmentID: f.env, PreparationID: p.ID, InstanceID: uuid.NewV7(), Epoch: 1, ChannelCredential: bytes.Repeat([]byte{1}, 32)},
		{EnvironmentID: f.env, PreparationID: p.ID, InstanceID: uuid.NewV7(), Epoch: 1, ChannelCredential: bytes.Repeat([]byte{2}, 32)},
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, ref := range refs {
		go func() {
			<-start
			_, err := claimPreparationFixture(t.Context(), f.pool, *f.host(), ref)
			results <- err
		}()
	}
	close(start)
	success, denied := 0, 0
	for range refs {
		err := <-results
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrDenied):
			denied++
		default:
			t.Fatal(err)
		}
	}
	if success != 1 || denied != 1 {
		t.Fatalf("success=%d denied=%d", success, denied)
	}
}

func TestPreparationAllocationCommitPrecedesEnrollmentCustodyCheck(t *testing.T) {
	f, p, _ := preparationAllocationFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='released',fenced_at=clock_timestamp(),fence_evidence='fixture stopped' WHERE worker_host_id=$1`, f.worker)
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	a, err := NewAllocator(tx, bytes.Repeat([]byte{19}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.AllocatePreparation(t.Context(), f.env, p.ID, f.worker); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), tx, `UPDATE worker_hosts SET status='registering' WHERE id=$1`, f.worker)
	cfg, err := workergroup.NewHostAuthConfig(bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := workergroup.EnrollHost(t.Context(), f.pool, cfg, workergroup.Enrollment{TokenHash: make([]byte, 32), PoolName: "pool", ResourceID: "host"})
		done <- err
	}()
	until := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err = f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FOR UPDATE OF t,g%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(until) {
			t.Fatal("enrollment did not wait for claim")
		}
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, workergroup.ErrHostCustody) {
		t.Fatalf("enrollment ignored committed custody: %v", err)
	}
	var claims int64
	if err = f.pool.QueryRow(t.Context(), `SELECT claim_version FROM worker_hosts WHERE id=$1`, f.worker).Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("claims=%d: %v", claims, err)
	}
}

func TestPreparationKnownAuthorityLossDoesNotAttachFreshDemand(t *testing.T) {
	for _, kind := range []string{"host", "revoked"} {
		t.Run(kind, func(t *testing.T) {
			f := newPreparationFixture(t)
			original := f.waiter(t)
			p := f.attach(t, original)
			ref := f.claim(t, p)
			host := *f.host()
			if kind == "host" {
				host = recoveringComputerHost(t, f.fixture)
			} else {
				if _, err := RecordPreparationExposure(t.Context(), f.pool, host, ref); err != nil {
					t.Fatal(err)
				}
				if _, err := f.secrets.Revoke(t.Context(), f.env, f.secretID, ""); err != nil {
					t.Fatal(err)
				}
			}
			fresh := f.waiter(t)
			if _, err := AttachComputerPreparation(t.Context(), f.pool, f.env, fresh); !errors.Is(err, ErrNotReady) {
				t.Fatalf("known %s loss before reconciliation: %v", kind, err)
			}
			if kind == "host" {
				if err := expirePreparation(t.Context(), f.pool, f.env, p.ID); err != nil {
					t.Fatal(err)
				}
				if err := ObserveRecoveredHostComputers(t.Context(), f.pool, host, []uuid.UUID{}); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := reconcilePreparationRevocation(t.Context(), f.pool, f.env, p.ID); err != nil {
					t.Fatal(err)
				}
				if err := ObservePreparationStopped(t.Context(), f.pool, host, ref.Identity()); err != nil {
					t.Fatal(err)
				}
			}
			if next := f.attach(t, fresh); next.ID == p.ID {
				t.Fatal("fresh demand retained dead attempt")
			}
			if replay := f.attach(t, original); replay.ID != p.ID {
				t.Fatal("original receipt replaced")
			}
		})
	}
}
