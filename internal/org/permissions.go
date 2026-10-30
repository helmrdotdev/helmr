package org

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

var (
	ErrAccessNotFound  = errors.New("group, role or assignment not found")
	ErrAccessNameInUse = errors.New("name is already in use")
	ErrStandardRole    = errors.New("standard roles cannot be changed; copy to a custom role")
	ErrRoleInUse       = errors.New("remove this role's assignments before deleting it")
	ErrRoleTarget      = errors.New("every permission in the role must support all assigned targets")
	ErrGrantExists     = errors.New("assignment already exists with different targets; read it before editing")
	ErrStaleRevision   = errors.New("the object changed; read it again before editing")
)

type RoleGrantConflict struct {
	Cause    error
	GrantIDs []string
	Revision int64
}

func (e *RoleGrantConflict) Error() string { return e.Cause.Error() }
func (e *RoleGrantConflict) Unwrap() error { return e.Cause }

type PermissionRole struct {
	AssignmentLevels []auth.PermissionScope `json:"assignment_levels"`
	APIKeyAssignable bool                   `json:"api_key_assignable"`
	ID               uuid.UUID              `json:"id"`
	Name             string                 `json:"name"`
	Description      string                 `json:"description"`
	BuiltinKey       *string                `json:"builtin_key"`
	Permissions      []string               `json:"permissions"`
	GrantCount       int                    `json:"grant_count"`
	GroupCount       int                    `json:"group_count"`
	UpdatedAt        time.Time              `json:"updated_at"`
	Revision         int64                  `json:"revision"`
}
type RoleInput struct {
	ExpectedRevision string   `json:"-"`
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	Permissions      []string `json:"permissions"`
}
type PermissionGroup struct {
	UpdatedAt time.Time   `json:"updated_at"`
	Revision  int64       `json:"revision"`
	ID        uuid.UUID   `json:"id"`
	Name      string      `json:"name"`
	MemberIDs []uuid.UUID `json:"member_ids"`
}
type GrantTarget struct {
	ResourceType string      `json:"resource_type"`
	Mode         string      `json:"mode"`
	AgentIDs     []uuid.UUID `json:"agent_ids,omitempty"`
	AgentKeys    []string    `json:"agent_keys,omitempty"`
	Permissions  []string    `json:"permissions"`
}
type RoleGrant struct {
	APIKeyID     *uuid.UUID    `json:"api_key_id"`
	APIKeyName   string        `json:"api_key_name"`
	RoleRevision int64         `json:"role_revision"`
	Revision     int64         `json:"revision"`
	AgentsMode   string        `json:"agents_mode"`
	AgentIDs     []uuid.UUID   `json:"agent_ids"`
	AgentKeys    []string      `json:"agent_keys"`
	Targets      []GrantTarget `json:"targets"`

	Created         bool       `json:"created"`
	GroupName       string     `json:"group_name"`
	Project         string     `json:"project"`
	Environment     string     `json:"environment"`
	ID              uuid.UUID  `json:"id"`
	SubjectKind     string     `json:"subject_kind"`
	GroupID         *uuid.UUID `json:"group_id"`
	MemberID        *uuid.UUID `json:"member_id"`
	MemberName      string     `json:"member_name"`
	CreatedAt       time.Time  `json:"created_at"`
	CreatedBy       *uuid.UUID `json:"created_by"`
	RoleID          uuid.UUID  `json:"role_id"`
	ProjectID       uuid.UUID  `json:"project_id"`
	EnvironmentID   uuid.UUID  `json:"environment_id"`
	TargetKind      string     `json:"target_kind"`
	RoleName        string     `json:"role_name"`
	ProjectName     string     `json:"project_name"`
	EnvironmentName string     `json:"environment_name"`
	Permissions     []string   `json:"permissions"`
}
type GrantSubject struct {
	APIKeyID *uuid.UUID `json:"api_key_id,omitempty"`
	Type     string     `json:"type"`
	MemberID *uuid.UUID `json:"member_id,omitempty"`
	GroupID  *uuid.UUID `json:"group_id,omitempty"`
}
type GrantInput struct {
	ProjectID            *uuid.UUID   `json:"project_id,omitempty"`
	Subject              GrantSubject `json:"subject"`
	RoleID               uuid.UUID    `json:"role_id"`
	EnvironmentID        *uuid.UUID   `json:"environment_id,omitempty"`
	AgentsMode           string       `json:"agents_mode"`
	AgentIDs             []uuid.UUID  `json:"agent_ids"`
	ExpectedRoleRevision int64        `json:"expected_role_revision"`
	ExpectedRevision     string       `json:"-"`
}

// Lock memberships in stable order before changing membership-owned access.
func lockAccessMembers(ctx context.Context, tx pgx.Tx, m *ManagingMember, permission auth.Permission, targets ...uuid.UUID) error {
	if err := lockActiveMembers(ctx, tx, *m, targets...); err != nil {
		return err
	}
	return m.Authorize(ctx, tx, permission)
}

func lockActiveMembers(ctx context.Context, tx pgx.Tx, m ManagingMember, targets ...uuid.UUID) error {
	ids := append([]uuid.UUID{m.MemberID}, targets...)
	// Account roots precede membership locks, including the target being removed.
	// This prevents a manager waiting for a member lock from blocking source revoke.
	if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id IN (SELECT user_id FROM org_members WHERE org_id=$1 AND id=ANY($2::uuid[])) ORDER BY id FOR SHARE`, m.OrgID, ids); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id,removed_at IS NULL AND (id<>$3 OR (user_id=$4 AND suspended_at IS NULL)) FROM org_members WHERE org_id=$1 AND id=ANY($2::uuid[]) ORDER BY id FOR UPDATE`, m.OrgID, ids, m.MemberID, m.UserID)
	if err != nil {
		return err
	}
	active := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		var enabled bool
		if err := rows.Scan(&id, &enabled); err != nil {
			rows.Close()
			return err
		}
		active[id] = enabled
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !active[m.MemberID] {
		return ErrMemberManagementRequired
	}
	for _, id := range targets {
		if !active[id] {
			return ErrMemberNotFound
		}
	}
	return nil
}

