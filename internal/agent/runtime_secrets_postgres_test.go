package agent

import (
	"bytes"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

func runtimeSecretsFixture(t *testing.T) (fixture, Execution, *secret.Store, uuid.UUID, int64, uuid.UUID) {
	t.Helper()
	f, _, e := startingSessionFixture(t)
	f.session = e.SessionID
	return runtimeSecretsOnFixture(t, f, e)
}

func runtimeSecretsOnFixture(t *testing.T, f fixture, e Execution) (fixture, Execution, *secret.Store, uuid.UUID, int64, uuid.UUID) {
	t.Helper()
	var instance uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT s.computer_id,l.computer_instance_id FROM sessions s JOIN computer_leases l ON l.environment_id=s.environment_id AND l.computer_id=s.computer_id AND l.epoch=$3 WHERE s.environment_id=$1 AND s.id=$2`, f.env, e.SessionID, e.LeaseEpoch).Scan(&f.computer, &instance); err != nil {
		t.Fatal(err)
	}
	store, err := secret.New(db.New(f.pool), f.pool, bytes.Repeat([]byte{17}, 32))
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Create(t.Context(), f.env, "RUNTIME_SECRET", []byte("v1"), "runtime-secret")
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
		dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computer_secret_bindings(environment_id,computer_id,secret_id,placement_kind,placement_target,mode,allowed_origins,placeholder) VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''))`, f.env, f.computer, secretID, binding.kind, binding.target, binding.mode, origins, marker)
	}
	trust, err := store.GenerateProxyTrust(f.env, f.computer, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET proxy_ca_certificate=$3,proxy_ca_not_after=$4,proxy_ca_private_key_nonce=$5,proxy_ca_private_key_ciphertext=$6 WHERE environment_id=$1 AND id=$2`, f.env, f.computer, trust.Certificate, trust.NotAfter, trust.PrivateKeyNonce, trust.PrivateKeyCiphertext)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET observed_at=clock_timestamp() WHERE id=$1`, f.worker)
	attachment, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	return f, e, store, secretID, attachment.Sequence, instance
}

