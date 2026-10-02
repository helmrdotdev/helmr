package workergroup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

// startedHost is a worker host enrolled and brought into service through the
// credential and lifecycle operations, as a worker process does.
type startedHost struct {
	enrolled   EnrolledHost
	serviceID  string
	credential HostCredential
	principal  HostPrincipal
}

func (f supplyFixture) enroll(t *testing.T, cfg HostAuthConfig, poolName string, resourceID string) EnrolledHost {
	t.Helper()
	tokenHash, err := auth.ParseEnrollmentToken(f.enrollmentToken)
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := EnrollHost(t.Context(), f.q, cfg, Enrollment{TokenHash: tokenHash, PoolName: poolName, ResourceID: resourceID})
	if err != nil {
		t.Fatal(err)
	}
	if enrolled.GroupID != f.groupID() {
		t.Fatalf("enrolled group = %s, want %s", enrolled.GroupID, f.groupID())
	}
	return enrolled
}

func (f supplyFixture) exchange(t *testing.T, cfg HostAuthConfig, enrolled EnrolledHost, serviceID string) HostCredential {
	t.Helper()
	credential, err := IssueHostCredential(t.Context(), f.q, cfg, HostCredentialRequest{
		HostID: enrolled.HostID.String(), Secret: enrolled.Secret, ServiceID: serviceID,
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

func (f supplyFixture) authenticate(t *testing.T, authenticate func(context.Context, db.Querier, HostAuthConfig, string, time.Time) (HostPrincipal, error), cfg HostAuthConfig, credential HostCredential) HostPrincipal {
	t.Helper()
	principal, err := authenticate(t.Context(), f.q, cfg, credential.Value, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return principal
}

// hostState reads a worker host's status and claim version.
func (f supplyFixture) hostState(t *testing.T, hostID uuid.UUID) (db.WorkerHostStatus, int64) {
	t.Helper()
	var status db.WorkerHostStatus
	var claim int64
	if err := f.pool.QueryRow(t.Context(), `SELECT status, claim_version FROM worker_hosts WHERE id = $1`, hostID).Scan(&status, &claim); err != nil {
		t.Fatal(err)
	}
	return status, claim
}

func startupEvidence(t *testing.T) []byte {
	t.Helper()
	evidence, err := json.Marshal(map[string]any{
		"inventory_complete": true, "inventory_scope": "worker_instance_state_roots_v0",
		"observed_at": time.Now().UTC(), "inventory": []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}

// start enrolls a host in the named pool, exchanges its secret, records its
// startup recovery, activates it with template and re-authenticates it for
// its ordinary routes.
func (f supplyFixture) start(t *testing.T, cfg HostAuthConfig, poolName string, resourceID string, template Template) startedHost {
	t.Helper()
	host := startedHost{enrolled: f.enroll(t, cfg, poolName, resourceID), serviceID: uuid.NewV7().String()}
	host.credential = f.exchange(t, cfg, host.enrolled, host.serviceID)
	recovering := f.authenticate(t, AuthenticateRecoveringHost, cfg, host.credential)
	if err := RecordStartupRecovery(t.Context(), f.q, recovering, startupEvidence(t)); err != nil {
		t.Fatal(err)
	}
	activating := f.authenticate(t, AuthenticateActivatingHost, cfg, host.credential)
	if err := ActivateHost(t.Context(), f.pool, activating, Activation{
		Template: template, CPUEnvironment: []byte(`{"vendor":"test"}`), CPUEnvironmentDigest: f.cpuConfigDigest,
	}); err != nil {
		t.Fatal(err)
	}
	// Activation can advance the group claim version by selecting the initial
	// primary pool, so the host exchanges its secret again.
	host.credential = f.exchange(t, cfg, host.enrolled, host.serviceID)
	host.principal = f.authenticate(t, AuthenticateHost, cfg, host.credential)
	return host
}

func TestHostLifecycleThroughCredentials(t *testing.T) {
	f := newSupplyFixture(t)
	cfg := testHostAuthConfig(t)
	enrolled := f.enroll(t, cfg, "default", "i-lifecycle")
	serviceID := uuid.NewV7().String()
	credential := f.exchange(t, cfg, enrolled, serviceID)
	if credential.Epoch != 1 || credential.ExpiresIn != time.Hour {
		t.Fatalf("credential epoch = %d, expires in %s", credential.Epoch, credential.ExpiresIn)
	}
	if _, err := AuthenticateHost(t.Context(), f.q, cfg, credential.Value, time.Now()); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("registering host authenticated for ordinary routes: %v", err)
	}

	recovering := f.authenticate(t, AuthenticateRecoveringHost, cfg, credential)
	if recovering.HostID != enrolled.HostID || recovering.GroupID != f.groupID() || recovering.Epoch != 1 ||
		recovering.ResourceID != "i-lifecycle" || recovering.Status != db.WorkerHostStatusRegistering || recovering.EpochStartedAt.IsZero() {
		t.Fatalf("recovering principal = %+v", recovering)
	}
	if err := RecordStartupRecovery(t.Context(), f.q, recovering, startupEvidence(t)); err != nil {
		t.Fatal(err)
	}
	stale := recovering
	stale.Epoch++
	var conflicting ConflictError
	if err := RecordStartupRecovery(t.Context(), f.q, stale, startupEvidence(t)); !errors.As(err, &conflicting) {
		t.Fatalf("stale startup recovery error = %v", err)
	}

	template := validHostTemplate(t)
	activating := f.authenticate(t, AuthenticateActivatingHost, cfg, credential)
	if err := ActivateHost(t.Context(), f.pool, activating, Activation{
		Template: template, CPUEnvironment: []byte(`{"vendor":"test"}`), CPUEnvironmentDigest: f.cpuConfigDigest,
	}); err != nil {
		t.Fatal(err)
	}
	group := f.currentGroup(t)
	if !group.PrimaryPoolID.Valid || pgvalue.MustUUIDValue(group.PrimaryPoolID) != enrolled.PoolID {
		t.Fatalf("activation did not select the sealed pool as primary: %+v", group.PrimaryPoolID)
	}
	// Selecting the initial primary pool advanced the group claim version, so
	// the host exchanges its secret again for the same epoch.
	if _, err := AuthenticateHost(t.Context(), f.q, cfg, credential.Value, time.Now()); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("pre-activation credential error = %v", err)
	}
	credential = f.exchange(t, cfg, enrolled, serviceID)
	principal := f.authenticate(t, AuthenticateHost, cfg, credential)
	state, err := ReadHost(t.Context(), f.q, principal)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != db.WorkerHostStatusActive || state.ActiveInstances != 0 {
		t.Fatalf("activated host state = %+v", state)
	}
	if err := RecordObservation(t.Context(), f.q, principal, HostObservation{VMPausedReason: "maintenance"}); err != nil {
		t.Fatal(err)
	}
	if state, err = ReadHost(t.Context(), f.q, principal); err != nil || !state.VMPausedReason.Valid || state.VMPausedReason.String != "maintenance" {
		t.Fatalf("observed host state = %+v, err = %v", state, err)
	}

	if err := BeginHostDrain(t.Context(), f.pool, principal); err != nil {
		t.Fatal(err)
	}
	// Draining advanced the host claim version, so the credential minted before it
	// no longer authenticates; the same service re-exchanges the same epoch.
	if _, err := AuthenticateHost(t.Context(), f.q, cfg, credential.Value, time.Now()); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("pre-drain credential error = %v", err)
	}
	credential = f.exchange(t, cfg, enrolled, serviceID)
	if credential.Epoch != 1 {
		t.Fatalf("reissued epoch = %d, want 1", credential.Epoch)
	}
	completing := f.authenticate(t, AuthenticateDrainCompletingHost, cfg, credential)
	if completing.Status != db.WorkerHostStatusDraining {
		t.Fatalf("drain completing principal = %+v", completing)
	}
	completed, err := CompleteHostDrain(t.Context(), f.pool, completing, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != db.WorkerHostStatusTerminationReady {
		t.Fatalf("completed drain status = %s", completed.Status)
	}
	// A lost completion response is replayed with the same credential.
	replaying := f.authenticate(t, AuthenticateDrainCompletingHost, cfg, credential)
	if _, err := CompleteHostDrain(t.Context(), f.pool, replaying, time.Now()); err != nil {
		t.Fatalf("drain completion replay: %v", err)
	}
	if _, err := AuthenticateHost(t.Context(), f.q, cfg, credential.Value, time.Now()); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("termination ready host authenticated for ordinary routes: %v", err)
	}
}

func TestIssueHostCredentialAdvancesEpochPerService(t *testing.T) {
	f := newSupplyFixture(t)
	cfg := testHostAuthConfig(t)
	enrolled := f.enroll(t, cfg, "default", "i-epochs")
	service := uuid.NewV7().String()
	if first, again := f.exchange(t, cfg, enrolled, service), f.exchange(t, cfg, enrolled, service); first.Epoch != 1 || again.Epoch != 1 {
		t.Fatalf("same service epochs = %d, %d", first.Epoch, again.Epoch)
	}
	if next := f.exchange(t, cfg, enrolled, uuid.NewV7().String()); next.Epoch != 2 {
		t.Fatalf("new service epoch = %d, want 2", next.Epoch)
	}
	for name, exchange := range map[string]HostCredentialRequest{
		"wrong secret": {HostID: enrolled.HostID.String(), Secret: hostSecretPrefix + "wrong", ServiceID: service},
		"other host":   {HostID: uuid.NewV7().String(), Secret: enrolled.Secret, ServiceID: service},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := IssueHostCredential(t.Context(), f.q, cfg, exchange, time.Now); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("error = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

func TestEnrollHostRejectsRotatedEnrollmentToken(t *testing.T) {
	f := newSupplyFixture(t)
	cfg := testHostAuthConfig(t)
	tokenHash, err := auth.ParseEnrollmentToken(f.enrollmentToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RotateGroupToken(t.Context(), f.q, f.groupID()); err != nil {
		t.Fatal(err)
	}
	if _, err := EnrollHost(t.Context(), f.q, cfg, Enrollment{TokenHash: tokenHash, PoolName: "default", ResourceID: "i-rotated"}); !errors.Is(err, ErrInvalidEnrollmentToken) {
		t.Fatalf("error = %v, want ErrInvalidEnrollmentToken", err)
	}
}

func TestHostCredentialsGoStaleWithGroupTransitions(t *testing.T) {
	f := newSupplyFixture(t)
	cfg := testHostAuthConfig(t)
	host := f.start(t, cfg, "default", "i-group", validHostTemplate(t))
	if _, err := PauseGroup(t.Context(), f.pool, f.groupID(), f.currentGroup(t).ClaimVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := AuthenticateHost(t.Context(), f.q, cfg, host.credential.Value, time.Now()); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("pre-transition credential error = %v", err)
	}
	refreshed := f.authenticate(t, AuthenticateHost, cfg, f.exchange(t, cfg, host.enrolled, host.serviceID))
	if refreshed.GroupClaimVersion != f.currentGroup(t).ClaimVersion || refreshed.HostClaimVersion != host.principal.HostClaimVersion {
		t.Fatalf("refreshed principal = %+v, started principal = %+v", refreshed, host.principal)
	}
}

func TestCheckLockedClaimsReportsStaleClaims(t *testing.T) {
	f := newSupplyFixture(t)
	cfg := testHostAuthConfig(t)
	host := f.start(t, cfg, "default", "i-claims", validHostTemplate(t))
	locked := func() (db.WorkerHost, db.WorkerGroup) {
		t.Helper()
		_, claim := f.hostState(t, host.enrolled.HostID)
		return db.WorkerHost{ClaimVersion: claim}, f.currentGroup(t)
	}
	if err := host.principal.checkLockedClaims(locked()); err != nil {
		t.Fatalf("current claims: %v", err)
	}
	if _, err := PauseGroup(t.Context(), f.pool, f.groupID(), f.currentGroup(t).ClaimVersion); err != nil {
		t.Fatal(err)
	}
	if err := host.principal.checkLockedClaims(locked()); !errors.Is(err, ErrStaleClaims) {
		t.Fatalf("group transition error = %v, want ErrStaleClaims", err)
	}
}

func TestActivateHostRejectsTemplateMismatchWithSealedPool(t *testing.T) {
	f := newSupplyFixture(t)
	cfg := testHostAuthConfig(t)
	template := validHostTemplate(t)
	f.start(t, cfg, "default", "i-first", template)

	changed := template
	changed.Capacity.VMSlots = 2
	enrolled := f.enroll(t, cfg, "default", "i-second")
	credential := f.exchange(t, cfg, enrolled, uuid.NewV7().String())
	if err := RecordStartupRecovery(t.Context(), f.q, f.authenticate(t, AuthenticateRecoveringHost, cfg, credential), startupEvidence(t)); err != nil {
		t.Fatal(err)
	}
	err := ActivateHost(t.Context(), f.pool, f.authenticate(t, AuthenticateActivatingHost, cfg, credential), Activation{
		Template: changed, CPUEnvironment: []byte(`{}`), CPUEnvironmentDigest: f.cpuConfigDigest,
	})
	var conflicting ConflictError
	if !errors.As(err, &conflicting) || err.Error() != "worker activation is stale" {
		t.Fatalf("error = %v, want stale activation conflict", err)
	}
}

func TestFenceHostReplaysAfterFence(t *testing.T) {
	f := newSupplyFixture(t)
	cfg := testHostAuthConfig(t)
	host := f.start(t, cfg, "default", "i-fence", validHostTemplate(t))
	fencing := f.authenticate(t, AuthenticateFencingHost, cfg, host.credential)
	if err := FenceHost(t.Context(), f.q, fencing, " worker_retired "); err != nil {
		t.Fatal(err)
	}
	if status, _ := f.hostState(t, host.enrolled.HostID); status != db.WorkerHostStatusLost {
		t.Fatalf("fenced host status = %s", status)
	}
	replaying := f.authenticate(t, AuthenticateFencingHost, cfg, host.credential)
	if err := FenceHost(t.Context(), f.q, replaying, "worker_retired"); err != nil {
		t.Fatalf("fence replay: %v", err)
	}
	if _, err := AuthenticateHost(t.Context(), f.q, cfg, host.credential.Value, time.Now()); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("fenced host authenticated for ordinary routes: %v", err)
	}
	if _, err := IssueHostCredential(t.Context(), f.q, cfg, HostCredentialRequest{
		HostID: host.enrolled.HostID.String(), Secret: host.enrolled.Secret, ServiceID: host.serviceID,
	}, time.Now); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("fenced host exchanged its revoked host secret: %v", err)
	}
}

func TestHostOperationsRejectAnotherEpoch(t *testing.T) {
	f := newSupplyFixture(t)
	cfg := testHostAuthConfig(t)
	host := f.start(t, cfg, "default", "i-epoch", validHostTemplate(t))
	other := host.principal
	other.Epoch++
	if err := RecordObservation(t.Context(), f.q, other, HostObservation{}); !errors.Is(err, ErrObservationConflict) {
		t.Fatalf("observation error = %v, want ErrObservationConflict", err)
	}
	if err := BeginHostDrain(t.Context(), f.pool, other); !errors.Is(err, ErrHostNotFound) {
		t.Fatalf("drain error = %v, want ErrHostNotFound", err)
	}
	if err := FenceHost(t.Context(), f.q, other, "worker_retired"); !errors.Is(err, ErrHostNotFound) {
		t.Fatalf("fence error = %v, want ErrHostNotFound", err)
	}
	other = host.principal
	other.GroupID = uuid.NewV7()
	if _, err := ReadHost(t.Context(), f.q, other); !errors.Is(err, ErrHostNotFound) {
		t.Fatalf("read error = %v, want ErrHostNotFound", err)
	}
}

func TestDrainInvalidEpochDrainsAnActiveHostOnce(t *testing.T) {
	f := newSupplyFixture(t)
	cfg := testHostAuthConfig(t)
	host := f.start(t, cfg, "default", "i-invalid", validHostTemplate(t))
	status := func() (db.WorkerHostStatus, int64) {
		t.Helper()
		return f.hostState(t, host.enrolled.HostID)
	}
	if err := DrainInvalidEpoch(t.Context(), f.pool, f.groupID(), host.enrolled.HostID, host.principal.Epoch); err != nil {
		t.Fatal(err)
	}
	drained, claim := status()
	if drained != db.WorkerHostStatusDraining {
		t.Fatalf("status = %s, want draining", drained)
	}
	if err := DrainInvalidEpoch(t.Context(), f.pool, f.groupID(), host.enrolled.HostID, host.principal.Epoch); err != nil {
		t.Fatal(err)
	}
	if again, againClaim := status(); again != db.WorkerHostStatusDraining || againClaim != claim {
		t.Fatalf("second drain changed the host: %s claim %d, want claim %d", again, againClaim, claim)
	}
	if err := DrainInvalidEpoch(t.Context(), f.pool, f.groupID(), host.enrolled.HostID, host.principal.Epoch+1); err == nil {
		t.Fatal("another epoch was drained")
	}
}

// lockWaitQuerier runs wait after the authenticating statement returns, as
// if the statement had waited for its row locks.
type lockWaitQuerier struct {
	db.Querier
	wait func()
}

func (q lockWaitQuerier) AuthenticateWorkerHostSecret(ctx context.Context, params db.AuthenticateWorkerHostSecretParams) (db.AuthenticateWorkerHostSecretRow, error) {
	row, err := q.Querier.AuthenticateWorkerHostSecret(ctx, params)
	q.wait()
	return row, err
}

func TestIssueHostCredentialIssuesAfterAuthentication(t *testing.T) {
	f := newSupplyFixture(t)
	keys, err := auth.NewKeys(bytes.Repeat([]byte{1}, auth.RootKeySize))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := NewHostAuthConfig(keys.WorkerHost, bytes.Repeat([]byte{2}, HostCredentialSigningKeySize), 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	enrolled := f.enroll(t, cfg, "default", "i-lock-wait")
	clock := time.Now()
	authenticated := clock.Add(time.Minute)
	credential, err := IssueHostCredential(t.Context(), lockWaitQuerier{Querier: f.q, wait: func() { clock = authenticated }}, cfg, HostCredentialRequest{
		HostID: enrolled.HostID.String(), Secret: enrolled.Secret, ServiceID: uuid.NewV7().String(),
	}, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	if credential.ExpiresIn != 2*time.Minute {
		t.Fatalf("expires in %s, want 2m", credential.ExpiresIn)
	}
	claims, err := verifyHostCredential(cfg.signingKey, credential.Value, authenticated)
	if err != nil {
		t.Fatal(err)
	}
	if !claims.IssuedAt.Equal(authenticated.Truncate(time.Second)) || claims.ExpiresAt.Sub(claims.IssuedAt) != 2*time.Minute {
		t.Fatalf("issued at %s, expires at %s; authenticated at %s", claims.IssuedAt, claims.ExpiresAt, authenticated)
	}
	// The lock wait does not shorten the advertised lifetime.
	if _, err := verifyHostCredential(cfg.signingKey, credential.Value, authenticated.Add(credential.ExpiresIn-2*time.Second)); err != nil {
		t.Fatalf("credential expired before its advertised lifetime: %v", err)
	}
}
