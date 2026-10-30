package auth

import "slices"

// PermissionDefinition describes one independently assignable product action.
// Roles contain explicit action IDs; actions never expand into other permissions.
type PermissionDefinition struct {
	ResourceType     string            `json:"resource_type"`
	ID               Permission        `json:"id"`
	Name             string            `json:"name"`
	Description      string            `json:"description"`
	ScopeTypes       []PermissionScope `json:"scope_types"`
	APIKeyAssignable bool              `json:"api_key_assignable"`
	AssignmentLevels []PermissionScope `json:"assignment_levels"`
}

type PermissionScope string

const (
	ScopeOrganization PermissionScope = "organization"
	ScopeEnvironment  PermissionScope = "environment"
	ScopeAgent        PermissionScope = "agent"
)

const (
	PermissionAuditEventsRead           Permission = "audit_events.read"
	PermissionAgentsRead                Permission = "agents.read"
	PermissionAgentsUse                 Permission = "agents.use"
	PermissionAgentsDelete              Permission = "agents.delete"
	PermissionAgentsRestore             Permission = "agents.restore"
	PermissionAgentConnectionsRead      Permission = "agent_connections.read"
	PermissionAgentConnectionsConfigure Permission = "agent_connections.configure"
	PermissionAgentConnectionsDelete    Permission = "agent_connections.delete"
	PermissionSessionsRead              Permission = "sessions.read"
	PermissionSessionsCreateKeyed       Permission = "sessions.create_keyed"
	PermissionSessionsSend              Permission = "sessions.send"
	PermissionSessionsAnswer            Permission = "sessions.answer"
	PermissionSessionsResume            Permission = "sessions.resume"
	PermissionSessionsInterrupt         Permission = "sessions.interrupt"
	PermissionSessionsCancel            Permission = "sessions.cancel"
	PermissionSessionsClose             Permission = "sessions.close"
	PermissionSessionsCheckDelivery     Permission = "sessions.check_delivery"
	PermissionSessionsAbandonDelivery   Permission = "sessions.abandon_delivery"
	PermissionDeploymentsRead           Permission = "deployments.read"
	PermissionDeploymentsPublish        Permission = "deployments.publish"
	PermissionDeploymentsPromote        Permission = "deployments.promote"
	PermissionTemplatesRead             Permission = "templates.read"
	PermissionTemplatesRefresh          Permission = "templates.refresh"
	PermissionTriggersRead              Permission = "triggers.read"
	PermissionComputersRead             Permission = "computers.read"
	PermissionCommandsReadOutput        Permission = "commands.read_output"
	PermissionComputersExecute          Permission = "computers.execute"
	PermissionCommandsCancel            Permission = "commands.cancel"
	PermissionComputersCreate           Permission = "computers.create"
	PermissionComputersWake             Permission = "computers.wake"
	PermissionComputersHibernate        Permission = "computers.hibernate"
	PermissionComputersUpdate           Permission = "computers.update"
	PermissionComputersDelete           Permission = "computers.delete"
	PermissionSecretsRead               Permission = "secrets.read"
	PermissionSecretsCreate             Permission = "secrets.create"
	PermissionSecretsRotate             Permission = "secrets.rotate"
	PermissionSecretsRevoke             Permission = "secrets.revoke"
	PermissionAPIKeysRead               Permission = "api_keys.read"
	PermissionAPIKeysCreate             Permission = "api_keys.create"
	PermissionAPIKeysUpdate             Permission = "api_keys.update"
	PermissionAPIKeysRotate             Permission = "api_keys.rotate"
	PermissionAPIKeysRevoke             Permission = "api_keys.revoke"
	PermissionProjectsRead              Permission = "projects.read"
	PermissionProjectsCreate            Permission = "projects.create"
	PermissionProjectsUpdate            Permission = "projects.update"
	PermissionEnvironmentsRead          Permission = "environments.read"
	PermissionEnvironmentsCreate        Permission = "environments.create"
	PermissionEnvironmentsUpdate        Permission = "environments.update"
	PermissionMembersRead               Permission = "members.read"
	PermissionMembersRemove             Permission = "members.remove"
	PermissionInvitationsRead           Permission = "invitations.read"
	PermissionInvitationsCreate         Permission = "invitations.create"
	PermissionInvitationsRevoke         Permission = "invitations.revoke"
	PermissionGroupsRead                Permission = "groups.read"
	PermissionGroupsCreate              Permission = "groups.create"
	PermissionGroupsUpdate              Permission = "groups.update"
	PermissionGroupsDelete              Permission = "groups.delete"
	PermissionGroupsAddMember           Permission = "groups.add_member"
	PermissionGroupsRemoveMember        Permission = "groups.remove_member"
	PermissionRolesRead                 Permission = "roles.read"
	PermissionRolesCreate               Permission = "roles.create"
	PermissionRolesUpdate               Permission = "roles.update"
	PermissionRolesDelete               Permission = "roles.delete"
	PermissionAssignmentsRead           Permission = "assignments.read"
	PermissionAssignmentsCreate         Permission = "assignments.create"
	PermissionAssignmentsUpdate         Permission = "assignments.update"
	PermissionAssignmentsDelete         Permission = "assignments.delete"
)

