package session_test

import (
	"bytes"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/computer"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

func runtimeSecretsFixture(t *testing.T) (fixture, session.Execution, *secret.Store, uuid.UUID, int64, uuid.UUID) {
	t.Helper()
	f, _, e := startingSessionFixture(t)
	f.session = e.SessionID
	return runtimeSecretsOnFixture(t, f, e)
}

func runtimeSecretsOnFixture(t *testing.T, f fixture, e session.Execution) (fixture, session.Execution, *secret.Store, uuid.UUID, int64, uuid.UUID) {
	t.Helper()
	var instance uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT s.computer_id,l.allocation_id FROM sessions s JOIN computer_leases l ON l.environment_id=s.environment_id AND l.computer_id=s.computer_id AND l.epoch=$3 WHERE s.environment_id=$1 AND s.id=$2`, f.env, e.SessionID, e.LeaseEpoch).Scan(&f.computer, &instance); err != nil {
		t.Fatal(err)
	}
	store, err := secret.New(db.New(f.pool), f.pool, bytes.Repeat([]byte{17}, 32), auditpolicy.Policy{RetentionSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Create(t.Context(), dbtest.SecretManager(t, f.pool, f.env), f.env, "RUNTIME_SECRET", []byte("v1"), "runtime-secret")
	if err != nil {
		t.Fatal(err)
	}
	secretID := pgvalue.MustUUIDValue(record.ID)
	for _, binding := range []struct{ kind, target, mode, origin string }{{"env", "TOKEN", "protected", "https://api.example.com"}, {"env", "OTHER_TOKEN", "protected", "https://other.example.com"}, {"env", "RAW_TOKEN", "raw", ""}, {"file", "/secrets/token", "raw", ""}} {
		marker, err := secretbinding.Placeholder(binding.mode)
		if err != nil {
			t.Fatal(err)
		}
		origins := []string{}
		if binding.origin != "" {
			origins = append(origins, binding.origin)
		}
		dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO secret_bindings(environment_id,computer_id,secret_id,placement_kind,placement_target,mode,allowed_origins,placeholder) VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''))`, f.env, f.computer, secretID, binding.kind, binding.target, binding.mode, origins, marker)
	}
	trust, err := store.GenerateProxyTrust(f.env, f.computer, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET proxy_ca_certificate=$3,proxy_ca_not_after=$4,proxy_ca_private_key_nonce=$5,proxy_ca_private_key_ciphertext=$6 WHERE environment_id=$1 AND id=$2`, f.env, f.computer, trust.Certificate, trust.NotAfter, trust.PrivateKeyNonce, trust.PrivateKeyCiphertext)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET observed_at=clock_timestamp() WHERE id=$1`, f.worker)
	attachment, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	return f, e, store, secretID, attachment.Sequence, instance
}

