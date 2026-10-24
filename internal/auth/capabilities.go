package auth

// Capabilities are the public, assignable permission vocabulary. Operation
// permissions stay internal; Roles and API keys store the catalog IDs; API keys accept only workload capabilities.
const (
	CapabilityAccessManage       Permission = "access.manage"
	CapabilityOrganizationManage Permission = "organization.manage"
	CapabilityAgentsManage       Permission = "agents.manage"
	CapabilityKeysManage         Permission = "keys.manage"
	CapabilityAgentsUse          Permission = "agents.use"
	CapabilitySessionsView       Permission = "sessions.view"
	CapabilitySessionsManage     Permission = "sessions.manage"
	CapabilityDeploymentsPublish Permission = "deployments.publish"
	CapabilityDeploymentsPromote Permission = "deployments.promote"
	CapabilityComputersOperate   Permission = "computers.operate"
	CapabilitySecretsManage      Permission = "secrets.manage"
)

type CapabilityDefinition struct {
	ResourceType string       `json:"resource_type"`
	ID           Permission   `json:"id"`
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	Included     []Permission `json:"included"`
}

func CapabilityCatalog() []CapabilityDefinition {
	catalog := []CapabilityDefinition{
		{"organization", CapabilityAccessManage, "Manage access", "Manage members, invitations, Groups, Roles and assignments across the organization. This can delegate powerful workload access.", nil},
		{"organization", CapabilityOrganizationManage, "Manage organization", "Create and manage Projects and environments. Does not grant workload or access administration.", nil},
		{"api_keys", CapabilityKeysManage, "Manage API keys", "Create and revoke organization-owned API keys in this environment, including keys with any workload permission.", nil},
		{"agents", CapabilityAgentsManage, "Configure agents", "Configure assigned Agent settings and chat connections. Does not grant Agent use, session history or deployment changes.", nil},
		{"agents", CapabilityAgentsUse, "Use agents", "Use assigned Agents in your own conversations and connected shared channels. Session viewers and operators can access these conversations.", nil},
		{"agents", CapabilitySessionsView, "View all sessions", "Read all conversations and execution history for assigned Agents, including other people's personal sessions. Does not grant Computer access.", nil},
		{"agents", CapabilitySessionsManage, "Manage all sessions", "View, stop and close all sessions for assigned Agents. Sending input, answering questions or resuming also requires Use agents for that Agent.", nil},
		{"deployments", CapabilityDeploymentsPublish, "Register deployments", "Upload code and register deployments. Workload code can exercise configured service credentials.", nil},
		{"deployments", CapabilityDeploymentsPromote, "Activate deployments", "Activate or roll back registered deployments. Does not include uploading code or reading run logs.", nil},
		{"computers", CapabilityComputersOperate, "Operate computers", "Create, execute commands, inspect files and remove computers. This is trusted access to workload credentials.", nil},
		{"secrets", CapabilitySecretsManage, "Manage secrets", "Create, rotate and revoke secrets. Stored secret values cannot be read through the API.", nil},
	}
	for i := range catalog {
		catalog[i].Included = IncludedCapabilities(catalog[i].ID)
	}
	return catalog
}

func IncludedCapabilities(capability Permission) []Permission {
	if capability == CapabilitySessionsManage {
		return []Permission{capability, CapabilitySessionsView}
	}
	return []Permission{capability}
}

func IsCapability(permission Permission) bool {
	for _, definition := range CapabilityCatalog() {
		if definition.ID == permission {
			return true
		}
	}
	return false
}

func CapabilityAllows(capability, operation Permission) bool {
	if capability == operation && IsCapability(capability) {
		return true
	}
	switch capability {
	case CapabilityAccessManage:
		return operation == PermissionMembersManage
	case CapabilityOrganizationManage:
		return operation == PermissionProjectsManage
	case CapabilityKeysManage:
		return operation == PermissionAPIKeysManage
	case CapabilitySessionsManage:
		for _, included := range IncludedCapabilities(capability)[1:] {
			if CapabilityAllows(included, operation) {
				return true
			}
		}
		switch operation {
		case PermissionSessionsClose, PermissionSessionsCancel, PermissionSessionsInterrupt:
			return true
		}
	case CapabilitySessionsView:
		return operation == PermissionSessionsRead || operation == PermissionDeploymentMetadataRead
	case CapabilityAgentsUse:
		// Own-session access is checked against immutable origin by the Session
		// owner, never inferred from this environment/Agent permission alone.
		return operation == PermissionSessionsCreate || operation == PermissionAgentUse
	case CapabilityDeploymentsPublish:
		return operation == PermissionDeploymentsWrite || operation == PermissionDeploymentMetadataRead
	case CapabilityDeploymentsPromote:
		return operation == PermissionDeploymentsPromote || operation == PermissionDeploymentMetadataRead
	case CapabilityComputersOperate:
		switch operation {
		case PermissionRunMetadataRead, PermissionDeploymentMetadataRead, PermissionSecretMetadataRead, PermissionComputersCreate, PermissionComputersRead, PermissionComputersDelete, PermissionComputersUpdate, PermissionComputerCommandCreate:
			return true
		}
	case CapabilitySecretsManage:
		return operation == PermissionSecretsWrite || operation == PermissionSecretMetadataRead
	}
	return false
}

// PermissionGrant is a request snapshot. Durable operation owners reauthorize
// against current state at admission; this is not an effective-access cache.
type PermissionGrant struct {
	ProjectID     string     `json:"projectId"`
	EnvironmentID string     `json:"environmentId"`
	AgentID       string     `json:"agentId,omitempty"`
	Permission    Permission `json:"permission"`
}

func (g PermissionGrant) allows(operation Permission, scope Scope) bool {
	if IsOrganizationCapability(g.Permission) {
		return g.ProjectID == "" && g.EnvironmentID == "" && g.AgentID == "" && scope.ProjectID == "" && scope.EnvironmentID == "" && scope.AgentID == "" && CapabilityAllows(g.Permission, operation)
	}
	return scope.ProjectID != "" && scope.EnvironmentID != "" &&
		g.ProjectID == scope.ProjectID && g.EnvironmentID == scope.EnvironmentID &&
		(g.AgentID == "" || (scope.AgentID != "" && g.AgentID == scope.AgentID)) &&
		CapabilityAllows(g.Permission, operation)
}

func IsOrganizationCapability(permission Permission) bool {
	return permission == CapabilityAccessManage || permission == CapabilityOrganizationManage
}

func IsMachineCapability(permission Permission) bool {
	return IsCapability(permission) && !IsOrganizationCapability(permission) && permission != CapabilityKeysManage && permission != CapabilityAgentsManage
}

// CapabilityResourceType is the catalog-owned target category for an action.
func CapabilityResourceType(permission Permission) string {
	for _, d := range CapabilityCatalog() {
		if d.ID == permission {
			return d.ResourceType
		}
	}
	return ""
}
