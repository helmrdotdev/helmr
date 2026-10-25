package api

// Permission management uses organization-owned roles and groups. Resource scope
// belongs to each grant, not to a role definition.
type PermissionRole struct {
	APIKeyAssignable bool     `json:"api_key_assignable"`
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	BuiltinKey       *string  `json:"builtin_key"`
	Permissions      []string `json:"permissions"`
	GrantCount       int      `json:"grant_count"`
	GroupCount       int      `json:"group_count"`
	Revision         int64    `json:"revision"`
}
type PermissionDefinition struct {
	ResourceType     string `json:"resource_type"`
	ID               string `json:"id"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	ScopeType        string `json:"scope_type"`
	APIKeyAssignable bool   `json:"api_key_assignable"`
}
type PermissionRolesResponse struct {
	NextCursor string                 `json:"next_cursor,omitempty"`
	Roles      []PermissionRole       `json:"roles"`
	Catalog    []PermissionDefinition `json:"catalog"`
}
type PermissionRoleInput struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Permissions []string `json:"permissions"`
}
type PermissionRolePatch struct {
	Name        *string   `json:"name,omitempty"`
	Description *string   `json:"description,omitempty"`
	Permissions *[]string `json:"permissions,omitempty"`
}
type PermissionGroup struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	MemberIDs []string `json:"member_ids"`
	Revision  int64    `json:"revision"`
}
type PermissionGroupsResponse struct {
	NextCursor string            `json:"next_cursor,omitempty"`
	Groups     []PermissionGroup `json:"groups"`
}
type PermissionGroupMembersResponse struct {
	NextCursor string   `json:"next_cursor,omitempty"`
	MemberIDs  []string `json:"member_ids"`
}
type GrantTarget struct {
	ResourceType string   `json:"resource_type"`
	Mode         string   `json:"mode"`
	AgentIDs     []string `json:"agent_ids,omitempty"`
	AgentKeys    []string `json:"agent_keys,omitempty"`
	Permissions  []string `json:"permissions"`
}
type RoleGrant struct {
	APIKeyID     *string       `json:"api_key_id"`
	APIKeyName   string        `json:"api_key_name"`
	RoleRevision int64         `json:"role_revision"`
	Revision     int64         `json:"revision"`
	AgentsMode   string        `json:"agents_mode"`
	AgentIDs     []string      `json:"agent_ids"`
	AgentKeys    []string      `json:"agent_keys"`
	Targets      []GrantTarget `json:"targets"`

	MemberID      *string  `json:"member_id"`
	MemberName    string   `json:"member_name"`
	ID            string   `json:"id"`
	Created       bool     `json:"created"`
	SubjectKind   string   `json:"subject_kind"`
	GroupID       *string  `json:"group_id"`
	GroupName     string   `json:"group_name"`
	RoleID        string   `json:"role_id"`
	RoleName      string   `json:"role_name"`
	ProjectID     string   `json:"project_id"`
	Project       string   `json:"project"`
	EnvironmentID string   `json:"environment_id"`
	Environment   string   `json:"environment"`
	TargetKind    string   `json:"target_kind"`
	Permissions   []string `json:"permissions"`
}
type RoleGrantsResponse struct {
	Grants     []RoleGrant `json:"grants"`
	NextCursor string      `json:"next_cursor,omitempty"`
}
type GrantSubject struct {
	APIKeyID *string `json:"api_key_id,omitempty"`
	Type     string  `json:"type"`
	MemberID *string `json:"member_id,omitempty"`
	GroupID  *string `json:"group_id,omitempty"`
}
type RoleGrantInput struct {
	Subject              GrantSubject `json:"subject"`
	RoleID               string       `json:"role_id"`
	ProjectID            *string      `json:"project_id,omitempty"`
	EnvironmentID        *string      `json:"environment_id,omitempty"`
	AgentsMode           string       `json:"agents_mode"`
	AgentIDs             []string     `json:"agent_ids"`
	ExpectedRoleRevision int64        `json:"expected_role_revision"`
}
type AccessTargetsResponse struct {
	ProjectID string         `json:"project_id"`
	Targets   []AccessTarget `json:"targets"`
}
type AccessTarget struct {
	EnvironmentID string        `json:"environment_id"`
	Environment   string        `json:"environment"`
	Name          string        `json:"name"`
	Agents        []AccessAgent `json:"agents"`
}
type AccessAgent struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

type MemberPermissionSource struct {
	SubjectKind string   `json:"subject_kind"`
	MemberID    string   `json:"member_id,omitempty"`
	TargetKind  string   `json:"target_kind,omitempty"`
	AgentID     string   `json:"agent_id,omitempty"`
	Agent       string   `json:"agent,omitempty"`
	Kind        string   `json:"kind"`
	GrantID     string   `json:"grant_id,omitempty"`
	GroupID     string   `json:"group_id,omitempty"`
	GroupName   string   `json:"group_name,omitempty"`
	RoleID      string   `json:"role_id,omitempty"`
	RoleName    string   `json:"role_name,omitempty"`
	Permissions []string `json:"permissions"`
}
type MemberPermissionScope struct {
	Type        string                   `json:"type"`
	Project     string                   `json:"project"`
	Environment string                   `json:"environment"`
	Agent       string                   `json:"agent,omitempty"`
	Permissions []string                 `json:"permissions"`
	Sources     []MemberPermissionSource `json:"sources"`
}
type MemberPermissionsResponse struct {
	Scopes       []MemberPermissionScope `json:"scopes"`
	UserID       string                  `json:"user_id"`
	MembershipID string                  `json:"membership_id"`
	IsOwner      bool                    `json:"is_owner"`
	Status       string                  `json:"status"`
	Notice       string                  `json:"notice"`
}
