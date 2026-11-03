package workergroup

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db"
)

// A host fenced as lost, for staying silent past its freshness or by its
// operator, returns with no operator step once its machine proves its key:
// the next credential exchange makes it registering under a new epoch, even
// for a worker process that was only asleep. A revoked key cannot return.
func TestLostHostReturnsWithItsKeyUnderANewEpoch(t *testing.T) {
	f := newSupplyFixture(t)
	cfg := testHostAuthConfig(t)
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	operator := DeploymentOperator(audit.Internal(audit.ControlPlane))
	started := f.start(t, cfg, "default", "machine", validHostTemplate(t))
	_, claim := f.hostState(t, started.enrolled.HostID)
	lost, err := MarkHostLost(t.Context(), f.pool, policy, operator, f.groupID(), "machine", started.enrolled.HostID, claim)
	if err != nil || lost.Status != "lost" {
		t.Fatalf("lost = %+v, %v", lost, err)
	}
	// The old credential no longer authenticates.
	if _, err := AuthenticateHost(t.Context(), f.q, cfg, started.credential.Value, time.Now()); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("old credential after loss: %v", err)
	}

	// The same worker process wakes and proves its key with its old service.
	returned, err := IssueHostCredential(t.Context(), f.pool, policy, cfg, signedExchange(t, cfg, started.enrolled, started.serviceID), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if returned.Epoch <= started.credential.Epoch {
		t.Fatalf("returned epoch = %d, want past %d", returned.Epoch, started.credential.Epoch)
	}
	status, _ := f.hostState(t, started.enrolled.HostID)
	if status != db.WorkerHostStatusRegistering {
		t.Fatalf("returned host status = %s, want registering", status)
	}
	principal, err := AuthenticateRecoveringHost(t.Context(), f.q, cfg, returned.Value, time.Now())
	if err != nil || principal.Epoch != returned.Epoch {
		t.Fatalf("returned host cannot report its recovery: %+v, %v", principal, err)
	}
	var audited int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='worker_host.returned' AND target_id=$1`, started.enrolled.HostID).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("return audit count = %d, %v", audited, err)
	}

	// A revoked key neither returns nor authenticates.
	revoked, err := RevokeHost(t.Context(), f.pool, policy, operator, f.groupID(), started.enrolled.HostID)
	if err != nil || revoked.Status != "lost" || !revoked.TransitionApplied {
		t.Fatalf("revoked = %+v, %v", revoked, err)
	}
	if _, err := IssueHostCredential(t.Context(), f.pool, policy, cfg, signedExchange(t, cfg, started.enrolled, uuid.NewV7().String()), time.Now); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked key returned: %v", err)
	}
	if again, err := RevokeHost(t.Context(), f.pool, policy, operator, f.groupID(), started.enrolled.HostID); err != nil || again.TransitionApplied {
		t.Fatalf("second revoke = %+v, %v", again, err)
	}
}

// A lost host whose resource another host has since taken does not return:
// two live incarnations of one machine never coexist.
func TestLostHostDoesNotReturnOverAReplacement(t *testing.T) {
	f := newSupplyFixture(t)
	cfg := testHostAuthConfig(t)
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	operator := DeploymentOperator(audit.Internal(audit.ControlPlane))
	first := f.start(t, cfg, "default", "machine", validHostTemplate(t))
	_, claim := f.hostState(t, first.enrolled.HostID)
	if _, err := MarkHostLost(t.Context(), f.pool, policy, operator, f.groupID(), "machine", first.enrolled.HostID, claim); err != nil {
		t.Fatal(err)
	}
	f.enroll(t, cfg, "default", "machine")
	if _, err := IssueHostCredential(t.Context(), f.pool, policy, cfg, signedExchange(t, cfg, first.enrolled, first.serviceID), time.Now); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("lost host returned over its replacement: %v", err)
	}
}