func permissionRoleSnapshot(role PermissionRole) *audit.AccessSnapshot {
	return &audit.AccessSnapshot{Name: role.Name, Description: role.Description, Permissions: role.Permissions, BuiltinKey: role.BuiltinKey}
}
func grantSnapshot(g RoleGrant) *audit.AccessSnapshot {
	s := &audit.AccessSnapshot{GrantID: g.ID.String(), MemberName: g.MemberName, GroupName: g.GroupName, RoleName: g.RoleName, ProjectName: g.ProjectName, EnvironmentName: g.EnvironmentName, AgentsMode: g.AgentsMode, Revision: g.Revision, Permissions: g.Permissions, SubjectKind: g.SubjectKind, RoleID: g.RoleID.String(), ProjectID: g.ProjectID.String(), EnvironmentID: g.EnvironmentID.String(), TargetKind: g.TargetKind}
	if g.MemberID != nil {
		s.MemberID = g.MemberID.String()
	}
	if g.APIKeyID != nil {
		s.APIKeyID = g.APIKeyID.String()
		s.APIKeyName = g.APIKeyName
	}
	if g.GroupID != nil {
		s.GroupID = g.GroupID.String()
	}
	for _, id := range g.AgentIDs {
		s.AgentIDs = append(s.AgentIDs, id.String())
	}
	return s
}
func accessAudit(ctx context.Context, tx pgx.Tx, m ManagingMember, action, kind string, id uuid.UUID, before, after *audit.AccessSnapshot) error {
	e := audit.Event{Evidence: m.Actor, OrgID: m.OrgID, Action: action, TargetKind: kind, TargetID: id, Details: audit.AccessChangeDetails{Before: before, After: after}}
	if kind == "role_grant" {
		snapshot := after
		if snapshot == nil {
			snapshot = before
		}
		if snapshot != nil {
			if snapshot.EnvironmentID != "" {
				id, err := uuid.Parse(snapshot.EnvironmentID)
				if err != nil {
					return err
				}
				e.EnvironmentID = id
			}
			if snapshot.ProjectID != "" {
				id, err := uuid.Parse(snapshot.ProjectID)
				if err != nil {
					return err
				}
				e.ProjectID = id
			}
		}
	}
	return audit.Append(ctx, tx, m.AuditPolicy, e)
}

func validateRoleInput(input RoleInput) (RoleInput, error) {
	name, err := normalizeDisplayName(input.Name)
	if err != nil {
		return input, err
	}
	input.Name = name
	if len(auth.RoleAssignmentLevels(input.Permissions)) == 0 && len(input.Permissions) > 0 {
		return input, invalidInput("permissions have no common assignment scope")
	}
	input.Description = strings.TrimSpace(input.Description)
	if len(input.Description) > 1000 {
		return input, invalidInput("description must be at most 1000 bytes")
	}
	if len(input.Permissions) == 0 {
		return input, invalidInput("select at least one permission")
	}
	input.Permissions = slices.Clone(input.Permissions)
	slices.Sort(input.Permissions)
	input.Permissions = slices.Compact(input.Permissions)
	for _, permission := range input.Permissions {
		if !auth.IsPermission(auth.Permission(permission)) {
			return input, invalidInput("unknown permission %q", permission)
		}
	}
	return input, nil
}
func hasAgentPermissions(permissions []string) bool {
	for _, p := range permissions {
		if auth.IsAgentPermission(auth.Permission(p)) {
			return true
		}
	}
	return false
}

// Agent applicability follows the current Role; only all-versus-selected is stored.
func grantAgentsMode(permissions []string, allAgents bool) string {
	if !hasAgentPermissions(permissions) {
		return "none"
	}
	if allAgents {
		return "all"
	}
	return "selected"
}

func roleSupportsTargets(permissions []string, organization bool, mode string) bool {
	if len(permissions) == 0 {
		return false
	}
	level := auth.ScopeEnvironment
	if organization {
		level = auth.ScopeOrganization
	}
	if !slices.Contains(auth.RoleAssignmentLevels(permissions), level) {
		return false
	}
	return !hasAgentPermissions(permissions) || mode != "none"
}

// machineAssignable checks the whole Role; incompatible capabilities are never
// silently removed when assigning a Role to a machine.
func machineAssignable(permissions []string) bool {
	if len(permissions) == 0 {
		return false
	}
	for _, p := range permissions {
		if !auth.IsMachinePermission(auth.Permission(p)) {
			return false
		}
	}
	return true
}

func grantTargets(g RoleGrant) []GrantTarget {
	targets := []GrantTarget{}
	for _, permission := range g.Permissions {
		resource := auth.PermissionResourceType(auth.Permission(permission))
		i := slices.IndexFunc(targets, func(t GrantTarget) bool { return t.ResourceType == resource })
		if i < 0 {
			t := GrantTarget{ResourceType: resource, Mode: "environment", Permissions: []string{}}
			if g.EnvironmentID == uuid.Nil() {
				t.Mode = "organization"
			}
			if auth.IsAgentPermission(auth.Permission(permission)) {
				t.Mode = g.AgentsMode
				t.AgentIDs = g.AgentIDs
				t.AgentKeys = g.AgentKeys
			}
			targets = append(targets, t)
			i = len(targets) - 1
		}
		targets[i].Permissions = append(targets[i].Permissions, permission)
	}
	return targets
}

func readPermissionRole(ctx context.Context, database db.DBTX, orgID, id uuid.UUID) (PermissionRole, error) {
	r := PermissionRole{Permissions: []string{}}
	err := database.QueryRow(ctx, `SELECT r.id,r.name,r.description,r.builtin_key,r.updated_at,r.revision,
 r.permissions,
 (SELECT count(*) FROM role_grants WHERE org_id=r.org_id AND role_id=r.id),
 (SELECT count(DISTINCT group_id) FROM role_grants WHERE org_id=r.org_id AND role_id=r.id)
 FROM roles r WHERE r.org_id=$1 AND r.id=$2`, orgID, id).Scan(&r.ID, &r.Name, &r.Description, &r.BuiltinKey, &r.UpdatedAt, &r.Revision, &r.Permissions, &r.GrantCount, &r.GroupCount)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrAccessNotFound
	}
	r.APIKeyAssignable = machineAssignable(r.Permissions)
	r.AssignmentLevels = auth.RoleAssignmentLevels(r.Permissions)
	return r, err
}