// Raw values are pinned per process; protected values resolve the Secret's
// current version per request. Revocation stops provision only, and rotation
// resumes it on the next request without restarting the process.
func TestRuntimeSecretRawPinsAndProtectedCurrentResolution(t *testing.T) {
	f, e, store, secretID, attachment, instance := runtimeSecretsFixture(t)
	manager := dbtest.SecretManager(t, f.pool, f.env)
	first, err := session.RecordSessionStartExposure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), e, attachment)
	if err != nil || len(first.Secrets) != 2 || len(first.ProtectedEnv) != 2 {
		t.Fatalf("first delivery: %+v %v", first, err)
	}
	var exposureAudit bool
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*)=1 FROM audit_events WHERE environment_id=$1 AND target_id=$2 AND action='secret.exposed' AND session_id=$3`, f.env, secretID, e.SessionID).Scan(&exposureAudit); err != nil || !exposureAudit {
		t.Fatalf("new exposure audit: %v %v", exposureAudit, err)
	}
	if _, err = store.Rotate(t.Context(), manager, f.env, secretID, []byte("v2"), ""); err != nil {
		t.Fatal(err)
	}
	retry, err := session.RecordSessionStartExposure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), e, attachment)
	if err != nil {
		t.Fatal(err)
	}
	for i, value := range first.Secrets {
		if value.Mode != "raw" || value.Version != 1 || retry.Secrets[i].VersionID != value.VersionID {
			t.Fatal("retry selected rotated raw value")
		}
	}
	if retry.ProtectedEnv["TOKEN"] != first.ProtectedEnv["TOKEN"] {
		t.Fatal("retry replaced selector")
	}
	peer := f.peer(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='starting' WHERE environment_id=$1 AND session_id=$2`, f.env, peer.session)
	peerExecution := e
	peerExecution.SessionID = peer.session
	peerAttachment, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), peerExecution)
	if err != nil {
		t.Fatal(err)
	}
	second, err := session.RecordSessionStartExposure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), peerExecution, peerAttachment.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range second.Secrets {
		if value.Version != 2 {
			t.Fatal("new peer did not select current raw version")
		}
	}
	if second.ProtectedEnv["TOKEN"] == first.ProtectedEnv["TOKEN"] {
		t.Fatal("shared selector cannot distinguish process owners")
	}
	resolve := func(selectors []string) (map[string][]byte, error) {
		rows, err := computer.CaptureProtectedSecrets(t.Context(), f.pool, *f.host(), instance, "https://api.example.com", selectors)
		if err != nil {
			return nil, err
		}
		return store.OpenProtectedCapture(rows, selectors)
	}
	markers := []string{first.ProtectedEnv["TOKEN"], second.ProtectedEnv["TOKEN"]}
	values, err := resolve(markers)
	if err != nil || string(values[markers[0]]) != "v2" || string(values[markers[1]]) != "v2" {
		t.Fatalf("protected requests did not use the current value: %v", err)
	}
	if _, err = resolve([]string{first.ProtectedEnv["OTHER_TOKEN"]}); !errors.Is(err, computer.ErrDenied) {
		t.Fatalf("cross-origin alias: %v", err)
	}
	// Ending the first process cannot leave its selector authorized by a live peer.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2`, f.env, e.SessionID)
	if _, err = resolve(markers); !errors.Is(err, computer.ErrDenied) {
		t.Fatalf("partial selection leaked an ended owner: %v", err)
	}
	if _, err = resolve(markers[1:]); err != nil {
		t.Fatalf("ending peer denied live owner: %v", err)
	}
	revoked, err := store.Revoke(t.Context(), manager, f.env, secretID, "runtime-revoke")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = resolve(markers[1:]); !errors.Is(err, secret.ErrProtectedRevoked) {
		t.Fatalf("revoked protected request: %v", err)
	}
	if _, err = computer.CaptureProxyTrust(t.Context(), f.pool, *f.host(), instance); err != nil {
		t.Fatalf("revocation denied transport preparation: %v", err)
	}
	identity := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, Epoch: e.LeaseEpoch, InstanceID: instance}
	if _, err = computer.RenewLease(t.Context(), f.pool, *f.host(), identity); err != nil {
		t.Fatalf("revocation stopped lease renewal: %v", err)
	}
	var running bool
	if err = f.pool.QueryRow(t.Context(), `SELECT status='starting' AND fenced_at IS NULL AND failure_recorded_at IS NULL FROM session_processes WHERE environment_id=$1 AND session_id=$2`, f.env, peer.session).Scan(&running); err != nil || !running {
		t.Fatalf("revocation stopped the running process: %v %v", running, err)
	}
	var retained bool
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*)=2 FROM secret_exposures WHERE environment_id=$1 AND secret_id=$2`, f.env, secretID).Scan(&retained); err != nil || !retained {
		t.Fatalf("exposure history: %v %v", retained, err)
	}
	// Rotating the revoked Secret reactivates the same identity and bindings with
	// the next version; the running process resolves it on its next request.
	reactivated, err := store.Rotate(t.Context(), manager, f.env, secretID, []byte("v3"), "")
	if err != nil || reactivated.ID != revoked.ID || reactivated.RevokedAt.Valid {
		t.Fatalf("reactivation: %+v %v", reactivated, err)
	}
	values, err = resolve(markers[1:])
	if err != nil || string(values[markers[1]]) != "v3" {
		t.Fatalf("reactivated protected request: %v", err)
	}
	var bindings int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM secret_bindings WHERE environment_id=$1 AND computer_id=$2 AND secret_id=$3`, f.env, f.computer, secretID).Scan(&bindings); err != nil || bindings != 4 {
		t.Fatalf("reactivation changed bindings: %d %v", bindings, err)
	}
}

