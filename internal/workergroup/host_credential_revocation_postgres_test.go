package workergroup

import (
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestRotateGroupTokenKeepsEnrolledHostsRunning(t *testing.T) {
	f := newSupplyFixture(t)
	cfg := testHostAuthConfig(t)
	host := f.start(t, cfg, "default", "i-rotation", validHostTemplate(t))
	if _, err := RotateGroupToken(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, DeploymentOperator(audit.Internal(audit.ControlPlane)), f.groupID()); err != nil {
		t.Fatal(err)
	}
	f.authenticate(t, AuthenticateHost, cfg, host.credential)
	f.exchange(t, cfg, host.enrolled, host.serviceID)
}

func TestRevokeGroupHostCredentialsFencesEveryLiveHost(t *testing.T) {
	f := newSupplyFixture(t)
	cfg := testHostAuthConfig(t)
	active := f.start(t, cfg, "default", "i-revoked-active", validHostTemplate(t))
	registering := f.enroll(t, cfg, "default", "i-revoked-registering")
	registeringCredential := f.exchange(t, cfg, registering, uuid.NewV7().String())

	other := newSupplyFixtureGroup(t, f, "us-west-2")
	untouched := other.start(t, cfg, "default", "i-other-group", validHostTemplate(t))

	fenced, err := RevokeGroupHostCredentials(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, DeploymentOperator(audit.Internal(audit.ControlPlane)), f.groupID())
	if err != nil {
		t.Fatal(err)
	}
	want := []uuid.UUID{active.enrolled.HostID, registering.HostID}
	slices.SortFunc(want, func(a, b uuid.UUID) int { return a.Compare(b) })
	if !slices.Equal(fenced, want) {
		t.Fatalf("fenced hosts = %v, want %v", fenced, want)
	}
	for _, hostID := range want {
		if status, _ := f.hostState(t, hostID); status != db.WorkerHostStatusLost {
			t.Fatalf("host %s status = %s, want lost", hostID, status)
		}
	}
	if _, err := AuthenticateHost(t.Context(), f.q, cfg, active.credential.Value, time.Now()); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked host credential authorization error = %v", err)
	}
	if _, err := AuthenticateRecoveringHost(t.Context(), f.q, cfg, registeringCredential.Value, time.Now()); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked registering host credential authorization error = %v", err)
	}
	for _, enrolled := range []EnrolledHost{active.enrolled, registering} {
		if _, err := IssueHostCredential(t.Context(), f.pool, cfg, HostCredentialRequest{
			HostID: enrolled.HostID.String(), Secret: enrolled.Secret, ServiceID: uuid.NewV7().String(),
		}, time.Now); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("revoked host secret exchange error = %v", err)
		}
	}
	other.authenticate(t, AuthenticateHost, cfg, untouched.credential)

	// The enrollment token is unchanged, so a returning machine enrolls again.
	tokenHash, err := auth.ParseEnrollmentToken(f.enrollmentToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EnrollHost(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, cfg, Enrollment{TokenHash: tokenHash, PoolName: "default", ResourceID: "i-replacement"}); err != nil {
		t.Fatalf("enrollment after revocation: %v", err)
	}
	var audited int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='worker_group.host_credentials_revoked' AND target_id=$1`, f.groupID()).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("revocation audit events = %d, %v", audited, err)
	}
	var lost []uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT COALESCE(array_agg(target_id ORDER BY target_id),'{}') FROM audit_events WHERE action='worker_host.marked_lost'`).Scan(&lost); err != nil || !slices.Equal(lost, want) {
		t.Fatalf("fenced host audit events = %v, %v; want %v", lost, err, want)
	}
}

func TestRevokeGroupHostCredentialsRejectsMissingGroup(t *testing.T) {
	f := newSupplyFixture(t)
	if _, err := RevokeGroupHostCredentials(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, DeploymentOperator(audit.Internal(audit.ControlPlane)), uuid.NewV7()); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("error = %v, want ErrGroupNotFound", err)
	}
}

// newSupplyFixtureGroup adds an active worker group in its own region to the
// fixture's database and returns a fixture for that group.
func newSupplyFixtureGroup(t *testing.T, f supplyFixture, regionID string) supplyFixture {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO regions (id, display_name) VALUES ($1, 'Second Worker Supply Test')`, regionID)
	created, err := CreateGroup(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, DeploymentOperator(audit.Internal(audit.ControlPlane)), GroupInput{RegionID: regionID, Name: "supply-test"})
	if err != nil {
		t.Fatal(err)
	}
	f.group, f.enrollmentToken = created.Group, created.EnrollmentToken
	return f
}
