package auth

import (
	"slices"
	"testing"
	"uuid"
)

func TestPermissionCatalog(t *testing.T) {
	expected := map[string]int{"agents": 4, "agent_connections": 5, "sessions": 10, "deployments": 3, "templates": 2, "triggers": 1, "computers": 9, "secrets": 4, "api_keys": 5, "projects": 4, "environments": 3, "members": 2, "invitations": 3, "groups": 6, "roles": 4, "assignments": 4}
	counts := map[string]int{}
	seen := map[Permission]bool{}
	for _, d := range PermissionCatalog() {
		if seen[d.ID] || d.Name == "" || d.Description == "" {
			t.Fatalf("invalid definition: %+v", d)
		}
		seen[d.ID] = true
		counts[d.ResourceType]++
		scope := ScopeOrganization
		switch d.ResourceType {
		case "agents", "agent_connections", "sessions", "triggers":
			scope = ScopeAgent
		case "deployments", "templates", "computers", "secrets", "api_keys":
			scope = ScopeEnvironment
		}
		machine := scope != ScopeOrganization && d.ResourceType != "api_keys" && d.ResourceType != "agent_connections" && !slices.Contains([]Permission{"agents.delete", "agents.restore", "sessions.check_delivery", "sessions.abandon_delivery"}, d.ID)
		if d.ScopeType != scope || d.APIKeyAssignable != machine {
			t.Fatalf("wrong scope or machine eligibility: %+v", d)
		}
	}
	if len(seen) != 69 {
		t.Fatalf("catalog size %d", len(seen))
	}
	for resource, count := range expected {
		if counts[resource] != count {
			t.Errorf("%s: %d, want %d", resource, counts[resource], count)
		}
	}
	for _, invalid := range []Permission{"", "*", "agents.manage", "sessions.view", "computers.operate", "access.manage", "unknown.read"} {
		if IsPermission(invalid) || IsMachinePermission(invalid) {
			t.Errorf("unknown permission accepted: %q", invalid)
		}
	}
}

func TestEveryPermissionIsExactAndScopeBounded(t *testing.T) {
	org := uuid.NewV7()
	env := Scope{OrgID: org, ProjectID: uuid.NewV7().String(), EnvironmentID: uuid.NewV7().String()}
	agent := env
	agent.AgentID = uuid.NewV7().String()
	for _, d := range PermissionCatalog() {
		t.Run(string(d.ID), func(t *testing.T) {
			permission := Permission(d.ID)
			scope := env
			if d.ScopeType == ScopeOrganization {
				scope = Scope{OrgID: org}
			} else if d.ScopeType == ScopeAgent {
				scope = agent
			}
			grant := PermissionGrant{ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, AgentID: scope.AgentID, Permission: permission}
			p := Principal{Kind: PrincipalKindSession, OrgID: org, IsOwner: true, Grants: []PermissionGrant{grant}}
			// The three execution actions intentionally require use on the same Agent.
			if SessionExecutionOperation(permission) {
				if p.HasPermission(permission, scope) {
					t.Fatal("execution without Agent use")
				}
				use := grant
				use.Permission = PermissionAgentsUse
				p.Grants = append(p.Grants, use)
			}
			if !p.HasPermission(permission, scope) {
				t.Fatal("dedicated permission denied")
			}
			for _, other := range AllPermissions() {
				if other != permission && !(SessionExecutionOperation(permission) && other == PermissionAgentsUse) && p.HasPermission(other, scope) {
					t.Errorf("implied %s", other)
				}
			}
			other := scope
			other.OrgID = uuid.NewV7()
			if p.HasPermission(permission, other) {
				t.Fatal("cross organization")
			}
			if d.ScopeType != ScopeOrganization {
				other = scope
				other.EnvironmentID = uuid.NewV7().String()
				if p.HasPermission(permission, other) {
					t.Fatal("cross environment")
				}
				other = scope
				other.ProjectID = uuid.NewV7().String()
				if p.HasPermission(permission, other) {
					t.Fatal("cross project")
				}
				if p.HasPermission(permission, Scope{OrgID: org}) {
					t.Fatal("missing environment")
				}
			} else if p.HasPermission(permission, env) {
				t.Fatal("organization action accepted environment scope")
			}
			if d.ScopeType == ScopeAgent {
				other = scope
				other.AgentID = uuid.NewV7().String()
				if p.HasPermission(permission, other) || p.HasPermission(permission, env) {
					t.Fatal("selected Agent escaped")
				}
				all, ids := p.AgentPermissionScope(permission, env)
				if all || len(ids) != 1 || ids[0].String() != agent.AgentID {
					t.Fatalf("targets: %v %v", all, ids)
				}
				for i := range p.Grants {
					p.Grants[i].AgentID = ""
				}
				if !p.HasPermission(permission, other) {
					t.Fatal("all Agents denied")
				}
			}
			p.Kind = PrincipalKindAPIKey
			p.ProjectID = env.ProjectID
			p.EnvironmentID = env.EnvironmentID
			if p.HasPermission(permission, scope) != d.APIKeyAssignable {
				t.Fatal("machine eligibility mismatch")
			}
			p.EnvironmentID = uuid.NewV7().String()
			if p.HasPermission(permission, scope) {
				t.Fatal("machine borrowed another environment")
			}
			p.Grants = nil
			if p.HasPermission(permission, scope) {
				t.Fatal("owner or machine creator bypass")
			}
		})
	}
}