func ListPermissionRoles(ctx context.Context, database db.TxBeginner, m ManagingMember) ([]PermissionRole, error) {
	return ListPermissionRolesPage(ctx, database, m, uuid.Nil(), 0)
}
func ListPermissionRolesPage(ctx context.Context, database db.TxBeginner, m ManagingMember, after uuid.UUID, limit int) ([]PermissionRole, error) {
	result := []PermissionRole{}
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := m.Authorize(ctx, tx, auth.PermissionRolesRead); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id FROM roles WHERE org_id=$1 AND ($2::uuid='00000000-0000-0000-0000-000000000000' OR id>$2) ORDER BY id LIMIT NULLIF($3,0)`, m.OrgID, after, limit)
		if err != nil {
			return err
		}
		ids := []uuid.UUID{}
		for rows.Next() {
			var id uuid.UUID
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			r, err := readPermissionRole(ctx, tx, m.OrgID, id)
			if err != nil {
				return err
			}
			result = append(result, r)
		}
		return nil
	})
	return result, err
}

// SavePermissionRole creates or edits a custom Role. Locking the Role before
// inspecting assignments also serializes with every assignment writer.
func SavePermissionRole(ctx context.Context, database db.TxBeginner, m ManagingMember, id uuid.UUID, input RoleInput) (PermissionRole, error) {
	permission := auth.PermissionRolesCreate
	if id != uuid.Nil() {
		permission = auth.PermissionRolesUpdate
	}
	input, err := validateRoleInput(input)
	if err != nil {
		return PermissionRole{}, err
	}
	var result PermissionRole
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := lockAccessMembers(ctx, tx, &m, permission); err != nil {
			return err
		}
		action := "role.created"
		beforeAssignments := map[uuid.UUID]*audit.AccessSnapshot{}
		var before *audit.AccessSnapshot
		if id == uuid.Nil() {
			id = uuid.NewV7()
			if _, err := tx.Exec(ctx, `INSERT INTO roles(id,org_id,name,description,permissions) VALUES($1,$2,$3,$4,$5)`, id, m.OrgID, input.Name, input.Description, input.Permissions); err != nil {
				return accessWriteError(err)
			}
		} else {
			var builtin *string
			if err := tx.QueryRow(ctx, `SELECT builtin_key FROM roles WHERE org_id=$1 AND id=$2 FOR UPDATE`, m.OrgID, id).Scan(&builtin); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrAccessNotFound
				}
				return err
			}
			if builtin != nil {
				return ErrStandardRole
			}
			old, err := readPermissionRole(ctx, tx, m.OrgID, id)
			if err != nil {
				return err
			}
			if input.ExpectedRevision != strconv.FormatInt(old.Revision, 10) {
				return ErrStaleRevision
			}
			before = permissionRoleSnapshot(old)
			action = "role.updated"
			rows, err := tx.Query(ctx, `SELECT id,environment_id IS NULL,all_agents,api_key_id IS NOT NULL FROM role_grants WHERE org_id=$1 AND role_id=$2 ORDER BY id FOR UPDATE`, m.OrgID, id)
			if err != nil {
				return err
			}
			incompatible := []string{}
			for rows.Next() {
				var gid uuid.UUID
				var organization, allAgents, machine bool
				if err = rows.Scan(&gid, &organization, &allAgents, &machine); err != nil {
					rows.Close()
					return err
				}
				if !roleSupportsTargets(input.Permissions, organization, grantAgentsMode(old.Permissions, allAgents)) || machine && !machineAssignable(input.Permissions) {
					incompatible = append(incompatible, gid.String())
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if len(incompatible) > 0 {
				return &RoleGrantConflict{Cause: ErrRoleTarget, GrantIDs: incompatible}
			}
			if hasAgentPermissions(old.Permissions) && !hasAgentPermissions(input.Permissions) {
				oldGrants, err := readRoleGrants(ctx, tx, m.OrgID, GrantFilter{RoleID: id})
				if err != nil {
					return err
				}
				for _, g := range oldGrants {
					beforeAssignments[g.ID] = grantSnapshot(g)
				}
			}

			if _, err = tx.Exec(ctx, `UPDATE roles SET name=$3,description=$4,permissions=$5,revision=revision+1,updated_at=clock_timestamp() WHERE org_id=$1 AND id=$2`, m.OrgID, id, input.Name, input.Description, input.Permissions); err != nil {
				return accessWriteError(err)
			}
			if hasAgentPermissions(old.Permissions) && !hasAgentPermissions(input.Permissions) {
				if _, err = tx.Exec(ctx, `DELETE FROM role_grant_agents WHERE grant_id IN (SELECT id FROM role_grants WHERE org_id=$1 AND role_id=$2)`, m.OrgID, id); err != nil {
					return err
				}
				if _, err = tx.Exec(ctx, `UPDATE role_grants SET all_agents=false,revision=revision+1 WHERE org_id=$1 AND role_id=$2`, m.OrgID, id); err != nil {
					return err
				}
			}
		}
		var err error
		result, err = readPermissionRole(ctx, tx, m.OrgID, id)
		if err != nil {
			return err
		}
		if len(beforeAssignments) > 0 {
			grants, err := readRoleGrants(ctx, tx, m.OrgID, GrantFilter{RoleID: id})
			if err != nil {
				return err
			}
			// Record actual target normalization, not unchanged inherited Role effects.
			for _, g := range grants {
				if err := accessAudit(ctx, tx, m, "role_grant.updated", "role_grant", g.ID, beforeAssignments[g.ID], grantSnapshot(g)); err != nil {
					return err
				}
			}
		}
		return audit.Append(ctx, tx, m.AuditPolicy, audit.Event{Evidence: m.Actor, OrgID: m.OrgID, Action: action, TargetKind: "role", TargetID: id, Details: audit.AccessChangeDetails{Before: before, After: permissionRoleSnapshot(result)}})
	})
	return result, err
}

// RolePatch changes only fields supplied by the caller. Reading the existing
// values is part of this mutation and does not require roles.read.
type RolePatch struct {
	Name             *string
	Description      *string
	Permissions      *[]string
	ExpectedRevision string
}

func PatchPermissionRole(ctx context.Context, database db.TxBeginner, m ManagingMember, id uuid.UUID, patch RolePatch) (PermissionRole, error) {
	var result PermissionRole
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := lockAccessMembers(ctx, tx, &m, auth.PermissionRolesUpdate); err != nil {
			return err
		}
		old, err := readPermissionRole(ctx, tx, m.OrgID, id)
		if err != nil {
			return err
		}
		input := RoleInput{Name: old.Name, Description: old.Description, Permissions: old.Permissions, ExpectedRevision: patch.ExpectedRevision}
		if patch.Name != nil {
			input.Name = *patch.Name
		}
		if patch.Description != nil {
			input.Description = *patch.Description
		}
		if patch.Permissions != nil {
			input.Permissions = *patch.Permissions
		}
		result, err = SavePermissionRole(ctx, tx, m, id, input)
		return err
	})
	return result, err
}

func DeletePermissionRole(ctx context.Context, database db.TxBeginner, m ManagingMember, id uuid.UUID) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := lockAccessMembers(ctx, tx, &m, auth.PermissionRolesDelete); err != nil {
			return err
		}
		var builtin *string
		if err := tx.QueryRow(ctx, `SELECT builtin_key FROM roles WHERE org_id=$1 AND id=$2 FOR UPDATE`, m.OrgID, id).Scan(&builtin); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrAccessNotFound
			}
			return err
		}
		if builtin != nil {
			return ErrStandardRole
		}
		role, err := readPermissionRole(ctx, tx, m.OrgID, id)
		if err != nil {
			return err
		}
		if role.GrantCount != 0 {
			rows, err := tx.Query(ctx, `SELECT id::text FROM role_grants WHERE org_id=$1 AND role_id=$2 ORDER BY id`, m.OrgID, id)
			if err != nil {
				return err
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			return &RoleGrantConflict{Cause: ErrRoleInUse, GrantIDs: ids}
		}
		if _, err = tx.Exec(ctx, `DELETE FROM roles WHERE org_id=$1 AND id=$2`, m.OrgID, id); err != nil {
			return err
		}
		return accessAudit(ctx, tx, m, "role.deleted", "role", id, permissionRoleSnapshot(role), nil)
	})
}

// SeedStandardRoles runs on organization creation, or explicit local fixture
// initialization, with the caller's transaction. Existing definitions are never
// rewritten merely because the initializer runs again.
func SeedStandardRoles(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, orgID uuid.UUID, actor audit.Evidence) error {
	standards := auth.StandardRoles()
	for _, standard := range standards {
		permissions := make([]string, 0, len(standard.Permissions))
		for _, permission := range standard.Permissions {
			permissions = append(permissions, string(permission))
		}
		slices.Sort(permissions)
		id := uuid.NewV7()
		tag, err := tx.Exec(ctx, `INSERT INTO roles(id,org_id,name,description,builtin_key,permissions) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(org_id,builtin_key) DO NOTHING`, id, orgID, standard.Name, standard.Description, standard.Key, permissions)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		if err = accessAudit(ctx, tx, ManagingMember{Actor: actor, OrgID: orgID, AuditPolicy: policy}, "role.created", "role", id, nil, &audit.AccessSnapshot{Name: standard.Name, Description: standard.Description, Permissions: permissions, BuiltinKey: &standard.Key}); err != nil {
			return err
		}
	}
	return nil
}

func accessWriteError(err error) error {
	if db.IsUniqueViolation(err) {
		return ErrAccessNameInUse
	}
	return err
}

func ListPermissionGroups(ctx context.Context, database db.TxBeginner, m ManagingMember) ([]PermissionGroup, error) {
	return ListPermissionGroupsPage(ctx, database, m, uuid.Nil(), 0)
}
func ListPermissionGroupsPage(ctx context.Context, database db.TxBeginner, m ManagingMember, after uuid.UUID, limit int) ([]PermissionGroup, error) {
	groups := []PermissionGroup{}
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := m.Authorize(ctx, tx, auth.PermissionGroupsRead); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT g.id,g.name,g.updated_at,g.revision,ARRAY(SELECT gm.member_id FROM permission_group_members gm JOIN org_members m ON (m.org_id,m.id)=(gm.org_id,gm.member_id) WHERE gm.org_id=g.org_id AND gm.group_id=g.id AND m.removed_at IS NULL ORDER BY gm.member_id) FROM permission_groups g WHERE g.org_id=$1 AND ($2::uuid='00000000-0000-0000-0000-000000000000' OR g.id>$2) ORDER BY g.id LIMIT NULLIF($3,0)`, m.OrgID, after, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g PermissionGroup
			if err = rows.Scan(&g.ID, &g.Name, &g.UpdatedAt, &g.Revision, &g.MemberIDs); err != nil {
				return err
			}
			groups = append(groups, g)
		}
		return rows.Err()
	})
	return groups, err
}
func SavePermissionGroup(ctx context.Context, database db.TxBeginner, m ManagingMember, id uuid.UUID, name string) (PermissionGroup, error) {
	return savePermissionGroup(ctx, database, m, id, name, "")
}

