package command

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

type fixture struct {
	agenttest.Fixture
	OrgID, ProjectID uuid.UUID
}

func commandFixture(t *testing.T) fixture {
	t.Helper()
	f := fixture{Fixture: agenttest.New(t)}
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&f.OrgID, &f.ProjectID); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f fixture) request(key string) CreateRequest {
	return CreateRequest{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.Environment, ComputerID: f.Computer, Creator: Creator{SubjectType: string(auth.PrincipalKindSession), SubjectID: f.User.String()}, Argv: []string{"true"}, IdempotencyKey: key}
}
func (f fixture) ref(id uuid.UUID) Ref {
	return Ref{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.Environment, CommandID: id}
}
func (f fixture) host() workergroup.HostPrincipal {
	return workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
}
func (f fixture) claim() ClaimRequest {
	return ClaimRequest{EnvironmentID: f.Environment, InstanceID: f.Computer, WriterGeneration: 1}
}
func (f fixture) create(t *testing.T, key string) db.ComputerCommand {
	t.Helper()
	c, err := Create(t.Context(), f.Pool, f.request(key))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func (f fixture) start(t *testing.T, key string) Start {
	t.Helper()
	c := f.create(t, key)
	r, err := Claim(t.Context(), f.Pool, f.host(), f.claim())
	if err != nil || r.Start == nil || r.Start.Command.ID != c.ID {
		t.Fatalf("start: %+v %v", r, err)
	}
	return *r.Start
}
func (f fixture) get(t *testing.T, id uuid.UUID) db.ComputerCommand {
	t.Helper()
	c, err := Get(t.Context(), db.New(f.Pool), f.ref(id))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func (f fixture) report(id uuid.UUID, outcome string) CompletionReport {
	r := CompletionReport{EnvironmentID: f.Environment, CommandID: id, InstanceID: f.Computer, WriterGeneration: 1, Outcome: outcome, Stdout: OutputBoundary{ThroughSequence: 1, Complete: true}, Stderr: OutputBoundary{ThroughSequence: 1, Complete: true}}
	if outcome == "exited" {
		n := int32(0)
		r.ExitCode = &n
	}
	return r
}

func TestCommandCreateConcurrentRetryAndConflict(t *testing.T) {
	f := commandFixture(t)
	r := f.request("once")
	var wg sync.WaitGroup
	ids := make(chan uuid.UUID, 12)
	errs := make(chan error, 12)
	for range 12 {
		wg.Go(func() {
			c, err := Create(t.Context(), f.Pool, r)
			if err != nil {
				errs <- err
			} else {
				ids <- uuid.UUID(c.ID.Bytes)
			}
		})
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var id uuid.UUID
	for got := range ids {
		if id == uuid.Nil() {
			id = got
		}
		if got != id {
			t.Fatal("duplicate Command")
		}
	}
	c := f.get(t, id)
	if c.Status != "pending" || c.ComputerLeaseEpoch.Valid {
		t.Fatalf("premature physical assignment: %+v", c)
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM platform_retry_keys WHERE environment_id=$1 AND command_id=$2 AND operation='computer.exec' AND receipt->>'command_id'=$2::text`, f.Environment, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("binding count=%d %v", count, err)
	}
	r.Argv = []string{"false"}
	var conflict idempotency.ConflictError
	if _, err := Create(t.Context(), f.Pool, r); !errors.As(err, &conflict) {
		t.Fatalf("changed request: %v", err)
	}
	r = f.request("once")
	r.ProjectID = uuid.NewV7()
	if _, err := Create(t.Context(), f.Pool, r); !errors.Is(err, computer.ErrNotFound) {
		t.Fatalf("scope: %v", err)
	}
}
func TestCommandReplayAfterDeletionAndExpiredReceipt(t *testing.T) {
	f := commandFixture(t)
	c := f.create(t, "replay")
	id := uuid.UUID(c.ID.Bytes)
	if _, err := Cancel(t.Context(), f.Pool, f.ref(id)); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET deleted_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, f.Environment, f.Computer)
	replay, err := Create(t.Context(), f.Pool, f.request("replay"))
	if err != nil || replay.ID != c.ID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	if _, err = Create(t.Context(), f.Pool, f.request("new")); !errors.Is(err, computer.ErrDeleting) {
		t.Fatalf("new admission: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE platform_retry_keys SET accepted_at=accepted_at-interval '31 days',receipt_expires_at=receipt_expires_at-interval '31 days' WHERE environment_id=$1 AND command_id=$2`, f.Environment, id)
	var expired idempotency.ExpiredError
	if _, err = Create(t.Context(), f.Pool, f.request("replay")); !errors.As(err, &expired) {
		t.Fatalf("expired create: %v", err)
	}
	if _, err = Cancel(t.Context(), f.Pool, f.ref(id)); !errors.As(err, &expired) {
		t.Fatalf("expired cancel: %v", err)
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_commands WHERE environment_id=$1`, f.Environment).Scan(&count); err != nil || count != 1 {
		t.Fatalf("expired replay admitted another command: %d %v", count, err)
	}
}
func TestCommandScopeAndCancellationReceipt(t *testing.T) {
	f := commandFixture(t)
	c := f.create(t, "cancel")
	id := uuid.UUID(c.ID.Bytes)
	for _, coordinate := range []string{"org", "project", "environment", "command"} {
		ref := f.ref(id)
		switch coordinate {
		case "org":
			ref.OrgID = uuid.NewV7()
		case "project":
			ref.ProjectID = uuid.NewV7()
		case "environment":
			ref.EnvironmentID = uuid.NewV7()
		case "command":
			ref.CommandID = uuid.NewV7()
		}
		if _, err := Get(t.Context(), db.New(f.Pool), ref); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s get: %v", coordinate, err)
		}
		if _, err := Cancel(t.Context(), f.Pool, ref); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s cancel: %v", coordinate, err)
		}
	}
	receipt, err := Cancel(t.Context(), f.Pool, f.ref(id))
	if err != nil {
		t.Fatal(err)
	}
	again, err := Cancel(t.Context(), f.Pool, f.ref(id))
	if err != nil || again != receipt {
		t.Fatalf("cancel replay %+v %v", again, err)
	}
	c = f.get(t, id)
	if c.Status != "cancelled" || !c.TerminalAt.Valid || c.ComputerLeaseEpoch.Valid {
		t.Fatalf("pending cancellation: %+v", c)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET result_expires_at=clock_timestamp()-interval '1 day',result_pruned_at=clock_timestamp(),argv=NULL,cwd=NULL,env=NULL,stdin=NULL,error=NULL WHERE environment_id=$1 AND id=$2`, f.Environment, id)
	again, err = Cancel(t.Context(), f.Pool, f.ref(id))
	if err != nil || again != receipt {
		t.Fatalf("pruned replay %+v %v", again, err)
	}
}
func TestCommandClaimUsesOneLeaseAndPreservesPeer(t *testing.T) {
	f := commandFixture(t)
	start := f.start(t, "claim")
	id := uuid.UUID(start.Command.ID.Bytes)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE deployments SET execution_revoked_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, f.Environment, f.Deployment)
	r, err := Claim(t.Context(), f.Pool, f.host(), f.claim())
	if err != nil || r.Start == nil || r.Start.Command.ID != start.Command.ID || !bytes.Equal(r.Start.RequestFingerprint, start.RequestFingerprint) {
		t.Fatalf("lost reply: %+v %v", r, err)
	}
	request := f.claim()
	request.ActiveCommandIDs = []uuid.UUID{id}
	r, err = Claim(t.Context(), f.Pool, f.host(), request)
	if err != nil || r.Start != nil {
		t.Fatalf("active duplicate: %+v %v", r, err)
	}
	host := f.host()
	host.HostClaimVersion++
	if _, err = Claim(t.Context(), f.Pool, host, f.claim()); !errors.Is(err, workergroup.ErrStaleClaims) {
		t.Fatalf("stale claims: %v", err)
	}
	if err = CheckStart(t.Context(), f.Pool, f.host(), start); err != nil {
		t.Fatal(err)
	}
	report := f.report(id, "exited")
	if err = Complete(t.Context(), f.Pool, f.host(), report); err != nil {
		t.Fatal(err)
	}
	c := f.get(t, id)
	if !c.TerminalAt.Valid || c.ProcessReconciledAt.Valid {
		t.Fatal("completion claimed physical stop")
	}
	f.acceptEmptyOutput(t, id)
	r, err = Claim(t.Context(), f.Pool, f.host(), f.claim())
	if err != nil || r.Release == nil || r.Release.Completion.CommandID != id {
		t.Fatalf("release: %+v %v", r, err)
	}
	if err = Reconcile(t.Context(), f.Pool, f.host(), report); err != nil {
		t.Fatal(err)
	}
	var state string
	var fenced bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT status,fenced_at IS NOT NULL FROM session_processes WHERE environment_id=$1 AND session_id=$2`, f.Environment, f.Session).Scan(&state, &fenced); err != nil || state != "ready" || fenced {
		t.Fatalf("peer changed %s %t %v", state, fenced, err)
	}
	if err = Complete(t.Context(), f.Pool, f.host(), report); err != nil {
		t.Fatal(err)
	}
	report.ExitCode = new(int32(7))
	if err = Complete(t.Context(), f.Pool, f.host(), report); !errors.Is(err, ErrChanged) {
		t.Fatalf("changed completion: %v", err)
	}
}
func TestCommandCancellationPrecedesOtherStarts(t *testing.T) {
	f := commandFixture(t)
	first := f.start(t, "first")
	id := uuid.UUID(first.Command.ID.Bytes)
	second := f.create(t, "second")
	if _, err := Cancel(t.Context(), f.Pool, f.ref(id)); err != nil {
		t.Fatal(err)
	}
	r, err := Claim(t.Context(), f.Pool, f.host(), f.claim())
	if err != nil || r.Cancellation == nil || r.Cancellation.CommandID != id {
		t.Fatalf("cancellation priority %+v %v", r, err)
	}
	req := f.claim()
	req.ActiveCancellationIDs = []uuid.UUID{id}
	req.ActiveCommandIDs = []uuid.UUID{id}
	r, err = Claim(t.Context(), f.Pool, f.host(), req)
	if err != nil || r.Start == nil || r.Start.Command.ID != second.ID {
		t.Fatalf("independent peer start %+v %v", r, err)
	}
	if err = CheckStart(t.Context(), f.Pool, f.host(), first); !errors.Is(err, ErrChanged) {
		t.Fatalf("cancelled delivery: %v", err)
	}
	report := f.report(id, "computer_command_cancelled")
	if err = Complete(t.Context(), f.Pool, f.host(), report); err != nil {
		t.Fatal(err)
	}
	f.acceptEmptyOutput(t, id)
	if err = Reconcile(t.Context(), f.Pool, f.host(), report); err != nil {
		t.Fatal(err)
	}
}
func TestCommandExpiryNeverReconcilesPhysicalScope(t *testing.T) {
	f := commandFixture(t)
	start := f.start(t, "lost")
	id := uuid.UUID(start.Command.ID.Bytes)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND computer_id=$2 AND epoch=1`, f.Environment, f.Computer)
	if _, err := Claim(t.Context(), f.Pool, f.host(), f.claim()); !errors.Is(err, ErrChanged) {
		t.Fatalf("expired launch: %v", err)
	}
	c := f.get(t, id)
	candidate := RecoveryCandidate{EnvironmentID: f.Environment, CommandID: id, ComputerID: f.Computer, ExpectedRevision: c.Revision}
	if err := Recover(t.Context(), f.Pool, candidate); err != nil {
		t.Fatal(err)
	}
	c = f.get(t, id)
	if c.Status != "lost" || c.ProcessReconciledAt.Valid {
		t.Fatalf("expiry result %+v", c)
	}
	if err := Recover(t.Context(), f.Pool, candidate); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale recovery: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET status='released',fenced_at=clock_timestamp(),fence_evidence='test physical VM stopped' WHERE environment_id=$1 AND computer_id=$2 AND epoch=1`, f.Environment, f.Computer)
	candidate.ExpectedRevision = c.Revision
	if err := Recover(t.Context(), f.Pool, candidate); err != nil {
		t.Fatal(err)
	}
	c = f.get(t, id)
	if !c.ProcessReconciledAt.Valid {
		t.Fatal("physical fence not reconciled")
	}
	var epoch int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_lease_epoch FROM computer_commands WHERE environment_id=$1 AND id=$2`, f.Environment, id).Scan(&epoch); err != nil || epoch != 1 {
		t.Fatalf("execution rebound: %d %v", epoch, err)
	}
}
func TestCommandFailPendingRejectsChangedBinding(t *testing.T) {
	f := commandFixture(t)
	c := f.create(t, "pending")
	id := uuid.UUID(c.ID.Bytes)
	failure := Failure{Code: "computer_unavailable", Detail: []byte(`{"code":"computer_unavailable"}`)}
	if err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
		return FailPending(t.Context(), tx, Pending{EnvironmentID: f.Environment, CommandID: id, ExpectedRevision: c.Revision}, failure)
	}); err != nil {
		t.Fatal(err)
	}
	c = f.get(t, id)
	if c.Status != "failed" || c.FailureReason.String != "dispatch_failed" {
		t.Fatalf("failed pending %+v", c)
	}
	second := f.create(t, "bound-after-discovery")
	id = uuid.UUID(second.ID.Bytes)
	if _, err := Claim(t.Context(), f.Pool, f.host(), f.claim()); err != nil {
		t.Fatal(err)
	}
	if err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
		return FailPending(t.Context(), tx, Pending{EnvironmentID: f.Environment, CommandID: id, ExpectedRevision: second.Revision}, failure)
	}); !errors.Is(err, ErrChanged) {
		t.Fatalf("bound pending changed: %v", err)
	}
}
func commandSecrets(t *testing.T, f fixture) (*secret.Store, uuid.UUID) {
	t.Helper()
	store, err := secret.New(db.New(f.Pool), f.Pool, bytes.Repeat([]byte{17}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Create(t.Context(), f.Environment, "COMMAND_TOKEN", []byte("first"), "secret")
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.UUID(s.ID.Bytes)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_secret_bindings(environment_id,computer_id,secret_id,placement_kind,placement_target,mode) VALUES($1,$2,$3,'env','TOKEN','raw')`, f.Environment, f.Computer, id)
	return store, id
}
func TestCommandSecretSelectionOccursAtFirstDelivery(t *testing.T) {
	f := commandFixture(t)
	store, secretID := commandSecrets(t, f)
	c := f.create(t, "first")
	id := uuid.UUID(c.ID.Bytes)
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM secret_exposures WHERE environment_id=$1 AND command_id=$2`, f.Environment, id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("admission selected Secret: %d %v", count, err)
	}
	if _, err := store.Rotate(t.Context(), f.Environment, secretID, []byte("second"), "rotate-before-delivery"); err != nil {
		t.Fatal(err)
	}
	first, err := Claim(t.Context(), f.Pool, f.host(), f.claim())
	if err != nil || first.Start == nil || len(first.Start.Secrets) != 1 || first.Start.Secrets[0].Version.Version != 2 {
		t.Fatalf("first delivery %+v %v", first, err)
	}
	if _, err = store.Rotate(t.Context(), f.Environment, secretID, []byte("third"), "rotate-after-delivery"); err != nil {
		t.Fatal(err)
	}
	retry, err := Claim(t.Context(), f.Pool, f.host(), f.claim())
	if err != nil || retry.Start == nil || retry.Start.Secrets[0].Version.ID != first.Start.Secrets[0].Version.ID {
		t.Fatalf("retry reselected %+v %v", retry, err)
	}
	materials, err := store.OpenDeliveries(f.Environment, retry.Start.Secrets)
	if err != nil || len(materials) != 1 || string(materials[0].Value) != "second" {
		t.Fatalf("retained delivery %v %v", len(materials), err)
	}
	for _, m := range materials {
		clear(m.Value)
	}
	second := f.create(t, "next")
	req := f.claim()
	req.ActiveCommandIDs = []uuid.UUID{id}
	next, err := Claim(t.Context(), f.Pool, f.host(), req)
	if err != nil || next.Start == nil || next.Start.Command.ID != second.ID || next.Start.Secrets[0].Version.Version != 3 {
		t.Fatalf("new logical command %+v %v", next, err)
	}
	if err = CheckStart(t.Context(), f.Pool, f.host(), *first.Start); err != nil {
		t.Fatal(err)
	}
	_, err = store.Revoke(t.Context(), f.Environment, secretID, "revoke")
	if err != nil {
		t.Fatal(err)
	}
	if err = CheckStart(t.Context(), f.Pool, f.host(), *first.Start); !errors.Is(err, secret.ErrDeliveryRevoked) {
		t.Fatalf("revoked delivery %v", err)
	}
	count, err = StopSecretRevokedCommands(t.Context(), f.Pool, secret.Revocation{EnvironmentID: f.Environment, SecretID: secretID, Generation: 1}, 10)
	if err != nil || count != 2 {
		t.Fatalf("revocation %d %v", count, err)
	}
	c = f.get(t, id)
	if c.Status != "stopping" || c.TerminalAt.Valid || c.ProcessReconciledAt.Valid {
		t.Fatalf("revocation claimed stop %+v", c)
	}
}

func TestCommandCaptureWaitsForPhysicalReconciliation(t *testing.T) {
	f := commandFixture(t)
	start := f.start(t, "capture")
	id := uuid.UUID(start.Command.ID.Bytes)
	request := agent.ComputerCaptureRequest{EnvironmentID: f.Environment, ComputerID: f.Computer, CheckpointID: uuid.NewV7(), LeaseEpoch: 1, ChannelCredential: agenttest.ChannelCredential}
	if _, err := agent.BeginComputerCapture(t.Context(), f.Pool, f.host(), request); !errors.Is(err, agent.ErrNotReady) {
		t.Fatalf("running capture: %v", err)
	}
	report := f.report(id, "exited")
	if err := Complete(t.Context(), f.Pool, f.host(), report); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.BeginComputerCapture(t.Context(), f.Pool, f.host(), request); !errors.Is(err, agent.ErrNotReady) {
		t.Fatalf("unreconciled capture: %v", err)
	}
	f.acceptEmptyOutput(t, id)
	if err := Reconcile(t.Context(), f.Pool, f.host(), report); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.BeginComputerCapture(t.Context(), f.Pool, f.host(), request); err != nil {
		t.Fatalf("reconciled capture: %v", err)
	}
	f.create(t, "after-seal")
	claimed, err := Claim(t.Context(), f.Pool, f.host(), f.claim())
	if err != nil || claimed.Start != nil {
		t.Fatalf("launch behind capture: %+v %v", claimed, err)
	}
}
func TestCommandClaimRacesCaptureUnderComputerLock(t *testing.T) {
	f := commandFixture(t)
	f.create(t, "race")
	request := agent.ComputerCaptureRequest{EnvironmentID: f.Environment, ComputerID: f.Computer, CheckpointID: uuid.NewV7(), LeaseEpoch: 1, ChannelCredential: agenttest.ChannelCredential}
	var claim ClaimResult
	var claimErr, captureErr error
	var wg sync.WaitGroup
	gate := make(chan struct{})
	wg.Go(func() { <-gate; claim, claimErr = Claim(t.Context(), f.Pool, f.host(), f.claim()) })
	wg.Go(func() { <-gate; _, captureErr = agent.BeginComputerCapture(t.Context(), f.Pool, f.host(), request) })
	close(gate)
	wg.Wait()
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if claim.Start != nil && !errors.Is(captureErr, agent.ErrNotReady) {
		t.Fatalf("execution and capture admitted together: %v", captureErr)
	}
	if claim.Start == nil && captureErr != nil {
		t.Fatalf("neither contender admitted: %v", captureErr)
	}
}

func TestCommandStandaloneComputerCreateReplayAndDeletion(t *testing.T) {
	f := commandFixture(t)
	store, secretID := commandSecrets(t, f)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_definitions SET resources='{"milliCpu":1000,"memoryMiB":512}' WHERE environment_id=$1 AND deployment_id=$2`, f.Environment, f.Deployment)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_preparation_specs SET seed='{"profile":"linux-amd64-ext4-v1"}' WHERE environment_id=$1 AND id=$2`, f.Environment, f.Deployment)
	creator := computer.NewCreator(store)
	key := "repair-shell"
	scope := computer.Scope{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.Environment}
	request := computer.Request{Scope: scope, DefinitionKey: "fixture-computer", Key: &key, Secrets: []secretbinding.Reference{{SecretID: secretID.String(), Env: &secretbinding.ReferenceEnv{Name: "TOKEN", Mode: "raw"}}}, IdempotencyKey: "create"}
	first, err := creator.Create(t.Context(), f.Pool, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Snapshot.DefinitionKey != "fixture-computer" || first.Snapshot.DeploymentID != f.Deployment.String() || len(first.Snapshot.Secrets) != 1 || first.Snapshot.Secrets[0].SecretID != secretID.String() {
		t.Fatalf("creation projection %+v", first.Snapshot)
	}
	other := request
	other.IdempotencyKey = "other-key"
	var keyConflict computer.KeyConflictError
	if _, err = creator.Create(t.Context(), f.Pool, other); !errors.As(err, &keyConflict) {
		t.Fatalf("unique Computer key %v", err)
	}
	// A changed current deployment cannot retarget an accepted creation.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=NULL WHERE id=$1`, f.Environment)
	replay, err := creator.Create(t.Context(), f.Pool, request)
	if err != nil || !replay.Replayed || replay.ComputerID != first.ComputerID || replay.Snapshot.DeploymentID != first.Snapshot.DeploymentID {
		t.Fatalf("replay %+v %v", replay, err)
	}
	// The certified test root stands in for successful preparation in this owner test.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers c SET initial_root_id=source.initial_root_id,initial_root_digest=source.initial_root_digest FROM computers source WHERE c.environment_id=$1 AND c.id=$2 AND source.environment_id=$1 AND source.id=$3`, f.Environment, first.ComputerID, f.Computer)
	exec := f.request("repair")
	exec.ComputerID = first.ComputerID
	cmd, err := Create(t.Context(), f.Pool, exec)
	if err != nil {
		t.Fatal(err)
	}
	deletion := computer.Deletion{Scope: scope, ComputerID: first.ComputerID, IdempotencyKey: "delete"}
	if _, err = computer.Delete(t.Context(), f.Pool, deletion); !errors.Is(err, computer.ErrBusy) {
		t.Fatalf("delete live Command: %v", err)
	}
	if _, err = Cancel(t.Context(), f.Pool, f.ref(uuid.UUID(cmd.ID.Bytes))); err != nil {
		t.Fatal(err)
	}
	deleted, err := computer.Delete(t.Context(), f.Pool, deletion)
	if err != nil || deleted.ComputerID != first.ComputerID {
		t.Fatalf("delete %+v %v", deleted, err)
	}
	again, err := computer.Delete(t.Context(), f.Pool, deletion)
	if err != nil || !again.Replayed {
		t.Fatalf("delete replay %+v %v", again, err)
	}
	snapshot, err := computer.Read(t.Context(), f.Pool, scope, first.ComputerID)
	if err != nil || snapshot.Status != computer.StatusDeleted {
		t.Fatalf("tombstone %+v %v", snapshot, err)
	}
	if _, err = Create(t.Context(), f.Pool, exec); err != nil {
		t.Fatalf("retained command replay: %v", err)
	}
	request.Secrets = []secretbinding.Reference{}
	var conflict idempotency.ConflictError
	if _, err = creator.Create(t.Context(), f.Pool, request); !errors.As(err, &conflict) {
		t.Fatalf("changed creation: %v", err)
	}
}

func TestCommandProtectedSelectorTracksLogicalOwner(t *testing.T) {
	f := commandFixture(t)
	store, secretID := commandSecrets(t, f)
	marker, err := secretbinding.Placeholder("protected")
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_secret_bindings(environment_id,computer_id,secret_id,placement_kind,placement_target,mode,allowed_origins,placeholder) VALUES($1,$2,$3,'env','PROTECTED_TOKEN','protected',ARRAY['https://api.example.com'],$4)`, f.Environment, f.Computer, secretID, marker)
	trust, err := store.GenerateProxyTrust(f.Environment, f.Computer, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET proxy_ca_certificate=$3,proxy_ca_not_after=$4,proxy_ca_private_key_nonce=$5,proxy_ca_private_key_ciphertext=$6 WHERE environment_id=$1 AND id=$2`, f.Environment, f.Computer, trust.Certificate, trust.NotAfter, trust.PrivateKeyNonce, trust.PrivateKeyCiphertext)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET observed_at=clock_timestamp() WHERE id=$1`, f.Worker)
	first := f.start(t, "protected")
	id := uuid.UUID(first.Command.ID.Bytes)
	selector := first.ProtectedEnv["PROTECTED_TOKEN"]
	if selector == "" || selector == marker || !bytes.Equal(first.ProxyCA, trust.Certificate) {
		t.Fatal("missing logical owner selector")
	}
	values, err := agent.CaptureComputerProtectedSecrets(t.Context(), f.Pool, f.host(), f.Computer, "https://api.example.com", []string{selector})
	if err != nil || len(values) != 1 {
		t.Fatalf("resolve: %d %v", len(values), err)
	}
	if _, err = store.Rotate(t.Context(), f.Environment, secretID, []byte("next"), "protected-rotate"); err != nil {
		t.Fatal(err)
	}
	retry, err := Claim(t.Context(), f.Pool, f.host(), f.claim())
	if err != nil || retry.Start == nil || retry.Start.ProtectedEnv["PROTECTED_TOKEN"] != selector {
		t.Fatalf("selector changed: %+v %v", retry, err)
	}
	current, err := agent.CaptureComputerProtectedSecrets(t.Context(), f.Pool, f.host(), f.Computer, "https://api.example.com", []string{selector})
	if err != nil || len(current) != 1 || current[0].VersionID != values[0].VersionID {
		t.Fatalf("pin changed %+v %v", current, err)
	}
	if _, err = agent.CaptureComputerProtectedSecrets(t.Context(), f.Pool, f.host(), f.Computer, "https://other.example.com", []string{selector}); err == nil {
		t.Fatal("wrong origin resolved")
	}
	if _, err = Cancel(t.Context(), f.Pool, f.ref(id)); err != nil {
		t.Fatal(err)
	}
	if _, err = agent.CaptureComputerProtectedSecrets(t.Context(), f.Pool, f.host(), f.Computer, "https://api.example.com", []string{selector}); err == nil {
		t.Fatal("stopping owner resolved")
	}
}
func TestCommandPendingTimeoutRetainsIdentity(t *testing.T) {
	f := commandFixture(t)
	c := f.create(t, "timeout")
	id := uuid.UUID(c.ID.Bytes)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET created_at=clock_timestamp()-interval '11 minutes' WHERE environment_id=$1 AND id=$2`, f.Environment, id)
	n, err := ExpirePending(t.Context(), f.Pool, 10)
	if err != nil || n != 1 {
		t.Fatalf("expiry %d %v", n, err)
	}
	c = f.get(t, id)
	if c.Status != "failed" || c.ComputerLeaseEpoch.Valid || c.TerminalReasonCode.String != "computer_command_assignment_timed_out" {
		t.Fatalf("pending timeout %+v", c)
	}
	retry, err := Create(t.Context(), f.Pool, f.request("timeout"))
	if err != nil || retry.ID != c.ID || retry.Status != "failed" {
		t.Fatalf("pending replay %+v %v", retry, err)
	}
	n, err = ExpirePending(t.Context(), f.Pool, 10)
	if err != nil || n != 0 {
		t.Fatalf("repeated expiry %d %v", n, err)
	}
}

// Empty executions still acknowledge one explicit end for each pipe.
func (f fixture) acceptEmptyOutput(t *testing.T, id uuid.UUID) {
	t.Helper()
	bounds := diagnostic.Bounds{ChunkBytes: 16, SourceBytes: 64, SourceRecords: 4, EnvironmentBytes: 128, EnvironmentRecords: 8, QueueBytes: 256, QueueRecords: 16}
	for _, stream := range []string{"stdout", "stderr"} {
		_, err := AppendLog(t.Context(), f.Pool, f.host(), LogChunk{EnvironmentID: f.Environment, CommandID: id, InstanceID: f.Computer, WriterGeneration: 1, Stream: stream, Kind: "end", ObservedSeq: 1, ThroughSequence: 1, ObservedAt: time.Now(), Complete: true}, bounds)
		if err != nil {
			t.Fatal(err)
		}
	}
}