func TestRuntimeSecretPinsSurviveRetryAndCoexistWithNewPeers(t *testing.T) {
	f, e, store, secretID, attachment, instance := runtimeSecretsFixture(t)
	first, err := RecordSessionStartExposure(t.Context(), f.pool, *f.host(), e, attachment)
	if err != nil || len(first.Secrets) != 4 || len(first.ProtectedEnv) != 2 {
		t.Fatalf("first delivery: %+v %v", first, err)
	}
	if _, err = store.Rotate(t.Context(), f.env, secretID, []byte("v2"), "runtime-rotate"); err != nil {
		t.Fatal(err)
	}
	retry, err := RecordSessionStartExposure(t.Context(), f.pool, *f.host(), e, attachment)
	if err != nil {
		t.Fatal(err)
	}
	for i, value := range first.Secrets {
		if value.Version != 1 || retry.Secrets[i].VersionID != value.VersionID {
			t.Fatal("retry selected rotated value")
		}
	}
	if retry.ProtectedEnv["TOKEN"] != first.ProtectedEnv["TOKEN"] {
		t.Fatal("retry replaced selector")
	}
	peer := f.peer(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='starting' WHERE environment_id=$1 AND session_id=$2`, f.env, peer.session)
	peerExecution := e
	peerExecution.SessionID = peer.session
	peerAttachment, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), peerExecution)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RecordSessionStartExposure(t.Context(), f.pool, *f.host(), peerExecution, peerAttachment.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range second.Secrets {
		if value.Version != 2 {
			t.Fatal("new peer did not select current version")
		}
	}
	if second.ProtectedEnv["TOKEN"] == first.ProtectedEnv["TOKEN"] {
		t.Fatal("shared selector cannot distinguish process pins")
	}
	resolve := func(selectors []string) (map[string][]byte, error) {
		rows, err := CaptureComputerProtectedSecrets(t.Context(), f.pool, *f.host(), instance, "https://api.example.com", selectors)
		if err != nil {
			return nil, err
		}
		return store.OpenProtectedCapture(rows, selectors)
	}
	markers := []string{first.ProtectedEnv["TOKEN"], second.ProtectedEnv["TOKEN"]}
	values, err := resolve(markers)
	if err != nil || string(values[markers[0]]) != "v1" || string(values[markers[1]]) != "v2" {
		t.Fatalf("simultaneous pins: %v", err)
	}
	if _, err = resolve([]string{first.ProtectedEnv["OTHER_TOKEN"]}); !errors.Is(err, ErrDenied) {
		t.Fatalf("cross-origin alias: %v", err)
	}
	// Ending the first process cannot leave its selector authorized by a live peer.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2`, f.env, e.SessionID)
	if _, err = resolve(markers); !errors.Is(err, ErrDenied) {
		t.Fatalf("partial selection leaked an ended pin: %v", err)
	}
	if _, err = resolve(markers[1:]); err != nil {
		t.Fatalf("ending peer revoked live pin: %v", err)
	}
	if _, err = store.Revoke(t.Context(), f.env, secretID, "runtime-revoke"); err != nil {
		t.Fatal(err)
	}
	if _, err = resolve(markers[1:]); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked pin: %v", err)
	}
	var retained bool
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*)=2 AND bool_and(revocation_generation=0) FROM secret_exposures WHERE environment_id=$1 AND secret_id=$2`, f.env, secretID).Scan(&retained); err != nil || !retained {
		t.Fatalf("exposure history: %v %v", retained, err)
	}
	if _, err = AuthorizeSessionStart(t.Context(), f.pool, *f.host(), peerExecution, peerAttachment.Sequence); !errors.Is(err, ErrNotReady) {
		t.Fatalf("revoked final release: %v", err)
	}
	var quarantine bool
	if err = f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM computer_secret_revocations WHERE environment_id=$1 AND computer_id=$2)`, f.env, f.computer).Scan(&quarantine); err != nil || !quarantine {
		t.Fatalf("ended/live lineage missing: %v %v", quarantine, err)
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
				dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$2,$3,'local','test')`, f.env, uuid.NewV7(), e.SessionID)
			case "revoked":
				if _, err := store.Revoke(t.Context(), f.env, id, "before-delivery"); err != nil {
					t.Fatal(err)
				}
			case "expired":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
			}
			if _, err := RecordSessionStartExposure(t.Context(), f.pool, *f.host(), e, attachment); err == nil {
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
	first, err := RecordSessionStartExposure(t.Context(), f.pool, *f.host(), e, attachment)
	if err != nil {
		t.Fatal(err)
	}
	if err = ObserveSessionReady(t.Context(), f.pool, *f.host(), e, attachment); err != nil {
		t.Fatal(err)
	}
	capture := checkpointStorageForFixture(t, f)
	if err = capture.publisher.RegisterCheckpoint(t.Context(), capture.ref, capture.manifest); err != nil {
		t.Fatal(err)
	}
	capture.upload(t)
	if err = capture.publish(t, capture.save, capture.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	if err = capture.publisher.CompleteCheckpoint(t.Context(), capture.ref, capture.manifest); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Rotate(t.Context(), f.env, id, []byte("v2"), "rotate-suspended"); err != nil {
		t.Fatal(err)
	}
	host := *f.host()
	host.HostID = uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO worker_hosts SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$2::uuid,'resource_id','restore-secret-host','current_service_id',$2::uuid))).* FROM worker_hosts h WHERE id=$1`, f.worker, host.HostID)
	restored := &computerRestoreFixture{checkpointStorageFixture: capture, host: host}
	restored.newTarget(t)
	p := restored.prepare(t)
	if err = ValidateComputerRestore(t.Context(), f.pool, host, f.env, p, restoreReceipt(p, false, false)); err != nil {
		t.Fatal(err)
	}
	if err = CommitComputerRestore(t.Context(), f.pool, host, f.env, p, restoreReceipt(p, true, false)); err != nil {
		t.Fatal(err)
	}
	acknowledgeComputerMembers(t, f, host, p)
	if err = CompleteComputerRestore(t.Context(), f.pool, host, f.env, p, restoreReceipt(p, true, true)); err != nil {
		t.Fatal(err)
	}
	selectors := []string{first.ProtectedEnv["TOKEN"]}
	if _, err = CaptureComputerProtectedSecrets(t.Context(), f.pool, *f.host(), sourceInstance, "https://api.example.com", selectors); !errors.Is(err, ErrDenied) {
		t.Fatalf("old source: %v", err)
	}
	rows, err := CaptureComputerProtectedSecrets(t.Context(), f.pool, host, uuid.MustParse(p.Envelope.ComputerInstanceId), "https://api.example.com", selectors)
	if err != nil {
		t.Fatal(err)
	}
	values, err := store.OpenProtectedCapture(rows, selectors)
	if err != nil || string(values[selectors[0]]) != "v1" {
		t.Fatalf("restored process lost pin: %v", err)
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
	first, err := RecordSessionStartExposure(t.Context(), f.pool, *f.host(), e, attachment)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Rotate(t.Context(), f.env, id, []byte("v2"), "rotate-before-cold"); err != nil {
		t.Fatal(err)
	}
	// Physical absence has been confirmed; the queued work remains for explicit reconstruction.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2 AND epoch=1`, f.env, e.SessionID)
	if _, err = allocator.AllocateSessionProcess(t.Context(), f.env, e.SessionID, 2); err != nil {
		t.Fatal(err)
	}
	e.ProcessEpoch = 2
	next, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RecordSessionStartExposure(t.Context(), f.pool, *f.host(), e, next.Sequence)
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
	first, err := RecordSessionStartExposure(t.Context(), f.pool, *f.host(), e, attachment)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Rotate(t.Context(), f.env, id, []byte("v2"), "rotate-lost-ack"); err != nil {
		t.Fatal(err)
	}
	replay := func() error {
		retry, err := RecordSessionStartExposure(t.Context(), f.pool, *f.host(), e, attachment)
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
		if err := <-results; errors.Is(err, ErrNotReady) {
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
	for _, mode := range []string{"group-paused", "group-draining", "pool-draining", "host-draining", "run-paused", "vm-paused"} {
		t.Run(mode, func(t *testing.T) {
			f, _, _, _, _, instance := runtimeSecretsFixture(t)
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='acquiring' WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
			if _, err := CaptureComputerProxyTrust(t.Context(), f.pool, *f.host(), instance); err != nil {
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
			case "run-paused":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET run_paused_reason='operator' WHERE id=$1`, f.worker)
			case "vm-paused":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET vm_paused_reason='operator' WHERE id=$1`, f.worker)
			}
			_, err := CaptureComputerProxyTrust(t.Context(), f.pool, *f.host(), instance)
			if mode == "host-draining" {
				if err != nil {
					t.Fatalf("committed acquiring allocation lost trust on Host drain: %v", err)
				}
			} else if !errors.Is(err, ErrDenied) {
				t.Fatalf("acquiring on unavailable supply: %v", err)
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='active' WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
			if _, err := CaptureComputerProxyTrust(t.Context(), f.pool, *f.host(), instance); err != nil {
				t.Fatalf("active continuation blocked: %v", err)
			}
		})
	}
}