func UpdatePermissionGroup(ctx context.Context, database db.TxBeginner, m ManagingMember, id uuid.UUID, name, revision string) (PermissionGroup, error) {
	return savePermissionGroup(ctx, database, m, id, name, revision)
}
func savePermissionGroup(ctx context.Context, database db.TxBeginner, m ManagingMember, id uuid.UUID, name, revision string) (PermissionGroup, error) {
	permission := auth.PermissionGroupsCreate
	if id != uuid.Nil() {
		permission = auth.PermissionGroupsUpdate
	}
	name, err := normalizeDisplayName(name)
	if err != nil {
		return PermissionGroup{}, err
	}
	result := PermissionGroup{ID: id, Name: name, MemberIDs: []uuid.UUID{}}
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := lockAccessMembers(ctx, tx, &m, permission); err != nil {
			return err
		}
		var before *audit.AccessSnapshot
		action := "permission_group.created"
		if id == uuid.Nil() {
			id = uuid.NewV7()
			if _, err := tx.Exec(ctx, `INSERT INTO permission_groups(id,org_id,name) VALUES($1,$2,$3)`, id, m.OrgID, name); err != nil {
				return accessWriteError(err)
			}
		} else {
			var old string
			var currentRevision int64
			if err := tx.QueryRow(ctx, `SELECT name,revision FROM permission_groups WHERE org_id=$1 AND id=$2 FOR UPDATE`, m.OrgID, id).Scan(&old, &currentRevision); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrAccessNotFound
				}
				return err
			}
			if revision != "" && revision != strconv.FormatInt(currentRevision, 10) {
				return ErrStaleRevision
			}
			before = &audit.AccessSnapshot{Name: old}
			action = "permission_group.updated"
			if _, err := tx.Exec(ctx, `UPDATE permission_groups SET name=$3,revision=revision+1,updated_at=clock_timestamp() WHERE org_id=$1 AND id=$2`, m.OrgID, id, name); err != nil {
				return accessWriteError(err)
			}
		}
		result.ID = id
		if err := tx.QueryRow(ctx, `SELECT updated_at,revision,ARRAY(SELECT member_id FROM permission_group_members WHERE org_id=$1 AND group_id=$2 ORDER BY member_id) FROM permission_groups WHERE org_id=$1 AND id=$2`, m.OrgID, id).Scan(&result.UpdatedAt, &result.Revision, &result.MemberIDs); err != nil {
			return err
		}
		return accessAudit(ctx, tx, m, action, "permission_group", id, before, &audit.AccessSnapshot{Name: name})
	})
	return result, err
}
func DeletePermissionGroup(ctx context.Context, database db.TxBeginner, m ManagingMember, id uuid.UUID) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := lockAccessMembers(ctx, tx, &m, auth.PermissionGroupsDelete); err != nil {
			return err
		}
		var name string
		if err := tx.QueryRow(ctx, `SELECT name FROM permission_groups WHERE org_id=$1 AND id=$2 FOR UPDATE`, m.OrgID, id).Scan(&name); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrAccessNotFound
			}
			return err
		}
		removed, err := readRoleGrants(ctx, tx, m.OrgID, GrantFilter{GroupID: id})
		if err != nil {
			return err
		}
		for _, grant := range removed {
			if err := accessAudit(ctx, tx, m, "role_grant.deleted", "role_grant", grant.ID, grantSnapshot(grant), nil); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM permission_groups WHERE org_id=$1 AND id=$2`, m.OrgID, id); err != nil {
			return err
		}
		return accessAudit(ctx, tx, m, "permission_group.deleted", "permission_group", id, &audit.AccessSnapshot{Name: name}, nil)
	})
}
func SetPermissionGroupMember(ctx context.Context, database db.TxBeginner, m ManagingMember, group, user uuid.UUID, add bool) error {
	permission := auth.PermissionGroupsRemoveMember
	if add {
		permission = auth.PermissionGroupsAddMember
	}
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := lockAccessMembers(ctx, tx, &m, permission, user); err != nil {
			return err
		}
		var existing uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM permission_groups WHERE org_id=$1 AND id=$2 FOR UPDATE`, m.OrgID, group).Scan(&existing); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrAccessNotFound
			}
			return err
		}
		snapshot, err := groupMemberSnapshot(ctx, tx, m.OrgID, group, user)
		if err != nil {
			return err
		}
		action := "permission_group.member_removed"
		before, after := snapshot, (*audit.AccessSnapshot)(nil)
		if add {
			tag, err := tx.Exec(ctx, `INSERT INTO permission_group_members(org_id,group_id,member_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, m.OrgID, group, user)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return nil
			}
			action = "permission_group.member_added"
			before, after = nil, snapshot
		} else {
			tag, err := tx.Exec(ctx, `DELETE FROM permission_group_members WHERE org_id=$1 AND group_id=$2 AND member_id=$3`, m.OrgID, group, user)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return nil
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE permission_groups SET revision=revision+1,updated_at=clock_timestamp() WHERE org_id=$1 AND id=$2`, m.OrgID, group); err != nil {
			return err
		}
		return accessAudit(ctx, tx, m, action, "permission_group", group, before, after)
	})
}

