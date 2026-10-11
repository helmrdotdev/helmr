package agent

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

type computerRestoreFixture struct {
	checkpointStorageFixture
	host       workergroup.HostPrincipal
	epoch      int64
	credential string
}

func readyCheckpointFixture(t *testing.T) checkpointStorageFixture {
	t.Helper()
	f := newCheckpointStorageFixture(t)
	ctx := t.Context()
	if err := f.publisher.RegisterCheckpoint(ctx, f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	f.upload(t)
	if err := f.publish(t, f.save, f.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	if err := f.publisher.CompleteCheckpoint(ctx, f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	return f
}

func newComputerRestoreFixture(t *testing.T) *computerRestoreFixture {
	t.Helper()
	f := readyCheckpointFixture(t)
	return restoreCheckpointFixture(t, f)
}

func restoreCheckpointFixture(t *testing.T, f checkpointStorageFixture) *computerRestoreFixture {
	t.Helper()
	ctx := t.Context()
	host := *f.f.host()
	host.HostID = uuid.NewV7()
	dbtest.MustExec(t, ctx, f.f.pool, `INSERT INTO worker_hosts SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$2::uuid,'resource_id','restore-host','current_service_id',$2::uuid))).* FROM worker_hosts h WHERE id=$1`, f.f.worker, host.HostID)
	r := &computerRestoreFixture{checkpointStorageFixture: f, host: host}
	r.newTarget(t)
	return r
}
func (r *computerRestoreFixture) newTarget(t *testing.T) {
	t.Helper()
	r.epoch++
	if r.epoch < 2 {
		r.epoch = 2
	}
	r.credential = uuid.NewV7().String()
	digest := sha256.Sum256([]byte(r.credential))
	// Acquisition/fencing and capacity are fixture setup, not a VM or allocator proof.
	dbtest.MustExec(t, t.Context(), r.f.pool, `UPDATE computer_leases SET status='released',fenced_at=clock_timestamp(),fence_evidence='owned physical closure' WHERE environment_id=$1 AND computer_id=$2 AND fenced_at IS NULL;
 INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,computer_instance_id,channel_credential_digest,restored_from_save_id,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at) VALUES($1,$2,$3,$4,1,clock_timestamp()+interval '1 hour',$5,$6,$7,'acquiring',1000,536870912,1073741824,'sha256:1111111111111111111111111111111111111111111111111111111111111111',1,'sha256:1111111111111111111111111111111111111111111111111111111111111111',clock_timestamp(),NULL)`, pgx.QueryExecModeSimpleProtocol, r.f.env, r.f.computer, r.epoch, r.host.HostID, uuid.NewV7(), digest[:], r.save)
	if _, err := BindComputerLeaseDisk(t.Context(), r.f.pool, r.host, r.f.env, r.f.computer, r.epoch, uuid.MustParse(r.writer.ActiveKey)); err != nil {
		t.Fatal(err)
	}
}
func (r *computerRestoreFixture) prepare(t *testing.T) *agentv1.ComputerSessionInstallation {
	t.Helper()
	p, err := PrepareComputerRestore(t.Context(), r.f.pool, r.host, r.f.env, r.manifest.CheckpointID, r.epoch, r.credential)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func (r *computerRestoreFixture) execution() Execution {
	e := r.f.execution()
	e.WorkerHostID = r.host.HostID
	e.LeaseEpoch = r.epoch
	e.AuthorityGeneration++
	return e
}
func restoreReceipt(p *agentv1.ComputerSessionInstallation, installed, activated bool) *agentv1.ComputerSessionReceipt {
	r := abortReceipt(p, installed, activated)
	r.Frozen = !activated
	return r
}
func TestComputerRestoreInstallsControlBeforeDispatch(t *testing.T) {
	f := newComputerRestoreFixture(t)
	ctx := t.Context()
	e := f.execution()
	if _, err := AcquireRuntimeAttachment(ctx, f.f.pool, f.host, e); err == nil {
		t.Fatal("unclaimed acquiring target attached")
	}
	p := f.prepare(t)
	if p.SourceAbort || p.Envelope.ComputerInstanceId == f.manifest.InstanceID.String() || p.DesiredVersion <= p.Capture.DesiredVersion || p.BaseComputerDiskVersionId != f.save.String() {
		t.Fatal("target identity did not advance")
	}
	for _, g := range p.Grants {
		if g.AuthorityGeneration != 2 || g.ComputerLeaseEpoch != 2 {
			t.Fatal("stale grant")
		}
	}
	if _, err := Dispatch(ctx, f.f.pool, e); !errors.Is(err, ErrNotReady) {
		t.Fatalf("acquiring dispatch: %v", err)
	}
	caller := Caller{Kind: "session", ID: f.f.session, Execution: e, Host: &f.host}
	if _, err := Enqueue(ctx, f.f.pool, caller, EnqueueRequest{EnvironmentID: f.f.env, SessionID: f.f.session, RetryKey: "acquiring-mutation", Input: []byte(`[]`)}); !errors.Is(err, ErrDenied) {
		t.Fatalf("control authority admitted business mutation: %v", err)
	}
	attachment, err := AcquireRuntimeAttachment(ctx, f.f.pool, f.host, e)
	if err != nil || attachment.Authority.Generation != 2 {
		t.Fatalf("target control attachment: %+v %v", attachment, err)
	}
	if _, err = AcquireRuntimeAttachment(ctx, f.f.pool, *f.f.host(), f.f.execution()); err == nil {
		t.Fatal("old source still attached")
	}
	initial := restoreReceipt(p, false, false)
	if err = ValidateComputerRestore(ctx, f.f.pool, f.host, f.f.env, p, initial); err != nil {
		t.Fatal(err)
	}
	if err = CommitComputerRestore(ctx, f.f.pool, f.host, f.f.env, p, initial); !errors.Is(err, ErrNotReady) {
		t.Fatalf("uninstalled consumed: %v", err)
	}
	installed := restoreReceipt(p, true, false)
	if err = CommitComputerRestore(ctx, f.f.pool, f.host, f.f.env, p, installed); err != nil {
		t.Fatal(err)
	}
	if err = ValidateComputerRestore(ctx, f.f.pool, f.host, f.f.env, p, initial); !errors.Is(err, ErrNotReady) {
		t.Fatalf("old image retried: %v", err)
	}
	if err = LoseUnreadyComputerCapture(ctx, f.f.pool, f.f.env, f.manifest.CheckpointID); !errors.Is(err, ErrNotReady) {
		t.Fatalf("historical source fence lost current target: %v", err)
	}
	admitted := f.f.enqueue(t, "queued-through-restore")
	if err = CompleteComputerRestore(ctx, f.f.pool, f.host, f.f.env, p, installed); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unactivated admitted: %v", err)
	}
	// Current controls are mandatory even after the activation reply is received.
	dbtest.MustExec(t, ctx, f.f.pool, `UPDATE sessions SET authority_generation=3 WHERE environment_id=$1 AND id=$2`, f.f.env, f.f.session)
	if err = CompleteComputerRestore(ctx, f.f.pool, f.host, f.f.env, p, restoreReceipt(p, true, true)); !errors.Is(err, ErrNotReady) {
		t.Fatalf("stale controls admitted: %v", err)
	}
	acknowledgeComputerMembers(t, f.f, f.host, p)
	for range 2 {
		if err = CompleteComputerRestore(ctx, f.f.pool, f.host, f.f.env, p, restoreReceipt(p, true, true)); err != nil {
			t.Fatal(err)
		}
	}
	e.AuthorityGeneration = 3
	dispatched, err := Dispatch(ctx, f.f.pool, e)
	if err != nil || dispatched.TurnID != admitted.TurnID {
		t.Fatalf("restored FIFO: %+v %v", dispatched, err)
	}
	var scrubbed bool
	if err = f.f.pool.QueryRow(ctx, `SELECT capture_request IS NULL FROM computer_checkpoints WHERE id=$1`, f.manifest.CheckpointID).Scan(&scrubbed); err != nil || !scrubbed {
		t.Fatal("retained secret capture")
	}
}
func TestComputerRestoreRetriesOnlyBeforeConsumption(t *testing.T) {
	f := newComputerRestoreFixture(t)
	ctx := t.Context()
	first := f.prepare(t)
	if err := ValidateComputerRestore(ctx, f.f.pool, f.host, f.f.env, first, restoreReceipt(first, false, false)); err != nil {
		t.Fatal(err)
	}
	f.newTarget(t)
	second := f.prepare(t)
	if second.DesiredVersion <= first.DesiredVersion || second.Envelope.ComputerInstanceId == first.Envelope.ComputerInstanceId {
		t.Fatal("physical attempt identity reused")
	}
	if err := ValidateComputerRestore(ctx, f.f.pool, f.host, f.f.env, first, restoreReceipt(first, false, false)); err == nil {
		t.Fatal("fenced attempt validated")
	}
	if err := ValidateComputerRestore(ctx, f.f.pool, f.host, f.f.env, second, restoreReceipt(second, false, false)); err != nil {
		t.Fatal(err)
	}
	if err := CommitComputerRestore(ctx, f.f.pool, f.host, f.f.env, second, restoreReceipt(second, true, false)); err != nil {
		t.Fatal(err)
	}
	f.newTarget(t)
	if _, err := PrepareComputerRestore(ctx, f.f.pool, f.host, f.f.env, f.manifest.CheckpointID, f.epoch, f.credential); !errors.Is(err, ErrNotReady) {
		t.Fatalf("consumed snapshot reloaded: %v", err)
	}
	// Loss follows the consumed physical owner, not the newly admitted fixture lease.
	if err := LoseUnreadyComputerCapture(ctx, f.f.pool, f.f.env, f.manifest.CheckpointID); err != nil {
		t.Fatal(err)
	}
	var holds int
	if err := f.f.pool.QueryRow(ctx, `SELECT count(*) FROM session_holds WHERE environment_id=$1 AND released_at IS NULL`, f.f.env).Scan(&holds); err != nil || holds != len(second.Grants) {
		t.Fatalf("loss holds=%d %v", holds, err)
	}
}
func TestComputerRestoreRejectsChangedInstallation(t *testing.T) {
	for _, which := range []string{"credential", "base", "version", "member", "instance", "expired", "unfrozen", "activation", "foreign"} {
		t.Run(which, func(t *testing.T) {
			f := newComputerRestoreFixture(t)
			p := f.prepare(t)
			observed := restoreReceipt(p, false, false)
			host := f.host
			switch which {
			case "credential":
				p.Envelope.ChannelCredential = "wrong"
			case "base":
				p.BaseComputerDiskVersionId = uuid.NewV7().String()
			case "version":
				p.DesiredVersion++
			case "member":
				p.Grants[0].Identity.ProcessEpoch++
			case "instance":
				p.Envelope.ComputerInstanceId = uuid.NewV7().String()
			case "expired":
				p.Envelope.OperationExpiresAtUnixNano = time.Now().Add(-time.Second).UnixNano()
			case "unfrozen":
				observed.Frozen = false
			case "activation":
				observed.Installed = true
				observed.ActivationStarted = true
				observed.DesiredVersion = p.DesiredVersion
			case "foreign":
				host = *f.f.host()
			}
			if err := ValidateComputerRestore(t.Context(), f.f.pool, host, f.f.env, p, observed); err == nil {
				t.Fatal("invalid installation accepted")
			}
		})
	}
}
func TestComputerRestoreRetainsStopsAndHolds(t *testing.T) {
	f := newComputerRestoreFixture(t)
	ctx := t.Context()
	peer := f.manifest.Members[1].SessionID
	if peer == f.f.session {
		peer = f.manifest.Members[0].SessionID
	}
	dbtest.MustExec(t, ctx, f.f.pool, `UPDATE sessions SET status='cancelled',authority_generation=authority_generation+1 WHERE environment_id=$1 AND id=$2`, f.f.env, peer)
	dbtest.MustExec(t, ctx, f.f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2`, f.f.env, peer)
	dbtest.MustExec(t, ctx, f.f.pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$2,$3,'local','explicit hold')`, f.f.env, uuid.NewV7(), f.f.session)
	p := f.prepare(t)
	if len(p.StoppedSessions) != 1 || p.StoppedSessions[0].SessionId != peer.String() {
		t.Fatal("cancelled captured member was not stopped")
	}
	if err := ValidateComputerRestore(ctx, f.f.pool, f.host, f.f.env, p, restoreReceipt(p, false, false)); err != nil {
		t.Fatal(err)
	}
	if err := CommitComputerRestore(ctx, f.f.pool, f.host, f.f.env, p, restoreReceipt(p, true, false)); err != nil {
		t.Fatal(err)
	}
	acknowledgeComputerMembers(t, f.f, f.host, p)
	if err := CompleteComputerRestore(ctx, f.f.pool, f.host, f.f.env, p, restoreReceipt(p, true, true)); err != nil {
		t.Fatal(err)
	}
	var held bool
	if err := f.f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM session_holds WHERE environment_id=$1 AND session_id=$2 AND released_at IS NULL)`, f.f.env, f.f.session).Scan(&held); err != nil || !held {
		t.Fatal("restore released user hold")
	}
}
func TestRuntimeControlRenewalSurvivesDeploymentRevocation(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.pool, `UPDATE deployments SET execution_revoked_at=clock_timestamp() WHERE environment_id=$1`, f.env)
	if _, err := RenewRuntimeAuthority(ctx, f.pool, *f.host(), f.execution()); err != nil {
		t.Fatalf("revocation blocked control delivery: %v", err)
	}
	if _, err := Dispatch(ctx, f.pool, f.execution()); !errors.Is(err, ErrNotReady) {
		t.Fatalf("revoked business admission: %v", err)
	}
}

func TestComputerRestoreRejectsChangedPhysicalMembership(t *testing.T) {
	for _, which := range []string{"added", "fenced", "stale-generation", "stale-terminal-set", "foreign-invalid-identity"} {
		t.Run(which, func(t *testing.T) {
			f := newComputerRestoreFixture(t)
			p := f.prepare(t)
			ctx := t.Context()
			host := f.host
			switch which {
			case "added":
				f.f.peer(t)
			case "fenced":
				dbtest.MustExec(t, ctx, f.f.pool, `UPDATE session_processes SET fenced_at=clock_timestamp(),status='lost' WHERE environment_id=$1 AND session_id=$2`, f.f.env, f.f.session)
			case "stale-generation":
				dbtest.MustExec(t, ctx, f.f.pool, `UPDATE sessions SET authority_generation=authority_generation+1 WHERE id=$1`, f.f.session)
			case "stale-terminal-set":
				dbtest.MustExec(t, ctx, f.f.pool, `UPDATE sessions SET status='cancelled' WHERE id=$1`, f.f.session)
			case "foreign-invalid-identity":
				host = *f.f.host()
				p.Envelope.WriterGeneration = 99
				p.DesiredVersion++
			}
			if err := ValidateComputerRestore(ctx, f.f.pool, host, f.f.env, p, restoreReceipt(p, false, false)); err == nil {
				t.Fatal("changed ownership admitted")
			}
		})
	}
}

func TestComputerRestorePreparationRejectsWrongTarget(t *testing.T) {
	for _, which := range []string{"active", "credential", "source-credential", "wrong-save", "pending-save"} {
		t.Run(which, func(t *testing.T) {
			f := newComputerRestoreFixture(t)
			ctx := t.Context()
			credential := f.credential
			switch which {
			case "active":
				dbtest.MustExec(t, ctx, f.f.pool, `UPDATE computer_leases SET status='active',initialized_at=clock_timestamp() WHERE computer_id=$1 AND epoch=$2`, f.f.computer, f.epoch)
			case "credential":
				credential = "wrong"
			case "source-credential":
				credential = "test-computer-channel"
				digest := sha256.Sum256([]byte(credential))
				dbtest.MustExec(t, ctx, f.f.pool, `UPDATE computer_leases SET channel_credential_digest=$3 WHERE computer_id=$1 AND epoch=$2`, f.f.computer, f.epoch, digest[:])
			case "wrong-save":
				dbtest.MustExec(t, ctx, f.f.pool, `UPDATE computer_leases l SET base_root_id=c.initial_root_id FROM computers c WHERE l.computer_id=$1 AND l.epoch=$2 AND (c.environment_id,c.id)=(l.environment_id,l.computer_id)`, f.f.computer, f.epoch)
			case "pending-save":
				dbtest.MustExec(t, ctx, f.f.pool, `INSERT INTO computer_saves(environment_id,id,computer_id,seq,computer_lease_epoch,status) VALUES($1,$2,$3,99,1,'requested')`, f.f.env, uuid.NewV7(), f.f.computer)
			}
			if _, err := PrepareComputerRestore(ctx, f.f.pool, f.host, f.f.env, f.manifest.CheckpointID, f.epoch, credential); err == nil {
				t.Fatal("invalid target prepared")
			}
		})
	}
}

func TestComputerRestoreRefreshesOnlyUninstalledTransportGrants(t *testing.T) {
	f := newComputerRestoreFixture(t)
	ctx := t.Context()
	p := f.prepare(t)
	p.Envelope.OperationExpiresAtUnixNano = time.Now().Add(-time.Second).UnixNano()
	if err := ValidateComputerRestore(ctx, f.f.pool, f.host, f.f.env, p, restoreReceipt(p, false, false)); !errors.Is(err, ErrNotReady) {
		t.Fatalf("expired uninstalled grant: %v", err)
	}
	p = f.prepare(t)
	p.Envelope.OperationExpiresAtUnixNano = time.Now().Add(500 * time.Millisecond).UnixNano()
	for _, g := range p.Grants {
		g.ExpiresAtUnixNano = p.Envelope.OperationExpiresAtUnixNano
	}
	if err := ValidateComputerRestore(ctx, f.f.pool, f.host, f.f.env, p, restoreReceipt(p, false, false)); err != nil {
		t.Fatal(err)
	}
	// The physical host retained this exact request after successful installation.
	// Expiring transport fields do not change its durable identity.
	time.Sleep(550 * time.Millisecond)
	if err := CommitComputerRestore(ctx, f.f.pool, f.host, f.f.env, p, restoreReceipt(p, true, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := RenewRuntimeAuthority(ctx, f.f.pool, f.host, f.execution()); err != nil {
		t.Fatal(err)
	}
	if err := ValidateComputerRestore(ctx, f.f.pool, f.host, f.f.env, p, restoreReceipt(p, false, false)); !errors.Is(err, ErrNotReady) {
		t.Fatalf("consumed image refreshed as uninstalled: %v", err)
	}
}

func TestComputerRestorePreparationPreservesProcessFence(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "first-target", true: "new-target"}[retry], func(t *testing.T) {
			f := newComputerRestoreFixture(t)
			ctx := t.Context()
			if retry {
				f.prepare(t)
				f.newTarget(t)
			}
			dbtest.MustExec(t, ctx, f.f.pool, `UPDATE session_processes SET fenced_at=clock_timestamp(),status='lost' WHERE environment_id=$1 AND session_id=$2`, f.f.env, f.f.session)
			p, err := PrepareComputerRestore(ctx, f.f.pool, f.host, f.f.env, f.manifest.CheckpointID, f.epoch, f.credential)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.StoppedSessions) != 1 || p.StoppedSessions[0].SessionId != f.f.session.String() {
				t.Fatal("fenced process admitted to resume")
			}
			var fenced bool
			if err := f.f.pool.QueryRow(ctx, `SELECT fenced_at IS NOT NULL AND status='lost' FROM session_processes WHERE environment_id=$1 AND session_id=$2`, f.f.env, f.f.session).Scan(&fenced); err != nil || !fenced {
				t.Fatalf("process fence lost: %v", err)
			}
		})
	}
}

func TestComputerRestoreRefreshesStopsBeforeInstallation(t *testing.T) {
	f := newComputerRestoreFixture(t)
	ctx := t.Context()
	first := f.prepare(t)
	if err := ValidateComputerRestore(ctx, f.f.pool, f.host, f.f.env, first, restoreReceipt(first, false, false)); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, f.f.pool, `UPDATE sessions SET status='cancelled',authority_generation=authority_generation+1 WHERE environment_id=$1 AND id=$2`, f.f.env, f.f.session)
	if err := ValidateComputerRestore(ctx, f.f.pool, f.host, f.f.env, first, restoreReceipt(first, false, false)); !errors.Is(err, ErrNotReady) {
		t.Fatalf("stale controls validated: %v", err)
	}
	second := f.prepare(t)
	if len(second.StoppedSessions) != 1 || second.StoppedSessions[0].SessionId != f.f.session.String() {
		t.Fatal("new stop missing")
	}
	if err := ValidateComputerRestore(ctx, f.f.pool, f.host, f.f.env, second, restoreReceipt(second, false, false)); err != nil {
		t.Fatal(err)
	}
	if err := CommitComputerRestore(ctx, f.f.pool, f.host, f.f.env, first, restoreReceipt(first, true, false)); !errors.Is(err, ErrConflict) {
		t.Fatalf("replaced installation consumed: %v", err)
	}
	if err := CommitComputerRestore(ctx, f.f.pool, f.host, f.f.env, second, restoreReceipt(second, true, false)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateComputerRestore(ctx, f.f.pool, f.host, f.f.env, first, restoreReceipt(first, true, false)); !errors.Is(err, ErrConflict) {
		t.Fatalf("installed identity replaced: %v", err)
	}
	acknowledgeComputerMembers(t, f.f, f.host, second)
	if err := CompleteComputerRestore(ctx, f.f.pool, f.host, f.f.env, second, restoreReceipt(second, true, true)); err != nil {
		t.Fatal(err)
	}
}
