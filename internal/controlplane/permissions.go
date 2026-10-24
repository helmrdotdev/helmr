package controlplane

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func (s *Server) listPermissionRoles(w http.ResponseWriter, r *http.Request) {
	limit, after, err := permissionListPage(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	roles, err := org.ListPermissionRolesPage(r.Context(), s.tx, managingMember(principalFromContext(r.Context())), after, limit+1)
	if err != nil {
		writeError(w, permissionManagementError(err))
		return
	}
	next := ""
	if len(roles) > limit {
		roles = roles[:limit]
		next = roles[limit-1].ID.String()
	}
	writeJSON(w, http.StatusOK, struct {
		NextCursor string                      `json:"next_cursor,omitempty"`
		Roles      []org.PermissionRole        `json:"roles"`
		Catalog    []auth.CapabilityDefinition `json:"catalog"`
	}{next, roles, auth.CapabilityCatalog()})
}
func permissionPathID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := ids.Parse(chi.URLParam(r, name))
	if err != nil || id == uuid.Nil() {
		return uuid.Nil(), badRequest(errors.New("invalid resource id"))
	}
	return id, nil
}
func (s *Server) savePermissionRole(w http.ResponseWriter, r *http.Request) {
	var id uuid.UUID
	var err error
	if chi.URLParam(r, "id") != "" {
		id, err = permissionPathID(r, "id")
		if err != nil {
			writeError(w, err)
			return
		}
	}
	var patch struct {
		Name        *string   `json:"name"`
		Description *string   `json:"description"`
		Permissions *[]string `json:"permissions"`
	}
	if err = decodeRequestJSON(r, &patch); err != nil {
		writeError(w, err)
		return
	}
	var input org.RoleInput
	if id != uuid.Nil() {
		old, e := org.GetPermissionRole(r.Context(), s.tx, managingMember(principalFromContext(r.Context())), id)
		if e != nil {
			writeError(w, permissionManagementError(e))
			return
		}
		revision, e := permissionEditRevision(r, old.Revision)
		if e != nil {
			writeError(w, e)
			return
		}
		input = org.RoleInput{Name: old.Name, Description: old.Description, Permissions: old.Permissions, ExpectedRevision: revision}
	}
	if patch.Name != nil {
		input.Name = *patch.Name
	}
	if patch.Description != nil {
		input.Description = *patch.Description
	}
	if patch.Permissions != nil {
		input.Permissions = *patch.Permissions
	}
	role, err := org.SavePermissionRole(r.Context(), s.tx, managingMember(principalFromContext(r.Context())), id, input)
	if err != nil {
		writeError(w, permissionManagementError(err))
		return
	}
	w.Header().Set("ETag", `"`+strconv.FormatInt(role.Revision, 10)+`"`)
	status := http.StatusOK
	if id == uuid.Nil() {
		status = http.StatusCreated
	}
	writeJSON(w, status, role)
}
func (s *Server) deletePermissionRole(w http.ResponseWriter, r *http.Request) {
	id, err := permissionPathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	if err = org.DeletePermissionRole(r.Context(), s.tx, managingMember(principalFromContext(r.Context())), id); err != nil {
		writeError(w, permissionManagementError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) listPermissionGroups(w http.ResponseWriter, r *http.Request) {
	limit, after, err := permissionListPage(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	groups, err := org.ListPermissionGroupsPage(r.Context(), s.tx, managingMember(principalFromContext(r.Context())), after, limit+1)
	if err != nil {
		writeError(w, permissionManagementError(err))
		return
	}
	next := ""
	if len(groups) > limit {
		groups = groups[:limit]
		next = groups[limit-1].ID.String()
	}
	writeJSON(w, http.StatusOK, struct {
		NextCursor string                `json:"next_cursor,omitempty"`
		Groups     []org.PermissionGroup `json:"groups"`
	}{next, groups})
}
func (s *Server) savePermissionGroup(w http.ResponseWriter, r *http.Request) {
	var id uuid.UUID
	var err error
	if chi.URLParam(r, "id") != "" {
		id, err = permissionPathID(r, "id")
		if err != nil {
			writeError(w, err)
			return
		}
	}
	var input struct {
		Name *string `json:"name"`
	}
	if err = decodeRequestJSON(r, &input); err != nil {
		writeError(w, err)
		return
	}
	var group org.PermissionGroup
	name, revision := "", ""
	if id != uuid.Nil() {
		old, e := org.GetPermissionGroup(r.Context(), s.tx, managingMember(principalFromContext(r.Context())), id)
		if e != nil {
			writeError(w, permissionManagementError(e))
			return
		}
		name = old.Name
		revision, e = permissionEditRevision(r, old.Revision)
		if e != nil {
			writeError(w, e)
			return
		}
	}
	if input.Name != nil {
		name = *input.Name
	}
	if id == uuid.Nil() {
		group, err = org.SavePermissionGroup(r.Context(), s.tx, managingMember(principalFromContext(r.Context())), id, name)
	} else {
		group, err = org.UpdatePermissionGroup(r.Context(), s.tx, managingMember(principalFromContext(r.Context())), id, name, revision)
	}
	if err != nil {
		writeError(w, permissionManagementError(err))
		return
	}
	w.Header().Set("ETag", `"`+strconv.FormatInt(group.Revision, 10)+`"`)
	status := http.StatusOK
	if id == uuid.Nil() {
		status = http.StatusCreated
	}
	writeJSON(w, status, group)
}
func (s *Server) deletePermissionGroup(w http.ResponseWriter, r *http.Request) {
	id, err := permissionPathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	if err = org.DeletePermissionGroup(r.Context(), s.tx, managingMember(principalFromContext(r.Context())), id); err != nil {
		writeError(w, permissionManagementError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) setPermissionGroupMember(w http.ResponseWriter, r *http.Request) {
	group, err := permissionPathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	user, err := permissionPathID(r, "memberID")
	if err != nil {
		writeError(w, err)
		return
	}
	if err = org.SetPermissionGroupMember(r.Context(), s.tx, managingMember(principalFromContext(r.Context())), group, user, r.Method == http.MethodPut); err != nil {
		writeError(w, permissionManagementError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) listRoleGrants(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := org.GrantFilter{Project: q.Get("project"), Environment: q.Get("environment"), Agent: q.Get("agent"), SubjectKind: q.Get("subject_kind"), ScopeKind: q.Get("scope_kind"), Limit: 101}
	if filter.SubjectKind != "" && filter.SubjectKind != "member" && filter.SubjectKind != "group" && filter.SubjectKind != "all_members" {
		writeError(w, badRequest(errors.New("invalid subject_kind")))
		return
	}
	for key, values := range q {
		switch key {
		case "project", "environment", "agent", "subject_kind", "scope_kind", "member_id", "group_id", "role_id", "cursor", "limit":
		default:
			writeError(w, badRequest(errors.New("unknown query parameter "+key)))
			return
		}
		if len(values) != 1 {
			writeError(w, badRequest(errors.New("query parameters cannot be repeated")))
			return
		}
	}
	if filter.ScopeKind != "" && filter.ScopeKind != "organization" && filter.ScopeKind != "environment" {
		writeError(w, badRequest(errors.New("invalid scope_kind")))
		return
	}
	if (q.Get("member_id") != "" && q.Get("group_id") != "") || (filter.SubjectKind == "all_members" && (q.Get("member_id") != "" || q.Get("group_id") != "")) || (filter.SubjectKind == "member" && q.Get("group_id") != "") || (filter.SubjectKind == "group" && q.Get("member_id") != "") {
		writeError(w, badRequest(errors.New("incompatible subject filters")))
		return
	}
	if filter.ScopeKind == "organization" && (filter.Project != "" || filter.Environment != "" || filter.Agent != "") {
		writeError(w, badRequest(errors.New("organization scope cannot include workload filters")))
		return
	}
	for key, target := range map[string]*uuid.UUID{"member_id": &filter.MemberID, "group_id": &filter.GroupID, "role_id": &filter.RoleID, "cursor": &filter.After} {
		if raw := q.Get(key); raw != "" {
			id, e := ids.Parse(raw)
			if e != nil || id == uuid.Nil() {
				writeError(w, badRequest(errors.New("invalid "+key)))
				return
			}
			*target = id
		}
	}
	limit := 100
	if raw := q.Get("limit"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 || n > 100 {
			writeError(w, badRequest(errors.New("limit must be between 1 and 100")))
			return
		}
		limit = n
	}
	filter.Limit = limit + 1
	if filter.Environment != "" && filter.Project == "" || filter.Agent != "" && (filter.Environment == "" || filter.Project == "") {
		writeError(w, badRequest(errors.New("environment requires project; agent requires project and environment")))
		return
	}
	grants, err := org.QueryRoleGrants(r.Context(), s.tx, managingMember(principalFromContext(r.Context())), filter)
	if err != nil {
		writeError(w, permissionManagementError(err))
		return
	}
	next := ""
	if len(grants) > limit {
		grants = grants[:limit]
		next = grants[limit-1].ID.String()
	}
	writeJSON(w, http.StatusOK, struct {
		Grants     []org.RoleGrant `json:"grants"`
		NextCursor string          `json:"next_cursor,omitempty"`
	}{grants, next})
}
func (s *Server) assignRole(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	var input org.GrantInput
	if err := decodeRequestJSON(r, &input); err != nil {
		writeError(w, err)
		return
	}
	grants, err := org.AssignRole(r.Context(), s.tx, managingMember(p), input)
	if err != nil {
		writeError(w, permissionManagementError(err))
		return
	}
	status := http.StatusOK
	for _, g := range grants {
		if g.Created {
			status = http.StatusCreated
			break
		}
	}
	writeJSON(w, status, struct {
		Grants []org.RoleGrant `json:"grants"`
	}{grants})
}
func (s *Server) removeRoleGrant(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	id, err := permissionPathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	grants, err := org.QueryRoleGrants(r.Context(), s.tx, managingMember(p), org.GrantFilter{ID: id})
	if err != nil {
		writeError(w, permissionManagementError(err))
		return
	}
	if len(grants) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	revision, err := permissionEditRevision(r, grants[0].Revision)
	if err != nil {
		writeError(w, err)
		return
	}
	if err = org.RemoveRoleGrant(r.Context(), s.tx, managingMember(p), id, revision); err != nil {
		writeError(w, permissionManagementError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func permissionManagementError(err error) error {
	var inputError org.InputError
	var grantConflict *org.RoleGrantConflict
	var details map[string]json.RawMessage
	if errors.As(err, &grantConflict) {
		ids, _ := json.Marshal(grantConflict.GrantIDs)
		revision, _ := json.Marshal(grantConflict.Revision)
		details = map[string]json.RawMessage{"grant_ids": ids, "revision": revision}
	}
	switch {
	case errors.Is(err, org.ErrAccessNotFound):
		return notFound(err)
	case errors.Is(err, org.ErrStaleRevision):
		return apiError{kind: errPreconditionFailed, err: codedError{code: "stale_revision", message: err.Error()}}
	case errors.Is(err, org.ErrGrantExists):
		return conflict(codedError{code: "assignment_exists", message: err.Error(), details: details})
	case errors.Is(err, org.ErrStandardRole):
		return forbidden(codedError{code: "standard_role_immutable", message: err.Error()})
	case errors.Is(err, org.ErrRoleTarget):
		coded := codedError{code: "role_target_incompatible", message: err.Error(), details: details}
		if grantConflict != nil {
			return conflict(coded)
		}
		return apiError{kind: errUnprocessable, err: coded}
	case errors.As(err, &inputError):
		return apiError{kind: errUnprocessable, err: err}
	case errors.Is(err, org.ErrAccessNameInUse):
		return conflict(codedError{code: "name_in_use", message: err.Error()})
	case errors.Is(err, org.ErrRoleInUse):
		return conflict(codedError{code: "role_in_use", message: err.Error(), details: details})
	default:
		return orgError(err)
	}
}

func (s *Server) listAccessTargets(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if !p.HasPermission(auth.CapabilityAccessManage, auth.Scope{OrgID: p.OrgID}) {
		writeError(w, forbidden(org.ErrMemberManagementRequired))
		return
	}
	project, err := org.GetProject(r.Context(), s.db, p.OrgID, chi.URLParam(r, "project"))
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	targets, err := org.ListAccessTargets(r.Context(), s.tx, managingMember(p), pgvalue.MustUUIDValue(project.ID))
	if err != nil {
		writeError(w, permissionManagementError(err))
		return
	}
	writeJSON(w, http.StatusOK, struct {
		ProjectID string             `json:"project_id"`
		Targets   []org.AccessTarget `json:"targets"`
	}{pgvalue.UUIDString(project.ID), targets})
}

func permissionEditRevision(r *http.Request, current int64) (string, error) {
	raw := r.Header.Get("If-Match")
	if raw == "" {
		return "", apiError{kind: errPreconditionRequired, err: codedError{code: "revision_required", message: "If-Match from the displayed revision is required"}}
	}
	if raw != `"`+strconv.FormatInt(current, 10)+`"` {
		return "", permissionManagementError(org.ErrStaleRevision)
	}
	return strings.Trim(raw, `"`), nil
}
func (s *Server) getPermissionRole(w http.ResponseWriter, r *http.Request) {
	id, err := permissionPathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	role, err := org.GetPermissionRole(r.Context(), s.tx, managingMember(principalFromContext(r.Context())), id)
	if err != nil {
		writeError(w, permissionManagementError(err))
		return
	}
	w.Header().Set("ETag", `"`+strconv.FormatInt(role.Revision, 10)+`"`)
	writeJSON(w, http.StatusOK, role)
}
func (s *Server) getPermissionGroup(w http.ResponseWriter, r *http.Request) {
	id, err := permissionPathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	group, err := org.GetPermissionGroup(r.Context(), s.tx, managingMember(principalFromContext(r.Context())), id)
	if err != nil {
		writeError(w, permissionManagementError(err))
		return
	}
	w.Header().Set("ETag", `"`+strconv.FormatInt(group.Revision, 10)+`"`)
	writeJSON(w, http.StatusOK, group)
}
func (s *Server) getRoleGrant(w http.ResponseWriter, r *http.Request) {
	id, err := permissionPathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	grants, err := org.QueryRoleGrants(r.Context(), s.tx, managingMember(principalFromContext(r.Context())), org.GrantFilter{ID: id})
	if err != nil {
		writeError(w, permissionManagementError(err))
		return
	}
	if len(grants) == 0 {
		writeError(w, notFound(org.ErrAccessNotFound))
		return
	}
	w.Header().Set("ETag", `"`+strconv.FormatInt(grants[0].Revision, 10)+`"`)
	writeJSON(w, http.StatusOK, grants[0])
}
func (s *Server) listPermissionGroupMembers(w http.ResponseWriter, r *http.Request) {
	id, err := permissionPathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	group, err := org.GetPermissionGroup(r.Context(), s.tx, managingMember(principalFromContext(r.Context())), id)
	if err != nil {
		writeError(w, permissionManagementError(err))
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			writeError(w, badRequest(errors.New("limit must be between 1 and 100")))
			return
		}
	}
	cursor := r.URL.Query().Get("cursor")
	if cursor != "" {
		parsed, err := ids.Parse(cursor)
		if err != nil || parsed == uuid.Nil() {
			writeError(w, badRequest(errors.New("invalid cursor")))
			return
		}
		cursor = parsed.String()
	}
	members := []uuid.UUID{}
	for _, member := range group.MemberIDs {
		if member.String() > cursor {
			members = append(members, member)
		}
	}
	next := ""
	if len(members) > limit {
		members = members[:limit]
		next = members[limit-1].String()
	}
	writeJSON(w, http.StatusOK, struct {
		MemberIDs  []uuid.UUID `json:"member_ids"`
		NextCursor string      `json:"next_cursor,omitempty"`
	}{members, next})
}

func permissionListPage(r *http.Request) (int, uuid.UUID, error) {
	limit := 100
	after := uuid.Nil()
	for key, values := range r.URL.Query() {
		if (key != "cursor" && key != "limit") || len(values) != 1 {
			return 0, after, errors.New("unsupported or repeated query parameter")
		}
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			return 0, after, errors.New("limit must be between 1 and 100")
		}
		limit = value
	}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		var err error
		after, err = ids.Parse(raw)
		if err != nil || after == uuid.Nil() {
			return 0, after, errors.New("invalid cursor")
		}
	}
	return limit, after, nil
}

func (s *Server) updateRoleGrant(w http.ResponseWriter, r *http.Request) {
	id, err := permissionPathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	var input org.GrantInput
	if err := decodeRequestJSON(r, &input); err != nil {
		writeError(w, err)
		return
	}
	raw := r.Header.Get("If-Match")
	revision, e := strconv.ParseInt(strings.Trim(raw, `"`), 10, 64)
	if e != nil || raw != `"`+strconv.FormatInt(revision, 10)+`"` || revision < 1 {
		writeError(w, apiError{kind: errPreconditionRequired, err: codedError{code: "revision_required", message: "If-Match from the displayed assignment revision is required"}})
		return
	}
	input.ExpectedRevision = strconv.FormatInt(revision, 10)
	result, err := org.UpdateRoleGrant(r.Context(), s.tx, managingMember(principalFromContext(r.Context())), id, input)
	if err != nil {
		writeError(w, permissionManagementError(err))
		return
	}
	w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
	writeJSON(w, http.StatusOK, result)
}