var permissionCatalog = []PermissionDefinition{
	{"audit_events", PermissionAuditEventsRead, "Read audit events", "Read security facts across the assigned organization or environment, including actor identity and IP; does not grant execution content access.", []PermissionScope{ScopeOrganization, ScopeEnvironment}, true, nil},
	{"agents", PermissionAgentsRead, "Read agents", "List/read the authorized Agent's definition and retained status, including deleted status; not other Agents or full deployment catalogs.", []PermissionScope{ScopeAgent}, true, nil},
	{"agents", PermissionAgentsUse, "Use agents", "Start ordinary own-origin conversations and perform the established own-session operations; participate in authorized connected shared conversations. Includes only identity/definition needed to use the target Agent.", []PermissionScope{ScopeAgent}, true, nil},
	{"agents", PermissionAgentsDelete, "Delete agents", "Preview and apply explicit Agent deletion, including its fixed cleanup effects; current-deployment and unsettled-work blockers remain.", []PermissionScope{ScopeAgent}, false, nil},
	{"agents", PermissionAgentsRestore, "Restore agents", "Preview and restore a deleted Agent identity. Does not restore removed selected grants or Slack connections; broad grants apply again and existing parent Sessions may create children immediately, before another deployment includes the Agent.", []PermissionScope{ScopeAgent}, false, nil},
	{"agent_connections", PermissionAgentConnectionsRead, "Read agent connections", "Read the Agent's Slack connection status/setup metadata, without credential values.", []PermissionScope{ScopeAgent}, false, nil},
	{"agent_connections", PermissionAgentConnectionsConfigure, "Configure agent connections", "Register the Agent's connection, replace supplied app credentials and authorize or reauthorize installation through verified OAuth. Does not grant credential export or disconnection.", []PermissionScope{ScopeAgent}, false, nil},
	{"agent_connections", PermissionAgentConnectionsDelete, "Delete agent connections", "Disconnect the Agent's connection and perform its fixed cleanup.", []PermissionScope{ScopeAgent}, false, nil},
	{"sessions", PermissionSessionsRead, "Read sessions", "Read/list matching Sessions, Turns, events, Asks, updates and execution history. Includes conversation contents and the existing human delivery-record view.", []PermissionScope{ScopeAgent}, true, nil},
	{"sessions", PermissionSessionsCreateKeyed, "Create keyed sessions", "Supply a caller-defined Session key or the existing administrative channel identifier on scoped Session creation; also requires agents.use. Does not grant somebody else's origin.", []PermissionScope{ScopeAgent}, true, nil},
	{"sessions", PermissionSessionsSend, "Send Session input", "Enqueue input or send a message to a matching Session/Turn; also requires agents.use.", []PermissionScope{ScopeAgent}, true, nil},
	{"sessions", PermissionSessionsAnswer, "Answer Session questions", "Answer an Ask; also requires agents.use.", []PermissionScope{ScopeAgent}, true, nil},
	{"sessions", PermissionSessionsResume, "Resume sessions", "Resume matching suspended work; also requires agents.use.", []PermissionScope{ScopeAgent}, true, nil},
	{"sessions", PermissionSessionsInterrupt, "Interrupt sessions", "Interrupt the addressed Session/Turn using existing interruption semantics.", []PermissionScope{ScopeAgent}, true, nil},
	{"sessions", PermissionSessionsCancel, "Cancel sessions", "Cancel the addressed Session using existing cancellation semantics.", []PermissionScope{ScopeAgent}, true, nil},
	{"sessions", PermissionSessionsClose, "Close sessions", "Inspect the close-scope preview and request Session closure using existing child/settlement constraints.", []PermissionScope{ScopeAgent}, true, nil},
	{"sessions", PermissionSessionsCheckDelivery, "Check Session delivery", "Request Slack publication reconciliation for an uncertain delivery; also requires Session read. No resend or new execution authority.", []PermissionScope{ScopeAgent}, false, nil},
	{"sessions", PermissionSessionsAbandonDelivery, "Abandon Session delivery", "Record the existing delivery-abandonment disposition; also requires Session read. No resend or new execution authority.", []PermissionScope{ScopeAgent}, false, nil},
	{"deployments", PermissionDeploymentsRead, "Read deployments", "Read deployment list, current deployment, record and events; not Session conversations or Computer stdout/stderr.", []PermissionScope{ScopeEnvironment}, true, nil},
	{"deployments", PermissionDeploymentsPublish, "Publish deployments", "Obtain upload plan, finalize bundle and register immutable deployment definitions/code. Publication/preparation may execute code with configured credentials.", []PermissionScope{ScopeEnvironment}, true, nil},
	{"deployments", PermissionDeploymentsPromote, "Promote deployments", "Activate or roll back to an existing deployment; changes current definitions and scheduled admission. No upload authority.", []PermissionScope{ScopeEnvironment}, true, nil},
	{"templates", PermissionTemplatesRead, "Read templates", "Read Template definitions and preparation-build status for permitted deployment selection. No broad Agent/Session authority.", []PermissionScope{ScopeEnvironment}, true, nil},
	{"templates", PermissionTemplatesRefresh, "Refresh templates", "Start explicit preparation refresh for a current Template, including configured preparation credential use.", []PermissionScope{ScopeEnvironment}, true, nil},
	{"triggers", PermissionTriggersRead, "Read triggers", "Read matching trigger definitions, input payloads and occurrence records. This is potentially sensitive input, not a metadata-only feed. Session links still require Session authority to follow.", []PermissionScope{ScopeAgent}, true, nil},
	{"computers", PermissionComputersRead, "Read computers", "List/read Computer state, resource diagnostics, execution-scope preview, usage IDs and Command status/outcome. No conversation/file/stdout/stderr content.", []PermissionScope{ScopeEnvironment}, true, nil},
	{"commands", PermissionCommandsReadOutput, "Read Command output", "Read retained Command stdout/stderr. This can disclose sensitive content even without execution.", []PermissionScope{ScopeEnvironment}, true, nil},
	{"computers", PermissionComputersExecute, "Execute computers", "Submit Commands on an existing Computer or explicitly place an authorized Session there. Does not implicitly grant historical output/status APIs; standard debugging Roles include them.", []PermissionScope{ScopeEnvironment}, true, nil},
	{"commands", PermissionCommandsCancel, "Cancel Commands", "Cancel a Command in the environment, regardless of creator. No new owner-only cancellation mechanism.", []PermissionScope{ScopeEnvironment}, true, nil},
	{"computers", PermissionComputersCreate, "Create computers", "Provision from an allowed definition, including requested Secret bindings and initial retention, under existing quota and creation rules. This is trusted credential-bearing provisioning.", []PermissionScope{ScopeEnvironment}, true, nil},
	{"computers", PermissionComputersWake, "Wake computers", "Explicitly wake a Computer, subject to existing state constraints.", []PermissionScope{ScopeEnvironment}, true, nil},
	{"computers", PermissionComputersHibernate, "Hibernate computers", "Explicitly hibernate a Computer with existing busy/settlement checks.", []PermissionScope{ScopeEnvironment}, true, nil},
	{"computers", PermissionComputersUpdate, "Update computers", "Change deletion-after-unused policy; shortening it can promptly cause deletion.", []PermissionScope{ScopeEnvironment}, true, nil},
	{"computers", PermissionComputersDelete, "Delete computers", "Delete the Computer through the platform API with existing settlement rules.", []PermissionScope{ScopeEnvironment}, true, nil},
	{"secrets", PermissionSecretsRead, "Read secrets", "Read Secret names/IDs, versions and non-value metadata. Never retrieve stored plaintext.", []PermissionScope{ScopeEnvironment}, true, nil},
	{"secrets", PermissionSecretsCreate, "Create secrets", "Create a Secret using newly supplied material.", []PermissionScope{ScopeEnvironment}, true, nil},
	{"secrets", PermissionSecretsRotate, "Rotate secrets", "Replace/rotate supplied Secret material.", []PermissionScope{ScopeEnvironment}, true, nil},
	{"secrets", PermissionSecretsRevoke, "Revoke secrets", "Revoke a Secret under existing binding/lifecycle constraints.", []PermissionScope{ScopeEnvironment}, true, nil},
	{"api_keys", PermissionAPIKeysRead, "Read api keys", "List key metadata and current non-secret status. Access-assignment UI may see minimal key identity through its target-directory rule.", []PermissionScope{ScopeEnvironment}, false, nil},
	{"api_keys", PermissionAPIKeysCreate, "Create api keys", "Issue a key and reveal its credential once. The new key has no workload Role until separately assigned.", []PermissionScope{ScopeEnvironment}, false, nil},
	{"api_keys", PermissionAPIKeysUpdate, "Update api keys", "Update a key's name and expiration, including extending its usable lifetime; never its Role grants.", []PermissionScope{ScopeEnvironment}, false, nil},
	{"api_keys", PermissionAPIKeysRotate, "Rotate api keys", "Rotate and receive a replacement credential. Can act with that key's assigned authority.", []PermissionScope{ScopeEnvironment}, false, nil},
	{"api_keys", PermissionAPIKeysRevoke, "Revoke api keys", "Revoke a key.", []PermissionScope{ScopeEnvironment}, false, nil},
	{"projects", PermissionProjectsRead, "Read projects", "Read the organization-wide Project directory, full Project metadata and history-retention settings.", []PermissionScope{ScopeOrganization}, false, nil},
	{"projects", PermissionProjectsCreate, "Create projects", "Create a Project and receive resource administration and audit access in its initial Environments.", []PermissionScope{ScopeOrganization}, false, nil},
	{"projects", PermissionProjectsUpdate, "Update projects", "Change a Project's display name and Session history retention. Shortening retention can permanently delete history; no slug/identity reassignment.", []PermissionScope{ScopeOrganization}, false, nil},
	{"environments", PermissionEnvironmentsRead, "Read environments", "Read full Environment metadata in the organization.", []PermissionScope{ScopeOrganization}, false, nil},
	{"environments", PermissionEnvironmentsCreate, "Create environments", "Create an Environment and receive resource administration and audit access in it.", []PermissionScope{ScopeOrganization}, false, nil},
	{"environments", PermissionEnvironmentsUpdate, "Update environments", "Update an Environment's display name and color; no identity/region move.", []PermissionScope{ScopeOrganization}, false, nil},
	{"members", PermissionMembersRead, "Read members", "List current members and identity metadata.", []PermissionScope{ScopeOrganization}, false, nil},
	{"members", PermissionMembersRemove, "Remove members", "Remove membership and its fixed cleanup; last-owner protections remain.", []PermissionScope{ScopeOrganization}, false, nil},
	{"invitations", PermissionInvitationsRead, "Read invitations", "List invitations.", []PermissionScope{ScopeOrganization}, false, nil},
	{"invitations", PermissionInvitationsCreate, "Create invitations", "Invite members; new members receive existing All-members access where configured.", []PermissionScope{ScopeOrganization}, false, nil},
	{"invitations", PermissionInvitationsRevoke, "Revoke invitations", "Revoke an invitation.", []PermissionScope{ScopeOrganization}, false, nil},
	{"groups", PermissionGroupsRead, "Read groups", "List/read Groups and their members.", []PermissionScope{ScopeOrganization}, false, nil},
	{"groups", PermissionGroupsCreate, "Create groups", "Create an empty Group.", []PermissionScope{ScopeOrganization}, false, nil},
	{"groups", PermissionGroupsUpdate, "Update groups", "Rename/update Group metadata, not its members.", []PermissionScope{ScopeOrganization}, false, nil},
	{"groups", PermissionGroupsDelete, "Delete groups", "Delete a Group, its memberships and its assignments, immediately revoking the access they conferred. Unlike Role deletion, this has no in-use blocker.", []PermissionScope{ScopeOrganization}, false, nil},
	{"groups", PermissionGroupsAddMember, "Add Group members", "Add a member to a Group, immediately conveying its assigned Roles.", []PermissionScope{ScopeOrganization}, false, nil},
	{"groups", PermissionGroupsRemoveMember, "Remove Group members", "Remove a member from a Group.", []PermissionScope{ScopeOrganization}, false, nil},
	{"roles", PermissionRolesRead, "Read roles", "Read Role definitions and the Permission catalog.", []PermissionScope{ScopeOrganization}, false, nil},
	{"roles", PermissionRolesCreate, "Create roles", "Create a custom Role with an explicit Permission set; does not assign it.", []PermissionScope{ScopeOrganization}, false, nil},
	{"roles", PermissionRolesUpdate, "Update roles", "Modify a custom Role; changes the authority of all existing assignments. Standard Roles remain immutable to users.", []PermissionScope{ScopeOrganization}, false, nil},
	{"roles", PermissionRolesDelete, "Delete roles", "Delete a custom Role subject to in-use constraints.", []PermissionScope{ScopeOrganization}, false, nil},
	{"assignments", PermissionAssignmentsRead, "Read assignments", "Read assignments, other members' effective-access explanation and the minimal assignment-target directory.", []PermissionScope{ScopeOrganization}, false, nil},
	{"assignments", PermissionAssignmentsCreate, "Create assignments", "Assign a Role to an eligible subject and valid scope. This can delegate authority the caller does not personally hold, as current Access administration can.", []PermissionScope{ScopeOrganization}, false, nil},
	{"assignments", PermissionAssignmentsUpdate, "Update assignments", "Change an assignment's Role/targets/eligible properties; same delegation power.", []PermissionScope{ScopeOrganization}, false, nil},
	{"assignments", PermissionAssignmentsDelete, "Delete assignments", "Remove an assignment.", []PermissionScope{ScopeOrganization}, false, nil},
}