func ListRoleGrants(ctx context.Context, database db.TxBeginner, m ManagingMember) ([]RoleGrant, error) {
	return QueryRoleGrants(ctx, database, m, GrantFilter{})
}

func validateGrantInput(input GrantInput) error {
	switch input.Subject.Type {
	case "member":
		if input.Subject.MemberID == nil || *input.Subject.MemberID == uuid.Nil() || input.Subject.GroupID != nil || input.Subject.APIKeyID != nil {
			return invalidInput("member subject requires only member_id")
		}
	case "group":
		if input.Subject.GroupID == nil || *input.Subject.GroupID == uuid.Nil() || input.Subject.MemberID != nil || input.Subject.APIKeyID != nil {
			return invalidInput("group subject requires only group_id")
		}
	case "api_key":
		if input.Subject.APIKeyID == nil || *input.Subject.APIKeyID == uuid.Nil() || input.Subject.MemberID != nil || input.Subject.GroupID != nil || input.EnvironmentID == nil {
			return invalidInput("API key subject requires only api_key_id and an environment")
		}
	case "all_members":
		if input.Subject.GroupID != nil || input.Subject.MemberID != nil || input.Subject.APIKeyID != nil {
			return invalidInput("All members has no subject id")
		}
	default:
		return invalidInput("select a member, group, API key or All members")
	}
	if input.RoleID == uuid.Nil() || input.ExpectedRoleRevision < 1 {
		return invalidInput("role and expected_role_revision are required")
	}
	if input.EnvironmentID == nil {
		if input.ProjectID != nil || input.AgentsMode != "none" {
			return invalidInput("organization assignment has no workload targets")
		}
	} else if *input.EnvironmentID == uuid.Nil() || input.ProjectID == nil || *input.ProjectID == uuid.Nil() {
		return invalidInput("workload assignment requires project and environment")
	}
	switch input.AgentsMode {
	case "none", "all":
		if len(input.AgentIDs) != 0 {
			return invalidInput("only selected mode accepts Agent IDs")
		}
	case "selected":
		if len(input.AgentIDs) > 100 {
			return invalidInput("select at most 100 Agents")
		}
	default:
		return invalidInput("agents_mode must be none, selected or all")
	}
	for _, id := range input.AgentIDs {
		if id == uuid.Nil() {
			return invalidInput("Agent IDs must not be empty")
		}
	}
	return nil
}