// Revoking one Secret leaves protected requests for another Secret on the same
// Computer usable, while new raw delivery of the revoked one is refused.
func TestRuntimeSecretRevocationIsolatesOtherSecrets(t *testing.T) {
	f, e, store, secretID, attachment, instance := runtimeSecretsFixture(t)
	manager := dbtest.SecretManager(t, f.pool, f.env)
	other, err := store.Create(t.Context(), manager, f.env, "OTHER_SECRET", []byte("other-v1"), "")
	if err != nil {
		t.Fatal(err)
	}
	marker, err := secretbinding.Placeholder("protected")
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO secret_bindings(environment_id,computer_id,secret_id,placement_kind,placement_target,mode,allowed_origins,placeholder) VALUES($1,$2,$3,'env','B_TOKEN','protected',ARRAY['https://api.example.com'],$4)`, f.env, f.computer, other.ID, marker)
	started, err := session.RecordSessionStartExposure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), e, attachment)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Revoke(t.Context(), manager, f.env, secretID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = computer.CaptureProxyTrust(t.Context(), f.pool, *f.host(), instance); err != nil {
		t.Fatalf("revoking A denied transport for B: %v", err)
	}
	selectors := []string{started.ProtectedEnv["B_TOKEN"]}
	rows, err := computer.CaptureProtectedSecrets(t.Context(), f.pool, *f.host(), instance, "https://api.example.com", selectors)
	if err != nil {
		t.Fatalf("revoking A denied protected B: %v", err)
	}
	values, err := store.OpenProtectedCapture(rows, selectors)
	if err != nil || string(values[selectors[0]]) != "other-v1" {
		t.Fatalf("protected B value: %v", err)
	}
	if _, err = computer.CaptureProtectedSecrets(t.Context(), f.pool, *f.host(), instance, "https://api.example.com", []string{started.ProtectedEnv["TOKEN"], selectors[0]}); !errors.Is(err, secret.ErrProtectedRevoked) {
		t.Fatalf("request needing revoked A: %v", err)
	}
	peer := f.peer(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='starting' WHERE environment_id=$1 AND session_id=$2`, f.env, peer.session)
	peerExecution := e
	peerExecution.SessionID = peer.session
	peerAttachment, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), peerExecution)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = session.RecordSessionStartExposure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), peerExecution, peerAttachment.Sequence); !errors.Is(err, secret.ErrDeliveryRevoked) {
		t.Fatalf("new raw process with a revoked binding: %v", err)
	}
}

func TestRuntimeSecretExposureRejectsStaleStartupWithoutRecording(t *testing.T) {
	for _, mode := range []string{"attachment", "hold", "revoked", "expired"} {
		t.Run(mode, func(t *testing.T) {
			f, e, store, id, attachment, _ := runtimeSecretsFixture(t)
			switch mode {
			case "attachment":
				attachment++
			case "hold":
				dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind) VALUES($1,$2,$3,'process_failed','system')`, f.env, uuid.NewV7(), e.SessionID)
			case "revoked":
				if _, err := store.Revoke(t.Context(), dbtest.SecretManager(t, f.pool, f.env), f.env, id, "before-delivery"); err != nil {
					t.Fatal(err)
				}
			case "expired":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
			}
			if _, err := session.RecordSessionStartExposure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), e, attachment); err == nil {
				t.Fatal("invalid startup received secrets")
			}
			var count int
			if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM secret_exposures WHERE environment_id=$1 AND session_id=$2`, f.env, e.SessionID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("rejected startup committed exposure: %d %v", count, err)
			}
		})
	}
}

