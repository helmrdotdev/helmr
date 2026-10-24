package auth

import (
	"testing"
	"uuid"
)

func TestCapabilityBoundaries(t *testing.T) {
	cases := []struct {
		cap            Permission
		allows, denies []Permission
	}{
		{CapabilityAgentsManage, []Permission{CapabilityAgentsManage}, []Permission{CapabilityAgentsUse, CapabilitySessionsView, CapabilitySessionsManage, CapabilityDeploymentsPublish, CapabilityDeploymentsPromote}},
		{CapabilityAgentsUse, []Permission{PermissionSessionsCreate, PermissionAgentUse}, []Permission{PermissionSessionsRead, PermissionSessionsSend, PermissionComputersRead, PermissionDeploymentMetadataRead}},
		{CapabilitySessionsView, []Permission{PermissionSessionsRead, PermissionDeploymentMetadataRead}, []Permission{PermissionRunMetadataRead, PermissionSessionsCreate, PermissionSessionsSend, PermissionComputersRead, PermissionComputerCommandCreate, PermissionSecretsWrite}},
		{CapabilitySessionsManage, []Permission{PermissionSessionsRead, PermissionSessionsCancel, PermissionSessionsClose, PermissionSessionsInterrupt}, []Permission{PermissionSessionsCreate, PermissionSessionsSend, PermissionSessionsResume, PermissionAsksAnswer, PermissionComputersCreate, PermissionComputerCommandCreate, PermissionSecretsWrite, PermissionDeploymentsWrite}},
		{CapabilityDeploymentsPublish, []Permission{PermissionDeploymentsWrite, PermissionDeploymentMetadataRead}, []Permission{PermissionDeploymentsPromote, PermissionSessionsRead, PermissionSecretsWrite}},
		{CapabilityDeploymentsPromote, []Permission{PermissionDeploymentsPromote, PermissionDeploymentMetadataRead}, []Permission{PermissionDeploymentsWrite, PermissionSessionsRead}},
		{CapabilityComputersOperate, []Permission{PermissionComputersCreate, PermissionComputersRead, PermissionComputersDelete, PermissionComputersUpdate, PermissionComputerCommandCreate, PermissionSecretMetadataRead}, []Permission{PermissionSecretsWrite, PermissionSessionsRead, PermissionDeploymentsWrite}},
		{CapabilitySecretsManage, []Permission{PermissionSecretsWrite, PermissionSecretMetadataRead}, []Permission{PermissionComputerCommandCreate, PermissionDeploymentsWrite, PermissionSessionsRead}},
	}
	for _, c := range cases {
		t.Run(string(c.cap), func(t *testing.T) {
			for _, operation := range c.allows {
				if !CapabilityAllows(c.cap, operation) {
					t.Errorf("missing operation %s", operation)
				}
			}
			for _, operation := range append(c.denies, PermissionMembersManage, PermissionAPIKeysManage, PermissionProjectsManage) {
				if CapabilityAllows(c.cap, operation) {
					t.Errorf("unexpected operation %s", operation)
				}
			}
		})
	}
}

func TestMachineCapabilitiesDoNotBorrowHumanRoleOrGrant(t *testing.T) {
	org := uuid.NewV7()
	scope := Scope{OrgID: org, ProjectID: uuid.NewV7().String(), EnvironmentID: uuid.NewV7().String()}
	p := Principal{Kind: PrincipalKindAPIKey, OrgID: org, IsOwner: true, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, Permissions: []Permission{CapabilitySessionsView}, Grants: []PermissionGrant{{ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, Permission: CapabilitySecretsManage}}}
	if !p.HasPermission(PermissionSessionsRead, scope) {
		t.Fatal("explicit capability denied")
	}
	for _, operation := range []Permission{PermissionSecretsWrite, PermissionAPIKeysManage, PermissionSessionsCreate} {
		if p.HasPermission(operation, scope) {
			t.Fatalf("borrowed authority: %s", operation)
		}
	}
	p.IsOwner = false
	if !p.HasPermission(PermissionSessionsRead, scope) {
		t.Fatal("creator role affected machine authority")
	}
	for _, other := range []Scope{{OrgID: org}, {OrgID: uuid.NewV7(), ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID}, {OrgID: org, ProjectID: scope.ProjectID, EnvironmentID: uuid.NewV7().String()}, {OrgID: org, ProjectID: uuid.NewV7().String(), EnvironmentID: scope.EnvironmentID}} {
		if p.HasPermission(PermissionSessionsRead, other) {
			t.Fatal("scope escaped")
		}
	}
}

func TestHumanGrantUnionAndAgentScope(t *testing.T) {
	org := uuid.NewV7()
	scope := Scope{OrgID: org, ProjectID: uuid.NewV7().String(), EnvironmentID: uuid.NewV7().String(), AgentID: uuid.NewV7().String()}
	p := Principal{Kind: PrincipalKindSession, OrgID: org, Grants: []PermissionGrant{{ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, AgentID: scope.AgentID, Permission: CapabilityAgentsUse}}}
	if !p.HasPermission(PermissionSessionsCreate, scope) || p.HasPermission(PermissionSessionsRead, scope) {
		t.Fatal("Agent use scope mismatch")
	}
	other := scope
	other.AgentID = uuid.NewV7().String()
	if p.HasPermission(PermissionSessionsCreate, other) {
		t.Fatal("other Agent allowed")
	}
	p.Grants = append(p.Grants, PermissionGrant{ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, Permission: CapabilitySessionsView})
	if !p.HasPermission(PermissionSessionsRead, other) || p.HasPermission(PermissionSessionsSend, scope) {
		t.Fatal("union broadened operation")
	}
	p.Grants = nil
	if p.HasPermission(PermissionSessionsRead, scope) {
		t.Fatal("member has implicit workload authority")
	}
	p.IsOwner = true
	for _, permission := range AllPermissions() {
		if p.HasPermission(permission, scope) || p.HasPermission(permission, Scope{OrgID: org}) {
			t.Fatalf("owner borrowed authority: %s", permission)
		}
	}

}