func TestExecutionRequiresMatchingAgentUse(t *testing.T) {
	org := uuid.NewV7()
	env := Scope{OrgID: org, ProjectID: uuid.NewV7().String(), EnvironmentID: uuid.NewV7().String()}
	first := env
	first.AgentID = uuid.NewV7().String()
	second := uuid.NewV7().String()
	for _, action := range []Permission{PermissionSessionsSend, PermissionSessionsAnswer, PermissionSessionsResume} {
		p := Principal{Kind: PrincipalKindSession, OrgID: org, Grants: []PermissionGrant{
			{ProjectID: env.ProjectID, EnvironmentID: env.EnvironmentID, AgentID: first.AgentID, Permission: action},
			{ProjectID: env.ProjectID, EnvironmentID: env.EnvironmentID, AgentID: second, Permission: PermissionAgentsUse},
		}}
		if p.HasPermission(action, first) {
			t.Fatal("borrowed use from another Agent")
		}
		p.Grants[1].AgentID = first.AgentID
		if !p.HasPermission(action, first) {
			t.Fatal("matching assignments failed to combine")
		}
		p.Grants = p.Grants[1:]
		if p.HasPermission(action, first) {
			t.Fatal("use implied session action")
		}
	}
}

func TestStandardRolesContainOnlyExplicitCompatibleActions(t *testing.T) {
	roles := StandardRoles()
	if len(roles) != 16 {
		t.Fatalf("roles: %d", len(roles))
	}
	keys := map[string]bool{}
	for _, role := range roles {
		if keys[role.Key] {
			t.Fatalf("duplicate role %s", role.Key)
		}
		keys[role.Key] = true
		seen := map[Permission]bool{}
		organization, workload := false, false
		for _, permission := range role.Permissions {
			if !IsPermission(permission) || seen[permission] {
				t.Fatalf("invalid permission in %s: %s", role.Key, permission)
			}
			seen[permission] = true
			if IsOrganizationPermission(permission) {
				organization = true
			} else {
				workload = true
			}
		}
		if organization && workload {
			t.Fatalf("mixed scope role %s", role.Key)
		}
		if role.Key == "agent_user" && (len(role.Permissions) != 1 || role.Permissions[0] != PermissionAgentsUse) {
			t.Fatal("Agent User must only allow use")
		}
	}
}
