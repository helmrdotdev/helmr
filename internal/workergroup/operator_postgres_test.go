package workergroup

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

var operatorTestPolicy = auditpolicy.Policy{RetentionSeconds: 3600}

func organizationTestOperator(orgID uuid.UUID) Operator {
	return OrganizationOperator(orgID, func(context.Context, pgx.Tx) (audit.Evidence, error) {
		return audit.Internal(audit.ControlPlane), nil
	})
}

// Each operator reaches only the groups it owns; every other group reads as
// missing and is left unchanged.
func TestOperatorsReachOnlyTheirOwnGroups(t *testing.T) {
	f := newSupplyFixture(t)
	owner, other := uuid.NewV7(), uuid.NewV7()
	for _, id := range []uuid.UUID{owner, other} {
		dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO organizations (id, name, slug) VALUES ($1, 'Org', $2)`, id, id.String())
	}
	created, err := CreateGroup(t.Context(), f.pool, operatorTestPolicy, organizationTestOperator(owner), GroupInput{RegionID: fixtureRegionID, Name: "supply-test"})
	if err != nil {
		t.Fatal(err)
	}
	if !created.Group.OwnerOrgID.Valid || uuid.UUID(created.Group.OwnerOrgID.Bytes) != owner {
		t.Fatalf("created group owner = %v", created.Group.OwnerOrgID)
	}
	organizationGroup := uuid.UUID(created.Group.ID.Bytes)
	deployment := DeploymentOperator(audit.Internal(audit.ControlPlane))
	for _, test := range []struct {
		name     string
		operator Operator
		group    uuid.UUID
	}{
		{"deployment operator on an organization group", deployment, organizationGroup},
		{"organization on another organization's group", organizationTestOperator(other), organizationGroup},
		{"organization on a deployment group", organizationTestOperator(owner), f.groupID()},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := PauseGroup(t.Context(), f.pool, operatorTestPolicy, test.operator, test.group, 1); !errors.Is(err, ErrGroupNotFound) {
				t.Fatalf("pause error = %v", err)
			}
			if _, err := RotateGroupToken(t.Context(), f.pool, operatorTestPolicy, test.operator, test.group); !errors.Is(err, ErrGroupNotFound) {
				t.Fatalf("rotate error = %v", err)
			}
			if _, err := RevokeGroupHostCredentials(t.Context(), f.pool, operatorTestPolicy, test.operator, test.group); !errors.Is(err, ErrGroupNotFound) {
				t.Fatalf("revoke error = %v", err)
			}
			if _, err := MarkHostLost(t.Context(), f.pool, operatorTestPolicy, test.operator, test.group, "host", uuid.Nil(), 1); !errors.Is(err, ErrGroupNotFound) {
				t.Fatalf("mark lost error = %v", err)
			}
		})
	}
	if _, err := GetOrganizationGroup(t.Context(), f.q, other, organizationGroup); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("other organization reads the group: %v", err)
	}
	if groups, err := ListOrganizationGroups(t.Context(), f.q, owner, "", 10); err != nil || len(groups) != 1 {
		t.Fatalf("owner lists %d groups, %v", len(groups), err)
	}

	// Platform safety may pause, revoke and disable any group, and nothing else.
	safety := PlatformSafetyOperator(audit.Internal(audit.ControlPlane))
	for _, refused := range []error{
		func() error {
			_, err := CreateGroup(t.Context(), f.pool, operatorTestPolicy, safety, GroupInput{RegionID: fixtureRegionID, Name: "safety"})
			return err
		}(),
		func() error {
			_, err := RotateGroupToken(t.Context(), f.pool, operatorTestPolicy, safety, organizationGroup)
			return err
		}(),
		func() error {
			_, err := ActivateGroup(t.Context(), f.pool, operatorTestPolicy, safety, organizationGroup, 1)
			return err
		}(),
		func() error {
			_, err := BeginGroupDrain(t.Context(), f.pool, operatorTestPolicy, safety, organizationGroup, 1)
			return err
		}(),
		func() error {
			_, err := MarkHostLost(t.Context(), f.pool, operatorTestPolicy, safety, organizationGroup, "host", uuid.Nil(), 1)
			return err
		}(),
	} {
		if !errors.Is(refused, errSafetyOperation) {
			t.Fatalf("safety operation error = %v", refused)
		}
	}
	paused, err := PauseGroup(t.Context(), f.pool, operatorTestPolicy, safety, organizationGroup, created.Group.ClaimVersion)
	if err != nil || paused.Status != "paused" {
		t.Fatalf("safety pause = %+v, %v", paused, err)
	}
	var orgRecorded bool
	if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM audit_events WHERE action='worker_group.paused' AND target_id=$1 AND org_id=$2)`, organizationGroup, owner).Scan(&orgRecorded); err != nil || !orgRecorded {
		t.Fatalf("safety pause recorded for the organization = %t, %v", orgRecorded, err)
	}
}

// Disabling a drained group also disables its remaining pools, so nobody has
// to retire them one by one first.
func TestDisableGroupDisablesRemainingPools(t *testing.T) {
	f := newSupplyFixture(t)
	f.pendingPool(t, "pending")
	active := f.activePool(t, "active")
	group := f.currentGroup(t)
	operator := DeploymentOperator(audit.Internal(audit.ControlPlane))
	if _, err := DisableGroup(t.Context(), f.pool, operatorTestPolicy, operator, f.groupID(), group.ClaimVersion); err == nil {
		t.Fatal("disabled an active group")
	}
	drained, err := BeginGroupDrain(t.Context(), f.pool, operatorTestPolicy, operator, f.groupID(), group.ClaimVersion)
	if err != nil {
		t.Fatal(err)
	}
	host := f.activeHost(t, active, "host")
	var conflicting ConflictError
	if _, err := DisableGroup(t.Context(), f.pool, operatorTestPolicy, operator, f.groupID(), drained.ClaimVersion); !errors.As(err, &conflicting) {
		t.Fatalf("disable with a live host error = %v", err)
	}
	var live int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM worker_pools WHERE worker_group_id=$1 AND status<>'disabled'`, f.groupID()).Scan(&live); err != nil || live != 2 {
		t.Fatalf("refused disable left %d live pools, %v", live, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status='lost', lost_at=now() WHERE id=$1`, host)
	disabled, err := DisableGroup(t.Context(), f.pool, operatorTestPolicy, operator, f.groupID(), drained.ClaimVersion)
	if err != nil || disabled.Status != "disabled" {
		t.Fatalf("disable = %+v, %v", disabled, err)
	}
	var pools, audited int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE status<>'disabled'),
 (SELECT count(*) FROM audit_events WHERE action='worker_pool.disabled')
 FROM worker_pools WHERE worker_group_id=$1`, f.groupID()).Scan(&pools, &audited); err != nil || pools != 0 || audited != 2 {
		t.Fatalf("live pools = %d, pool audits = %d, %v", pools, audited, err)
	}
}
