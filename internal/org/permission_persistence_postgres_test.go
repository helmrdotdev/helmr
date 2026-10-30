package org_test

import (
	"errors"
	"slices"
	"strconv"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestRolePermissionSetIsCanonicalAndNonempty(t *testing.T) {
	f := newMachineGrantFixture(t)
	role := f.role(t, "secrets.create", "sessions.create", "sessions.create")
	want := []string{"secrets.create", "sessions.create"}
	if !slices.Equal(role.Permissions, want) || role.Revision != 1 {
		t.Fatalf("created Role: %+v", role)
	}
	var stored []string
	if err := f.pool.QueryRow(t.Context(), `SELECT permissions FROM roles WHERE id=$1`, role.ID).Scan(&stored); err != nil || !slices.Equal(stored, want) {
		t.Fatalf("stored permission set: %v %v", stored, err)
	}
	for _, permissions := range [][]string{nil, {"unknown.permission"}} {
		var invalid org.InputError
		if _, err := org.SavePermissionRole(t.Context(), f.pool, f.owner, role.ID, org.RoleInput{Name: role.Name, ExpectedRevision: "1", Permissions: permissions}); !errors.As(err, &invalid) {
			t.Fatalf("invalid permission set accepted: %v", err)
		}
	}
	for _, value := range []string{"{}", "{NULL}", "{{sessions.create,secrets.create},{sessions.create,secrets.create}}"} {
		_, err := f.pool.Exec(t.Context(), `UPDATE roles SET permissions=$2::text[] WHERE id=$1`, role.ID, value)
		var constraint *pgconn.PgError
		if !errors.As(err, &constraint) || constraint.Code != "23514" {
			t.Fatalf("invalid array %s accepted: %v", value, err)
		}
	}
}

func TestGrantRevisionAndAuditFollowLogicalMutation(t *testing.T) {
	f := newMachineGrantFixture(t)
	ctx := t.Context()
	role := f.role(t, "sessions.create", "secrets.create")
	input := f.input(role)
	input.AgentIDs = []uuid.UUID{f.agent, f.otherAgent}
	grants, err := org.AssignRole(ctx, f.pool, f.owner, input)
	if err != nil {
		t.Fatal(err)
	}
	grant := grants[0]
	if grant.Revision != 1 {
		t.Fatalf("two-target creation revision = %d, want 1", grant.Revision)
	}
	check := func(want org.RoleGrant) {
		t.Helper()
		rows, err := org.QueryRoleGrants(ctx, f.pool, f.owner, org.GrantFilter{ID: grant.ID})
		if err != nil || len(rows) != 1 || rows[0].Revision != want.Revision || rows[0].AgentsMode != want.AgentsMode || !slices.Equal(rows[0].AgentIDs, want.AgentIDs) || !slices.Equal(rows[0].Permissions, want.Permissions) {
			t.Fatalf("assignment changed: %+v %v; want %+v", rows, err, want)
		}
	}
	// A real audit failure must roll back both header and target changes.
	dbtest.MustExec(t, ctx, f.pool, `ALTER TABLE audit_events ADD CONSTRAINT reject_policy_edit CHECK (action NOT IN ('role.updated','role_grant.updated')) NOT VALID`)
	input.ExpectedRevision = "1"
	input.AgentsMode, input.AgentIDs = "all", nil
	if _, err := org.UpdateRoleGrant(ctx, f.pool, f.owner, grant.ID, input); err == nil {
		t.Fatal("unaudited assignment update committed")
	}
	check(grant)
	if _, err := org.SavePermissionRole(ctx, f.pool, f.owner, role.ID, org.RoleInput{Name: role.Name, ExpectedRevision: "1", Permissions: []string{"secrets.create"}}); err == nil {
		t.Fatal("unaudited Role edit committed")
	}
	check(grant)
	var revision int64
	if err := f.pool.QueryRow(ctx, `SELECT revision FROM roles WHERE id=$1`, role.ID).Scan(&revision); err != nil || revision != 1 {
		t.Fatalf("failed Role edit changed revision: %d %v", revision, err)
	}
	dbtest.MustExec(t, ctx, f.pool, `ALTER TABLE audit_events DROP CONSTRAINT reject_policy_edit`)
	for _, replacement := range []struct {
		mode string
		ids  []uuid.UUID
	}{{"all", nil}, {"selected", []uuid.UUID{f.otherAgent}}, {"selected", []uuid.UUID{f.agent, f.otherAgent}}} {
		input.ExpectedRevision = strconv.FormatInt(grant.Revision, 10)
		input.AgentsMode, input.AgentIDs = replacement.mode, replacement.ids
		changed, err := org.UpdateRoleGrant(ctx, f.pool, f.owner, grant.ID, input)
		if err != nil || changed.Revision != grant.Revision+1 {
			t.Fatalf("logical replacement: %+v %v", changed, err)
		}
		var audited bool
		if err := f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_events WHERE action='role_grant.updated' AND target_id=$1 AND (details->'before'->>'revision')::bigint=$2 AND (details->'after'->>'revision')::bigint=$3)`, grant.ID, grant.Revision, changed.Revision).Scan(&audited); err != nil || !audited {
			t.Fatalf("logical revision missing from audit: %v", err)
		}
		grant = changed
		check(grant)
	}
	updated, err := org.SavePermissionRole(ctx, f.pool, f.owner, role.ID, org.RoleInput{Name: role.Name, ExpectedRevision: "1", Permissions: []string{"secrets.create"}})
	if err != nil {
		t.Fatal(err)
	}
	grant.Revision++
	grant.AgentsMode, grant.AgentIDs, grant.Permissions = "none", nil, updated.Permissions
	check(grant)
	var allAgents bool
	if err := f.pool.QueryRow(ctx, `SELECT all_agents FROM role_grants WHERE id=$1`, grant.ID).Scan(&allAgents); err != nil || allAgents {
		t.Fatalf("Role removal retained all-Agent intent: %t %v", allAgents, err)
	}
	if _, err := org.SavePermissionRole(ctx, f.pool, f.owner, role.ID, org.RoleInput{Name: role.Name, ExpectedRevision: strconv.FormatInt(updated.Revision, 10), Permissions: []string{"sessions.create", "secrets.create"}}); !errors.Is(err, org.ErrRoleTarget) {
		t.Fatalf("Agent capability restored without explicit targets: %v", err)
	}
}

func TestTargetRestrictionRequiresExplicitCleanupBeforeParentTeardown(t *testing.T) {
	for _, parent := range []string{"environment", "organization"} {
		t.Run(parent, func(t *testing.T) {
			f := newMachineGrantFixture(t)
			role := f.role(t, "sessions.create")
			input := f.input(role)
			// Use a human assignment so key lifecycle restriction is not the subject.
			input.Subject = org.GrantSubject{Type: "member", MemberID: &f.member.MemberID}
			grants, err := org.AssignRole(t.Context(), f.pool, f.owner, input)
			if err != nil {
				t.Fatal(err)
			}
			removeParent := func() error {
				if parent == "environment" {
					_, err := f.pool.Exec(t.Context(), `DELETE FROM environments WHERE id=$1`, f.env)
					return err
				}
				_, err := f.pool.Exec(t.Context(), `DELETE FROM organizations WHERE id=$1`, f.owner.OrgID)
				return err
			}
			// Parent teardown still requires explicit assignment and Agent cleanup.
			var constraint *pgconn.PgError
			if err := removeParent(); !errors.As(err, &constraint) || constraint.Code != "23503" {
				t.Fatalf("parent deletion bypassed Agent references: %v", err)
			}
			if err := org.RemoveRoleGrant(t.Context(), f.pool, f.owner, grants[0].ID, strconv.FormatInt(grants[0].Revision, 10)); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.pool, `DELETE FROM agents WHERE environment_id=$1`, f.env)
			if err := removeParent(); err != nil {
				t.Fatalf("%s teardown after explicit cleanup: %v", parent, err)
			}
			var targets int
			if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM role_grant_agents WHERE environment_id=$1`, f.env).Scan(&targets); err != nil || targets != 0 {
				t.Fatalf("orphan targets: %d %v", targets, err)
			}
		})
	}
}