func PermissionCatalog() []PermissionDefinition {
	result := make([]PermissionDefinition, 0, len(permissionCatalog))
	for _, d := range permissionCatalog {
		result = append(result, copyDefinition(d))
	}
	return result
}

func copyDefinition(d PermissionDefinition) PermissionDefinition {
	d.ScopeTypes = slices.Clone(d.ScopeTypes)
	d.AssignmentLevels = RoleAssignmentLevels([]string{string(d.ID)})
	return d
}

// RoleAssignmentLevels describes where every action in a Role can be assigned.
// Agent targeting is a refinement of one Environment assignment, not a third
// mutually exclusive assignment level.
func RoleAssignmentLevels(permissions []string) []PermissionScope {
	levels := []PermissionScope{}
	if len(permissions) == 0 {
		return levels
	}
	for _, level := range []PermissionScope{ScopeOrganization, ScopeEnvironment} {
		valid := true
		for _, p := range permissions {
			d, ok := permissionIndex[Permission(p)]
			if !ok || !(slices.Contains(d.ScopeTypes, level) || level == ScopeEnvironment && slices.Contains(d.ScopeTypes, ScopeAgent)) {
				valid = false
				break
			}
		}
		if valid {
			levels = append(levels, level)
		}
	}
	return levels
}

func SupportsScope(p Permission, scope PermissionScope) bool {
	d, ok := permissionIndex[p]
	return ok && slices.Contains(d.ScopeTypes, scope)
}