func AssignRole(ctx context.Context, database db.TxBeginner, m ManagingMember, input GrantInput) ([]RoleGrant, error) {
	g, err := writeRoleGrant(ctx, database, m, uuid.Nil(), input)
	if err != nil {
		return nil, err
	}
	return []RoleGrant{g}, nil
}
func UpdateRoleGrant(ctx context.Context, database db.TxBeginner, m ManagingMember, id uuid.UUID, input GrantInput) (RoleGrant, error) {
	if input.ExpectedRevision == "" {
		return RoleGrant{}, ErrStaleRevision
	}
	return writeRoleGrant(ctx, database, m, id, input)
}
func writeRoleGrant(ctx context.Context, database db.TxBeginner, m ManagingMember, id uuid.UUID, input GrantInput) (RoleGrant, error) {
	permission := auth.PermissionAssignmentsCreate
	if id != uuid.Nil() {
		permission = auth.PermissionAssignmentsUpdate
	}
	var result RoleGrant
	if err := validateGrantInput(input); err != nil {
		return result, err
	}
	input.AgentIDs = slices.Clone(input.AgentIDs)
	slices.SortFunc(input.AgentIDs, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	input.AgentIDs = slices.Compact(input.AgentIDs)
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		targets := []uuid.UUID{}
		if input.Subject.MemberID != nil {
			targets = append(targets, *input.Subject.MemberID)
		}
		if err := lockAccessMembers(ctx, tx, &m, permission, targets...); err != nil {
			return err
		}
		var roleID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM roles WHERE org_id=$1 AND id=$2 FOR UPDATE`, m.OrgID, input.RoleID).Scan(&roleID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrAccessNotFound
			}
			return err
		}
		role, err := readPermissionRole(ctx, tx, m.OrgID, roleID)
		if err != nil {
			return err
		}
		if role.Revision != input.ExpectedRoleRevision {
			return ErrStaleRevision
		}
		if !roleSupportsTargets(role.Permissions, input.EnvironmentID == nil, input.AgentsMode) || (!hasAgentPermissions(role.Permissions) && input.AgentsMode != "none") {
			return ErrRoleTarget
		}
		if input.AgentsMode == "selected" && len(input.AgentIDs) == 0 {
			mixed := false
			for _, permission := range role.Permissions {
				if !auth.IsAgentPermission(auth.Permission(permission)) {
					mixed = true
				}
			}
			if !mixed {
				return invalidInput("select at least one Agent for an Agent-only assignment")
			}
		}
		if input.Subject.APIKeyID != nil {
			if !machineAssignable(role.Permissions) {
				return ErrRoleTarget
			}
			var key uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM api_keys WHERE org_id=$1 AND environment_id=$2 AND id=$3 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>statement_timestamp()) FOR SHARE`, m.OrgID, input.EnvironmentID, input.Subject.APIKeyID).Scan(&key); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrAccessNotFound
				}
				return err
			}
		}
		if input.Subject.GroupID != nil {
			var group uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM permission_groups WHERE org_id=$1 AND id=$2 FOR KEY SHARE`, m.OrgID, input.Subject.GroupID).Scan(&group); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrAccessNotFound
				}
				return err
			}
		}
		if input.EnvironmentID != nil {
			var env uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM environments WHERE org_id=$1 AND project_id=$2 AND id=$3 FOR KEY SHARE`, m.OrgID, input.ProjectID, input.EnvironmentID).Scan(&env); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrEnvironmentNotFound
				}
				return err
			}
		}
		for _, agent := range input.AgentIDs {
			var found uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM agents WHERE environment_id=$1 AND id=$2 AND deleted_at IS NULL FOR KEY SHARE`, input.EnvironmentID, agent).Scan(&found); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrAccessNotFound
				}
				return err
			}
		}
		action := "role_grant.created"
		var before *audit.AccessSnapshot
		created := false
		if id == uuid.Nil() {
			id = uuid.NewV7()
			tag, err := tx.Exec(ctx, `INSERT INTO role_grants(id,org_id,role_id,member_id,group_id,all_members,environment_id,all_agents,created_by_user_id,api_key_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING`, id, m.OrgID, roleID, input.Subject.MemberID, input.Subject.GroupID, input.Subject.Type == "all_members", input.EnvironmentID, input.AgentsMode == "all", m.UserID, input.Subject.APIKeyID)
			if err != nil {
				return err
			}
			created = tag.RowsAffected() != 0
			if !created {
				if err := tx.QueryRow(ctx, `SELECT id FROM role_grants WHERE org_id=$1 AND role_id=$2 AND member_id IS NOT DISTINCT FROM $3::uuid AND group_id IS NOT DISTINCT FROM $4::uuid AND all_members=$5 AND environment_id IS NOT DISTINCT FROM $6::uuid AND api_key_id IS NOT DISTINCT FROM $7::uuid FOR UPDATE`, m.OrgID, roleID, input.Subject.MemberID, input.Subject.GroupID, input.Subject.Type == "all_members", input.EnvironmentID, input.Subject.APIKeyID).Scan(&id); err != nil {
					return err
				}
				grants, err := readRoleGrants(ctx, tx, m.OrgID, GrantFilter{ID: id})
				if err != nil {
					return err
				}
				if len(grants) != 1 {
					return ErrAccessNotFound
				}
				result = grants[0]
				if result.AgentsMode != input.AgentsMode || !slices.Equal(result.AgentIDs, input.AgentIDs) {
					return &RoleGrantConflict{Cause: ErrGrantExists, GrantIDs: []string{id.String()}, Revision: result.Revision}
				}
				return nil
			}
		} else {
			var locked uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM role_grants WHERE org_id=$1 AND id=$2 AND role_id=$3 FOR UPDATE`, m.OrgID, id, roleID).Scan(&locked); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrAccessNotFound
				}
				return err
			}
			grants, err := readRoleGrants(ctx, tx, m.OrgID, GrantFilter{ID: id})
			if err != nil {
				return err
			}
			if len(grants) != 1 {
				return ErrAccessNotFound
			}
			old := grants[0]
			if strconv.FormatInt(old.Revision, 10) != input.ExpectedRevision {
				return ErrStaleRevision
			}
			if old.SubjectKind != input.Subject.Type || !equalUUID(old.MemberID, input.Subject.MemberID) || !equalUUID(old.GroupID, input.Subject.GroupID) || !equalUUID(old.APIKeyID, input.Subject.APIKeyID) || (input.EnvironmentID == nil && old.EnvironmentID != uuid.Nil()) || (input.EnvironmentID != nil && old.EnvironmentID != *input.EnvironmentID) {
				return invalidInput("assignment identity cannot change; create a new assignment")
			}
			before = grantSnapshot(old)
			action = "role_grant.updated"
			if _, err := tx.Exec(ctx, `DELETE FROM role_grant_agents WHERE grant_id=$1`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE role_grants SET all_agents=$3,revision=revision+1 WHERE org_id=$1 AND id=$2`, m.OrgID, id, input.AgentsMode == "all"); err != nil {
				return err
			}
		}
		for _, agent := range input.AgentIDs {
			if _, err := tx.Exec(ctx, `INSERT INTO role_grant_agents(grant_id,environment_id,agent_id) VALUES($1,$2,$3)`, id, input.EnvironmentID, agent); err != nil {
				return err
			}
		}
		grants, err := readRoleGrants(ctx, tx, m.OrgID, GrantFilter{ID: id})
		if err != nil {
			return err
		}
		if len(grants) != 1 {
			return ErrAccessNotFound
		}
		result = grants[0]
		result.Created = created
		return accessAudit(ctx, tx, m, action, "role_grant", id, before, grantSnapshot(result))
	})
	return result, err
}
func equalUUID(a, b *uuid.UUID) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

func RemoveRoleGrant(ctx context.Context, database db.TxBeginner, m ManagingMember, id uuid.UUID, revision string) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := lockAccessMembers(ctx, tx, &m, auth.PermissionAssignmentsDelete); err != nil {
			return err
		}
		var role uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT role_id FROM role_grants WHERE org_id=$1 AND id=$2`, m.OrgID, id).Scan(&role); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT id FROM roles WHERE org_id=$1 AND id=$2 FOR UPDATE`, m.OrgID, role).Scan(&role); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		var locked uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM role_grants WHERE org_id=$1 AND id=$2 FOR UPDATE`, m.OrgID, id).Scan(&locked); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		grants, err := readRoleGrants(ctx, tx, m.OrgID, GrantFilter{ID: id})
		if err != nil {
			return err
		}
		if len(grants) == 0 {
			return nil
		}
		g := grants[0]
		if revision != strconv.FormatInt(g.Revision, 10) {
			return ErrStaleRevision
		}
		if _, err := tx.Exec(ctx, `DELETE FROM role_grants WHERE org_id=$1 AND id=$2`, m.OrgID, id); err != nil {
			return err
		}
		return accessAudit(ctx, tx, m, "role_grant.deleted", "role_grant", id, grantSnapshot(g), nil)
	})
}

