package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/helmrdotdev/helmr/internal/api"
)

func (c *Client) permissionRequest(ctx context.Context, method, path string, input, output any, revision string) error {
	var body bytes.Buffer
	if input != nil {
		if err := json.NewEncoder(&body).Encode(input); err != nil {
			return err
		}
	}
	req, err := c.newRequest(ctx, method, path, &body)
	if err != nil {
		return err
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if revision != "" {
		req.Header.Set("If-Match", `"`+revision+`"`)
	}
	return c.doJSON(req, output)
}
func (c *Client) ListPermissionRoles(ctx context.Context) (api.PermissionRolesResponse, error) {
	out := api.PermissionRolesResponse{Roles: []api.PermissionRole{}}
	cursor := ""
	for {
		var page api.PermissionRolesResponse
		if err := c.permissionRequest(ctx, http.MethodGet, "/api/roles?limit=100&cursor="+url.QueryEscape(cursor), nil, &page, ""); err != nil {
			return out, err
		}
		out.Roles = append(out.Roles, page.Roles...)
		out.Catalog = page.Catalog
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}
func (c *Client) GetPermissionRole(ctx context.Context, id string) (api.PermissionRole, error) {
	var out api.PermissionRole
	err := c.permissionRequest(ctx, http.MethodGet, "/api/roles/"+url.PathEscape(id), nil, &out, "")
	return out, err
}
func (c *Client) CreatePermissionRole(ctx context.Context, input api.PermissionRoleInput) (api.PermissionRole, error) {
	var out api.PermissionRole
	err := c.permissionRequest(ctx, http.MethodPost, "/api/roles", input, &out, "")
	return out, err
}
func (c *Client) UpdatePermissionRole(ctx context.Context, id string, input api.PermissionRolePatch, revision string) (api.PermissionRole, error) {
	var out api.PermissionRole
	err := c.permissionRequest(ctx, http.MethodPatch, "/api/roles/"+url.PathEscape(id), input, &out, revision)
	return out, err
}
func (c *Client) DeletePermissionRole(ctx context.Context, id string) error {
	return c.permissionRequest(ctx, http.MethodDelete, "/api/roles/"+url.PathEscape(id), nil, nil, "")
}
func (c *Client) ListPermissionGroups(ctx context.Context) (api.PermissionGroupsResponse, error) {
	out := api.PermissionGroupsResponse{Groups: []api.PermissionGroup{}}
	cursor := ""
	for {
		var page api.PermissionGroupsResponse
		if err := c.permissionRequest(ctx, http.MethodGet, "/api/groups?limit=100&cursor="+url.QueryEscape(cursor), nil, &page, ""); err != nil {
			return out, err
		}
		out.Groups = append(out.Groups, page.Groups...)
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}
func (c *Client) GetPermissionGroup(ctx context.Context, id string) (api.PermissionGroup, error) {
	var out api.PermissionGroup
	err := c.permissionRequest(ctx, http.MethodGet, "/api/groups/"+url.PathEscape(id), nil, &out, "")
	return out, err
}
func (c *Client) SavePermissionGroup(ctx context.Context, id, name, revision string) (api.PermissionGroup, error) {
	var out api.PermissionGroup
	method, path := http.MethodPost, "/api/groups"
	if id != "" {
		method = http.MethodPatch
		path += "/" + url.PathEscape(id)
	}
	err := c.permissionRequest(ctx, method, path, struct {
		Name string `json:"name"`
	}{name}, &out, revision)
	return out, err
}
func (c *Client) DeletePermissionGroup(ctx context.Context, id string) error {
	return c.permissionRequest(ctx, http.MethodDelete, "/api/groups/"+url.PathEscape(id), nil, nil, "")
}
func (c *Client) ListPermissionGroupMembers(ctx context.Context, id string) (api.PermissionGroupMembersResponse, error) {
	out := api.PermissionGroupMembersResponse{MemberIDs: []string{}}
	cursor := ""
	for {
		var page api.PermissionGroupMembersResponse
		err := c.permissionRequest(ctx, http.MethodGet, "/api/groups/"+url.PathEscape(id)+"/members?limit=100&cursor="+url.QueryEscape(cursor), nil, &page, "")
		if err != nil {
			return out, err
		}
		out.MemberIDs = append(out.MemberIDs, page.MemberIDs...)
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}
func (c *Client) SetPermissionGroupMember(ctx context.Context, group, user string, add bool) error {
	method := http.MethodDelete
	if add {
		method = http.MethodPut
	}
	return c.permissionRequest(ctx, method, "/api/groups/"+url.PathEscape(group)+"/members/"+url.PathEscape(user), nil, nil, "")
}
func (c *Client) ListRoleGrants(ctx context.Context, filter url.Values) (api.RoleGrantsResponse, error) {
	var out api.RoleGrantsResponse
	err := c.permissionRequest(ctx, http.MethodGet, "/api/role-grants?"+filter.Encode(), nil, &out, "")
	return out, err
}
func (c *Client) GetRoleGrant(ctx context.Context, id string) (api.RoleGrant, error) {
	var out api.RoleGrant
	err := c.permissionRequest(ctx, http.MethodGet, "/api/role-grants/"+url.PathEscape(id), nil, &out, "")
	return out, err
}
func (c *Client) CreateRoleGrants(ctx context.Context, input api.RoleGrantInput) (api.RoleGrantsResponse, error) {
	var out api.RoleGrantsResponse
	err := c.permissionRequest(ctx, http.MethodPost, "/api/role-grants", input, &out, "")
	return out, err
}
func (c *Client) UpdateRoleGrant(ctx context.Context, id string, input api.RoleGrantInput, revision string) (api.RoleGrant, error) {
	var out api.RoleGrant
	err := c.permissionRequest(ctx, http.MethodPatch, "/api/role-grants/"+url.PathEscape(id), input, &out, revision)
	return out, err
}
func (c *Client) DeleteRoleGrant(ctx context.Context, id, revision string) error {
	return c.permissionRequest(ctx, http.MethodDelete, "/api/role-grants/"+url.PathEscape(id), nil, nil, revision)
}
func (c *Client) ListAccessTargets(ctx context.Context, project string) (api.AccessTargetsResponse, error) {
	var out api.AccessTargetsResponse
	err := c.permissionRequest(ctx, http.MethodGet, "/api/projects/"+url.PathEscape(project)+"/access-targets", nil, &out, "")
	return out, err
}
func (c *Client) ListMembers(ctx context.Context) (api.ListMembersResponse, error) {
	var out api.ListMembersResponse
	err := c.permissionRequest(ctx, http.MethodGet, "/api/members", nil, &out, "")
	return out, err
}
func (c *Client) MemberPermissions(ctx context.Context, user string, filter url.Values) (api.MemberPermissionsResponse, error) {
	var out api.MemberPermissionsResponse
	err := c.permissionRequest(ctx, http.MethodGet, "/api/members/"+url.PathEscape(user)+"/permissions?"+filter.Encode(), nil, &out, "")
	return out, err
}