func TestRuntimeSecretPinsSurviveHealthyComputerRestore(t *testing.T) {
	original := newFixture(t)
	dbtest.MustExec(t, t.Context(), original.pool, `UPDATE session_processes SET status='starting' WHERE environment_id=$1 AND session_id=$2`, original.env, original.session)
	f, e, store, id, attachment, sourceInstance := runtimeSecretsOnFixture(t, original, original.execution())
	first, err := session.RecordSessionStartExposure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), e, attachment)
	if err != nil {
		t.Fatal(err)
	}
	if err = session.ObserveSessionReady(t.Context(), f.pool, *f.host(), e, attachment); err != nil {
		t.Fatal(err)
	}
	capture := checkpointStorageForFixture(t, f)
	if err = capture.checkpoints.RegisterCheckpoint(t.Context(), capture.ref, capture.manifest); err != nil {
		t.Fatal(err)
	}
	capture.upload(t)
	if err = capture.publish(t, capture.save, capture.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	if err = capture.checkpoints.CompleteCheckpoint(t.Context(), capture.ref, capture.manifest); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Rotate(t.Context(), dbtest.SecretManager(t, f.pool, f.env), f.env, id, []byte("v2"), "rotate-suspended"); err != nil {
		t.Fatal(err)
	}
	host := *f.host()
	host.HostID = uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO worker_hosts SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$2::uuid,'resource_id','restore-secret-host','current_service_id',$2::uuid))).* FROM worker_hosts h WHERE id=$1`, f.worker, host.HostID)
	restored := &computerRestoreFixture{checkpointStorageFixture: capture, host: host}
	restored.newTarget(t)
	p := restored.prepare(t)
	if err = session.ValidateComputerRestore(t.Context(), f.pool, host, f.env, p, restoreReceipt(p, false, false)); err != nil {
		t.Fatal(err)
	}
	if err = session.CommitComputerRestore(t.Context(), f.pool, host, f.env, p, restoreReceipt(p, true, false)); err != nil {
		t.Fatal(err)
	}
	acknowledgeComputerMembers(t, f, host, p)
	if err = session.CompleteComputerRestore(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, host, f.env, p, restoreReceipt(p, true, true)); err != nil {
		t.Fatal(err)
	}
	selectors := []string{first.ProtectedEnv["TOKEN"]}
	if _, err = computer.CaptureProtectedSecrets(t.Context(), f.pool, *f.host(), sourceInstance, "https://api.example.com", selectors); !errors.Is(err, computer.ErrDenied) {
		t.Fatalf("old source: %v", err)
	}
	rows, err := computer.CaptureProtectedSecrets(t.Context(), f.pool, host, uuid.MustParse(p.Envelope.ComputerInstanceId), "https://api.example.com", selectors)
	if err != nil {
		t.Fatal(err)
	}
	values, err := store.OpenProtectedCapture(rows, selectors)
	if err != nil || string(values[selectors[0]]) != "v2" {
		t.Fatalf("restored process did not resolve the current value: %v", err)
	}
	var retained bool
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*)=1 AND bool_and(process_epoch=1) FROM secret_exposures WHERE environment_id=$1 AND session_id=$2`, f.env, f.session).Scan(&retained); err != nil || !retained {
		t.Fatalf("restore replaced logical exposure: %v", err)
	}
}