// RemovePermissionGroupMemberships is part of member removal, under the member
// locks acquired by the caller. Rejoining never restores these custom edges.
func RemovePermissionGroupMemberships(ctx context.Context, tx pgx.Tx, m ManagingMember, user uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT g.id FROM permission_groups g JOIN permission_group_members gm ON (gm.org_id,gm.group_id)=(g.org_id,g.id) WHERE gm.org_id=$1 AND gm.member_id=$2 ORDER BY g.id FOR UPDATE OF g`, m.OrgID, user)
	if err != nil {
		return err
	}
	groups := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		groups = append(groups, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM permission_group_members WHERE org_id=$1 AND member_id=$2`, m.OrgID, user); err != nil {
		return err
	}
	for _, group := range groups {
		if _, err = tx.Exec(ctx, `UPDATE permission_groups SET revision=revision+1,updated_at=now() WHERE org_id=$1 AND id=$2`, m.OrgID, group); err != nil {
			return err
		}
		snapshot, err := groupMemberSnapshot(ctx, tx, m.OrgID, group, user)
		if err != nil {
			return err
		}
		if err = accessAudit(ctx, tx, m, "permission_group.member_removed", "permission_group", group, snapshot, nil); err != nil {
			return err
		}
	}
	return nil
}

type AccessTarget struct {
	EnvironmentID uuid.UUID     `json:"environment_id"`
	Environment   string        `json:"environment"`
	Name          string        `json:"name"`
	Agents        []AccessAgent `json:"agents"`
}
type AccessAgent struct {
	ID  uuid.UUID `json:"id"`
	Key string    `json:"key"`
}

func ListAccessTargets(ctx context.Context, database db.TxBeginner, m ManagingMember, project uuid.UUID) ([]AccessTarget, error) {
	result := []AccessTarget{}
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := m.Authorize(ctx, tx, auth.PermissionAssignmentsRead); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id,slug,name FROM environments WHERE org_id=$1 AND project_id=$2 ORDER BY is_default DESC,slug`, m.OrgID, project)
		if err != nil {
			return err
		}
		for rows.Next() {
			var target AccessTarget
			if err = rows.Scan(&target.EnvironmentID, &target.Environment, &target.Name); err != nil {
				rows.Close()
				return err
			}
			target.Agents = []AccessAgent{}
			result = append(result, target)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for i := range result {
			rows, err = tx.Query(ctx, `SELECT id,key FROM agents WHERE environment_id=$1 AND deleted_at IS NULL ORDER BY key`, result[i].EnvironmentID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var a AccessAgent
				if err = rows.Scan(&a.ID, &a.Key); err != nil {
					rows.Close()
					return err
				}
				result[i].Agents = append(result[i].Agents, a)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

func GetPermissionRole(ctx context.Context, database db.TxBeginner, m ManagingMember, id uuid.UUID) (PermissionRole, error) {
	var result PermissionRole
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := m.Authorize(ctx, tx, auth.PermissionRolesRead); err != nil {
			return err
		}
		var err error
		result, err = readPermissionRole(ctx, tx, m.OrgID, id)
		return err
	})
	return result, err
}

func GetPermissionGroup(ctx context.Context, database db.TxBeginner, m ManagingMember, id uuid.UUID) (PermissionGroup, error) {
	var result PermissionGroup
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := m.Authorize(ctx, tx, auth.PermissionGroupsRead); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT id,name,updated_at,revision,ARRAY(SELECT gm.member_id FROM permission_group_members gm JOIN org_members m ON (m.org_id,m.id)=(gm.org_id,gm.member_id) WHERE gm.org_id=$1 AND gm.group_id=$2 AND m.removed_at IS NULL ORDER BY gm.member_id) FROM permission_groups WHERE org_id=$1 AND id=$2`, m.OrgID, id).Scan(&result.ID, &result.Name, &result.UpdatedAt, &result.Revision, &result.MemberIDs)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAccessNotFound
		}
		return err
	})
	return result, err
}