var permissionIndex = func() map[Permission]PermissionDefinition {
	index := make(map[Permission]PermissionDefinition, len(permissionCatalog))
	for _, d := range permissionCatalog {
		index[d.ID] = d
	}
	return index
}()

func PermissionInfo(permission Permission) (PermissionDefinition, bool) {
	d, ok := permissionIndex[permission]
	return copyDefinition(d), ok
}
func IsPermission(p Permission) bool { _, ok := PermissionInfo(p); return ok }
func IsMachinePermission(p Permission) bool {
	d, ok := PermissionInfo(p)
	return ok && d.APIKeyAssignable
}
func PermissionResourceType(p Permission) string { d, _ := PermissionInfo(p); return d.ResourceType }
func IsAgentPermission(p Permission) bool {
	d, ok := PermissionInfo(p)
	return ok && slices.Contains(d.ScopeTypes, ScopeAgent)
}
func AgentPermissionIDs() []string {
	ids := []string{}
	for _, d := range permissionCatalog {
		if slices.Contains(d.ScopeTypes, ScopeAgent) {
			ids = append(ids, string(d.ID))
		}
	}
	return ids
}
func AllPermissions() []Permission {
	ids := []Permission{}
	for _, d := range permissionCatalog {
		ids = append(ids, d.ID)
	}
	return ids
}