func TestRuntimeSecretPinsSelectAgainOnlyForNewLogicalGeneration(t *testing.T) {
	original, allocator, e := startingSessionFixture(t)
	original.session = e.SessionID
	f, e, store, id, attachment, _ := runtimeSecretsOnFixture(t, original, e)
	first, err := session.RecordSessionStartExposure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), e, attachment)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Rotate(t.Context(), dbtest.SecretManager(t, f.pool, f.env), f.env, id, []byte("v2"), "rotate-before-cold"); err != nil {
		t.Fatal(err)
	}
	// Physical absence has been confirmed; the queued work remains for explicit reconstruction.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2 AND epoch=1`, f.env, e.SessionID)
	if _, err = allocator.AllocateSessionProcess(t.Context(), f.env, e.SessionID, 2); err != nil {
		t.Fatal(err)
	}
	e.ProcessEpoch = 2
	next, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	second, err := session.RecordSessionStartExposure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), e, next.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if second.ProtectedEnv["TOKEN"] == first.ProtectedEnv["TOKEN"] {
		t.Fatal("reconstructed process reused old selector")
	}
	for _, value := range second.Secrets {
		if value.Version != 2 {
			t.Fatal("reconstructed process retained superseded version")
		}
	}
	var count int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM secret_exposures WHERE environment_id=$1 AND session_id=$2`, f.env, e.SessionID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("cold reconstruction discarded lineage: %d %v", count, err)
	}
}

func TestRuntimeSecretConcurrentLostAcknowledgementKeepsOnePin(t *testing.T) {
	f, e, store, id, attachment, _ := runtimeSecretsFixture(t)
	first, err := session.RecordSessionStartExposure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), e, attachment)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Rotate(t.Context(), dbtest.SecretManager(t, f.pool, f.env), f.env, id, []byte("v2"), "rotate-lost-ack"); err != nil {
		t.Fatal(err)
	}
	replay := func() error {
		retry, err := session.RecordSessionStartExposure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), e, attachment)
		if err == nil && (retry.ProtectedEnv["TOKEN"] != first.ProtectedEnv["TOKEN"] || retry.Secrets[0].VersionID != first.Secrets[0].VersionID) {
			err = errors.New("concurrent retry changed pin")
		}
		return err
	}
	results := make(chan error, 8)
	for range 8 {
		go func() { results <- replay() }()
	}
	var contended int
	for range 8 {
		if err := <-results; errors.Is(err, session.ErrNotReady) {
			contended++
		} else if err != nil {
			t.Error(err)
		}
	}
	// Concurrent owners may hit the bounded allocation lock wait. Join them
	// before replaying those requests; every successful retry must keep the pin.
	for range contended {
		if err := replay(); err != nil {
			t.Error(err)
		}
	}
	var count int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM secret_exposures WHERE environment_id=$1 AND session_id=$2`, f.env, e.SessionID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retry duplicated exposure: %d %v", count, err)
	}
}

func TestRuntimeProxyTrustPreservesCommittedHostDrainAndHealthPolicies(t *testing.T) {
	for _, mode := range []string{"group-paused", "group-draining", "pool-draining", "host-draining", "admission-paused"} {
		t.Run(mode, func(t *testing.T) {
			f, _, _, _, _, instance := runtimeSecretsFixture(t)
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='acquiring' WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
			if _, err := computer.CaptureProxyTrust(t.Context(), f.pool, *f.host(), instance); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "group-paused":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET status='paused' WHERE id=$1`, f.group)
			case "group-draining":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET status='draining' WHERE id=$1`, f.group)
			case "pool-draining":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_pools SET status='draining' WHERE id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1)`, f.worker)
			case "host-draining":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp(),drain_reason='admin' WHERE id=$1`, f.worker)
			case "admission-paused":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET admission_paused_reason='operator' WHERE id=$1`, f.worker)
			}
			_, err := computer.CaptureProxyTrust(t.Context(), f.pool, *f.host(), instance)
			if mode == "host-draining" {
				if err != nil {
					t.Fatalf("committed acquiring allocation lost trust on Host drain: %v", err)
				}
			} else if !errors.Is(err, computer.ErrDenied) {
				t.Fatalf("acquiring on unavailable supply: %v", err)
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='active' WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
			if _, err := computer.CaptureProxyTrust(t.Context(), f.pool, *f.host(), instance); err != nil {
				t.Fatalf("active continuation blocked: %v", err)
			}
		})
	}
}