type GrantFilter struct {
	ID, MemberID, GroupID, APIKeyID, RoleID, After      uuid.UUID
	SubjectKind, ScopeKind, Project, Environment, Agent string
	Limit                                               int
}

func QueryRoleGrants(ctx context.Context, database db.TxBeginner, m ManagingMember, filter GrantFilter) ([]RoleGrant, error) {
	result := []RoleGrant{}
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := m.Authorize(ctx, tx, auth.PermissionAssignmentsRead); err != nil {
			return err
		}
		var err error
		result, err = readRoleGrants(ctx, tx, m.OrgID, filter)
		return err
	})
	return result, err
}

func readRoleGrants(ctx context.Context, q db.DBTX, orgID uuid.UUID, filter GrantFilter) ([]RoleGrant, error) {
	rows, err := q.Query(ctx, `SELECT g.id,
 CASE WHEN g.all_members THEN 'all_members' WHEN g.member_id IS NOT NULL THEN 'member' WHEN g.api_key_id IS NOT NULL THEN 'api_key' ELSE 'group' END,
 g.member_id,g.group_id,g.api_key_id,COALESCE(k.name,''),g.role_id,e.project_id,g.environment_id,
 CASE WHEN g.environment_id IS NULL THEN 'organization' ELSE 'environment' END,
 g.revision,r.revision,g.all_agents,ARRAY(SELECT agent_id FROM role_grant_agents WHERE grant_id=g.id ORDER BY agent_id),ARRAY(SELECT a.key FROM role_grant_agents ga JOIN agents a ON (a.environment_id,a.id)=(ga.environment_id,ga.agent_id) WHERE ga.grant_id=g.id ORDER BY ga.agent_id),r.name,COALESCE(p.name,''),COALESCE(e.name,''),COALESCE(p.slug,''),COALESCE(e.slug,''),COALESCE(pg.name,''),COALESCE(m.display_name,u.display_name,''),g.created_at,g.created_by_user_id,
 r.permissions
 FROM role_grants g JOIN roles r ON (r.org_id,r.id)=(g.org_id,g.role_id)
 LEFT JOIN environments e ON e.id=g.environment_id LEFT JOIN projects p ON p.id=e.project_id
 LEFT JOIN api_keys k ON (k.org_id,k.id)=(g.org_id,g.api_key_id)
 LEFT JOIN permission_groups pg ON (pg.org_id,pg.id)=(g.org_id,g.group_id)
 LEFT JOIN org_members m ON (m.org_id,m.id)=(g.org_id,g.member_id) LEFT JOIN users u ON u.id=m.user_id
 WHERE g.org_id=$1 AND ($2::uuid='00000000-0000-0000-0000-000000000000' OR g.id=$2)
 AND ($3::uuid='00000000-0000-0000-0000-000000000000' OR g.group_id=$3)
 AND ($4::uuid='00000000-0000-0000-0000-000000000000' OR g.role_id=$4)
 AND ($5='' OR CASE WHEN g.all_members THEN 'all_members' WHEN g.member_id IS NOT NULL THEN 'member' WHEN g.api_key_id IS NOT NULL THEN 'api_key' ELSE 'group' END=$5)
 AND ($6='' OR p.slug=$6) AND ($7='' OR e.slug=$7)
 AND ($8='' OR g.all_agents OR EXISTS(SELECT 1 FROM role_grant_agents ga JOIN agents a ON (a.environment_id,a.id)=(ga.environment_id,ga.agent_id) WHERE ga.grant_id=g.id AND a.key=$8))
 AND g.id>$9
 AND ($11::uuid='00000000-0000-0000-0000-000000000000' OR g.member_id=$11)
 AND ($12='' OR CASE WHEN g.environment_id IS NULL THEN 'organization' ELSE 'environment' END=$12)
 AND ($13::uuid='00000000-0000-0000-0000-000000000000' OR g.api_key_id=$13)
 ORDER BY g.id LIMIT NULLIF($10::integer,0)`, orgID, filter.ID, filter.GroupID, filter.RoleID, filter.SubjectKind, filter.Project, filter.Environment, filter.Agent, filter.After, filter.Limit, filter.MemberID, filter.ScopeKind, filter.APIKeyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []RoleGrant{}
	for rows.Next() {
		var g RoleGrant
		var allAgents bool
		var project, env *uuid.UUID
		if err := rows.Scan(&g.ID, &g.SubjectKind, &g.MemberID, &g.GroupID, &g.APIKeyID, &g.APIKeyName, &g.RoleID, &project, &env, &g.TargetKind, &g.Revision, &g.RoleRevision, &allAgents, &g.AgentIDs, &g.AgentKeys, &g.RoleName, &g.ProjectName, &g.EnvironmentName, &g.Project, &g.Environment, &g.GroupName, &g.MemberName, &g.CreatedAt, &g.CreatedBy, &g.Permissions); err != nil {
			return nil, err
		}
		if project != nil {
			g.ProjectID = *project
		}
		if env != nil {
			g.EnvironmentID = *env
		}
		g.AgentsMode = grantAgentsMode(g.Permissions, allAgents)
		g.Targets = grantTargets(g)
		result = append(result, g)
	}
	return result, rows.Err()
}

func groupMemberSnapshot(ctx context.Context, tx pgx.Tx, orgID, groupID, memberID uuid.UUID) (*audit.AccessSnapshot, error) {
	s := &audit.AccessSnapshot{GroupID: groupID.String(), MemberID: memberID.String()}
	err := tx.QueryRow(ctx, `SELECT g.name,COALESCE(u.display_name,''),m.user_id::text FROM permission_groups g JOIN org_members m ON m.org_id=g.org_id JOIN users u ON u.id=m.user_id WHERE g.org_id=$1 AND g.id=$2 AND m.id=$3`, orgID, groupID, memberID).Scan(&s.GroupName, &s.MemberName, &s.UserID)
	return s, err
}
