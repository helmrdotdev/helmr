package auth

import (
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"
)

func TestPermissionCatalog(t *testing.T) {
	seen := map[Permission]bool{}
	for _, d := range PermissionCatalog() {
		if seen[d.ID] || d.Name == "" || d.Description == "" {
			t.Fatalf("invalid definition: %+v", d)
		}
		seen[d.ID] = true
		scope := ScopeOrganization
		switch d.ResourceType {
		case "agents", "agent_connections", "sessions", "triggers":
			scope = ScopeAgent
		case "deployments", "templates", "computers", "commands", "secrets", "api_keys":
			scope = ScopeEnvironment
		}
		machine := scope != ScopeOrganization && d.ResourceType != "api_keys" && d.ResourceType != "agent_connections" && !slices.Contains([]Permission{"agents.delete", "agents.restore", "sessions.check_delivery", "sessions.abandon_delivery"}, d.ID)
		scopes := []PermissionScope{scope}
		if d.ID == PermissionAuditEventsRead {
			scopes = []PermissionScope{ScopeOrganization, ScopeEnvironment}
			machine = true
		}
		if !slices.Equal(d.ScopeTypes, scopes) || d.APIKeyAssignable != machine {
			t.Fatalf("wrong scope or machine eligibility: %+v", d)
		}
	}
	if len(seen) == 0 {
		t.Fatal("empty permission catalog")
	}
	for _, invalid := range []Permission{"", "*", "unknown.permission"} {
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
		for _, scopeType := range d.ScopeTypes {
			t.Run(string(d.ID)+"/"+string(scopeType), func(t *testing.T) {
				permission := Permission(d.ID)
				scope := env
				if scopeType == ScopeOrganization {
					scope = Scope{OrgID: org}
				} else if scopeType == ScopeAgent {
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
				if scopeType != ScopeOrganization {
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
				} else if p.HasPermission(permission, env) != SupportsScope(permission, ScopeEnvironment) {
					t.Fatal("organization action accepted environment scope")
				}
				if scopeType == ScopeAgent {
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
				if p.HasPermission(permission, scope) != (d.APIKeyAssignable && scopeType != ScopeOrganization) {
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
	if len(roles) != 17 {
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
			if SupportsScope(permission, ScopeOrganization) {
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

func TestAuditScopeUsesCurrentTypedAuthority(t *testing.T) {
	org := uuid.NewV7()
	project := uuid.NewV7().String()
	prod, dev := uuid.NewV7(), uuid.NewV7()
	grant := PermissionGrant{Permission: PermissionAuditEventsRead, ProjectID: project, EnvironmentID: prod.String()}
	p := Principal{Kind: PrincipalKindSession, OrgID: org, Grants: []PermissionGrant{grant}}
	all, ids := p.AuditScope()
	if all || len(ids) != 1 || ids[0] != prod {
		t.Fatalf("environment scope: %v %v", all, ids)
	}
	p.Grants = append(p.Grants, PermissionGrant{Permission: PermissionAuditEventsRead})
	all, ids = p.AuditScope()
	if !all || len(ids) != 0 {
		t.Fatalf("organization scope: %v %v", all, ids)
	}
	p.Kind = PrincipalKindAPIKey
	p.ProjectID = project
	p.EnvironmentID = dev.String()
	all, ids = p.AuditScope()
	if all || len(ids) != 0 {
		t.Fatal("key borrowed organization or foreign environment authority")
	}
	p.EnvironmentID = prod.String()
	all, ids = p.AuditScope()
	if all || len(ids) != 1 || ids[0] != prod {
		t.Fatal("key lost its explicit environment grant")
	}
	p.Grants = nil
	all, ids = p.AuditScope()
	if all || len(ids) != 0 {
		t.Fatal("revoked grant still visible")
	}
	p.Grants = []PermissionGrant{grant}
	p.OrgID = uuid.Nil()
	all, ids = p.AuditScope()
	if all || len(ids) != 0 {
		t.Fatal("unbound principal can inspect audit")
	}
}

func TestRoleAssignmentLevelsPreserveAgentRefinement(t *testing.T) {
	for _, tc := range []struct {
		permissions []string
		levels      []PermissionScope
	}{
		{[]string{"audit_events.read"}, []PermissionScope{ScopeOrganization, ScopeEnvironment}},
		{[]string{"audit_events.read", "members.read"}, []PermissionScope{ScopeOrganization}},
		{[]string{"audit_events.read", "sessions.read"}, []PermissionScope{ScopeEnvironment}},
		{[]string{"computers.read", "sessions.read"}, []PermissionScope{ScopeEnvironment}},
		{[]string{"members.read", "computers.read"}, []PermissionScope{}},
		{[]string{"missing.read"}, []PermissionScope{}},
	} {
		if got := RoleAssignmentLevels(tc.permissions); !slices.Equal(got, tc.levels) {
			t.Fatalf("%v: %v, want %v", tc.permissions, got, tc.levels)
		}
	}
}

func TestConnectionConfigureRequiresRecentAuthentication(t *testing.T) {
	p := Principal{Kind: PrincipalKindSession, AuthenticatedAt: time.Now().Add(-2 * time.Hour)}
	if err := RequireRecentFor(p, PermissionAgentConnectionsConfigure); !errors.Is(err, ErrRecentProofRequired) {
		t.Fatalf("stale sign-in configured connection: %v", err)
	}
	if err := RequireRecentFor(p, PermissionAgentConnectionsRead); err != nil {
		t.Fatalf("connection read required recent sign-in: %v", err)
	}
	p.AuthenticatedAt = time.Now()
	if err := RequireRecentFor(p, PermissionAgentConnectionsConfigure); err != nil {
		t.Fatalf("recent sign-in denied configuration: %v", err)
	}
}