func TestAPIKeyGrantCatalog(t *testing.T) {
	for _, entry := range CapabilityCatalog() {
		if permission, ok := ParseAPIKeyGrant(" " + string(entry.ID) + " "); ok != IsMachineCapability(entry.ID) || permission != entry.ID {
			t.Fatal("catalog capability rejected")
		}
	}
	for _, value := range []string{"", "*", "unsupported.permission"} {
		if _, ok := ParseAPIKeyGrant(value); ok {
			t.Fatalf("unsupported grant %q accepted", value)
		}
	}
}

func TestOrganizationAndKeyGrantsHaveExactScope(t *testing.T) {
	org := uuid.NewV7()
	env := Scope{OrgID: org, ProjectID: uuid.NewV7().String(), EnvironmentID: uuid.NewV7().String()}
	organization := Scope{OrgID: org}
	p := Principal{Kind: PrincipalKindSession, OrgID: org, Grants: []PermissionGrant{{Permission: CapabilityAccessManage}, {Permission: CapabilityOrganizationManage}, {ProjectID: env.ProjectID, EnvironmentID: env.EnvironmentID, Permission: CapabilityKeysManage}}}
	if !p.HasPermission(PermissionMembersManage, organization) || !p.HasPermission(PermissionProjectsManage, organization) || !p.HasPermission(PermissionAPIKeysManage, env) {
		t.Fatal("explicit administrative authority denied")
	}
	if p.HasPermission(PermissionMembersManage, env) || p.HasPermission(PermissionProjectsManage, env) || p.HasPermission(PermissionAPIKeysManage, organization) || p.HasPermission(PermissionSessionsRead, env) {
		t.Fatal("administrative grant crossed its boundary")
	}
	p.Kind = PrincipalKindAPIKey
	p.ProjectID = env.ProjectID
	p.EnvironmentID = env.EnvironmentID
	p.Permissions = []Permission{CapabilityAccessManage, CapabilityOrganizationManage, CapabilityKeysManage}
	for _, permission := range []Permission{PermissionMembersManage, PermissionProjectsManage, PermissionAPIKeysManage} {
		if p.HasPermission(permission, env) || p.HasPermission(permission, organization) {
			t.Fatal("machine obtained human administration")
		}
	}
}

func TestAgentManagementScopeAndMachineExclusion(t *testing.T) {
	org := uuid.NewV7()
	scope := Scope{OrgID: org, ProjectID: uuid.NewV7().String(), EnvironmentID: uuid.NewV7().String(), AgentID: uuid.NewV7().String()}
	p := Principal{Kind: PrincipalKindSession, OrgID: org, Grants: []PermissionGrant{{ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, AgentID: scope.AgentID, Permission: CapabilityAgentsManage}}}
	if !p.HasPermission(CapabilityAgentsManage, scope) {
		t.Fatal("exact Agent denied")
	}
	other := scope
	other.AgentID = uuid.NewV7().String()
	if p.HasPermission(CapabilityAgentsManage, other) {
		t.Fatal("other Agent admitted")
	}
	p.Kind = PrincipalKindAPIKey
	p.ProjectID = scope.ProjectID
	p.EnvironmentID = scope.EnvironmentID
	p.Permissions = []Permission{CapabilityAgentsManage}
	if IsMachineCapability(CapabilityAgentsManage) || p.HasPermission(CapabilityAgentsManage, scope) {
		t.Fatal("machine acquired human configuration authority")
	}
}

func TestSessionExecutionRequiresUseAndManagementOnSameAgent(t *testing.T) {
	env, project, first, second := uuid.NewV7().String(), uuid.NewV7().String(), uuid.NewV7().String(), uuid.NewV7().String()
	p := Principal{Kind: PrincipalKindSession, Grants: []PermissionGrant{
		{ProjectID: project, EnvironmentID: env, AgentID: first, Permission: CapabilitySessionsManage},
		{ProjectID: project, EnvironmentID: env, AgentID: second, Permission: CapabilityAgentsUse},
	}}
	scope := Scope{ProjectID: project, EnvironmentID: env, AgentID: first}
	if !p.HasPermission(PermissionSessionsRead, scope) || !p.HasPermission(PermissionSessionsClose, scope) {
		t.Fatal("management must allow reading and closing")
	}
	for _, op := range []Permission{PermissionSessionsSend, PermissionAsksAnswer, PermissionSessionsResume, PermissionSessionsCreate} {
		if p.HasPermission(op, scope) {
			t.Fatalf("borrowed use from another Agent: %s", op)
		}
	}
	p.Grants = append(p.Grants, PermissionGrant{ProjectID: project, EnvironmentID: env, AgentID: first, Permission: CapabilityAgentsUse})
	for _, op := range []Permission{PermissionSessionsSend, PermissionAsksAnswer, PermissionSessionsResume, PermissionSessionsCreate} {
		if !p.HasPermission(op, scope) {
			t.Fatalf("separate matching assignments must combine: %s", op)
		}
	}
	all, agents := p.AgentPermissionScope(PermissionSessionsRead, Scope{ProjectID: project, EnvironmentID: env})
	if all || len(agents) != 1 || agents[0].String() != first {
		t.Fatalf("read scope escaped: %v %v", all, agents)
	}
}