func TestFirstAgentCapabilityRacesUntargetedAssignment(t *testing.T) {
	f := newMachineGrantFixture(t)
	ctx := t.Context()
	for range 4 {
		role := f.role(t, "secrets.create")
		input := f.input(role)
		input.AgentsMode, input.AgentIDs = "none", nil
		start := make(chan struct{})
		assigned, edited := make(chan error, 1), make(chan error, 1)
		go func() {
			<-start
			_, err := org.AssignRole(ctx, f.pool, f.owner, input)
			assigned <- err
		}()
		go func() {
			<-start
			_, err := org.SavePermissionRole(ctx, f.pool, f.admin, role.ID, org.RoleInput{Name: role.Name, ExpectedRevision: "1", Permissions: []string{"sessions.create", "secrets.create"}})
			edited <- err
		}()
		close(start)
		assignErr, editErr := <-assigned, <-edited
		if !(assignErr == nil && errors.Is(editErr, org.ErrRoleTarget) || editErr == nil && errors.Is(assignErr, org.ErrStaleRevision)) {
			t.Fatalf("first Agent capability race: assign=%v edit=%v", assignErr, editErr)
		}
		var invalid bool
		if err := f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM role_grants g JOIN roles r ON r.id=g.role_id WHERE r.id=$1 AND 'sessions.create'=ANY(r.permissions))`, role.ID).Scan(&invalid); err != nil || invalid {
			t.Fatalf("untargeted assignment gained Agent capability: %t %v", invalid, err)
		}
	}
}

func TestAgentDeletionRacesSelectedAssignment(t *testing.T) {
	f := newMachineGrantFixture(t)
	ctx := t.Context()
	for range 4 {
		role := f.role(t, "sessions.create")
		agent := uuid.NewV7()
		dbtest.MustExec(t, ctx, f.pool, `INSERT INTO agents(id,environment_id,key) VALUES($1,$2,$1::uuid::text)`, agent, f.env)
		input := f.input(role)
		input.AgentIDs = []uuid.UUID{agent}
		start := make(chan struct{})
		assigned, deleted := make(chan error, 1), make(chan error, 1)
		go func() {
			<-start
			_, err := org.AssignRole(ctx, f.pool, f.owner, input)
			assigned <- err
		}()
		go func() {
			<-start
			_, err := f.pool.Exec(ctx, `DELETE FROM agents WHERE environment_id=$1 AND id=$2`, f.env, agent)
			deleted <- err
		}()
		close(start)
		assignErr, deleteErr := <-assigned, <-deleted
		var constraint *pgconn.PgError
		if !(assignErr == nil && errors.As(deleteErr, &constraint) && constraint.Code == "23001" || deleteErr == nil && errors.Is(assignErr, org.ErrAccessNotFound)) {
			t.Fatalf("Agent deletion race: assign=%v delete=%v", assignErr, deleteErr)
		}
		var exists bool
		var targets int
		if err := f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agents WHERE environment_id=$1 AND id=$2),(SELECT count(*) FROM role_grant_agents WHERE environment_id=$1 AND agent_id=$2)`, f.env, agent).Scan(&exists, &targets); err != nil {
			t.Fatal(err)
		}
		if assignErr == nil && (!exists || targets != 1) || deleteErr == nil && (exists || targets != 0) {
			t.Fatalf("Agent deletion race lost its target: exists=%t targets=%d", exists, targets)
		}
	}
}
