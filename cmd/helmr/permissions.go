package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/spf13/cobra"
)

// executeCommand formats pre-RunE failures too, including flag parsing errors.
func executeCommand(root *cobra.Command, args []string) error {
	root.SetArgs(args)
	cmd, err := root.ExecuteC()
	if err == nil {
		return nil
	}
	permissionCommand := false
	for current := cmd; current != nil; current = current.Parent() {
		if current.Name() == "role" || current.Name() == "group" || current.Name() == "member" {
			permissionCommand = true
		}
	}
	jsonOutput := false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--json" {
			jsonOutput = true
		}
		if value, ok := strings.CutPrefix(arg, "--json="); ok {
			if parsed, err := strconv.ParseBool(value); err == nil {
				jsonOutput = parsed
			}
		}
	}
	if permissionCommand && jsonOutput {
		var already map[string]json.RawMessage
		if json.Unmarshal([]byte(err.Error()), &already) == nil && already["error"] != nil {
			return err
		}
		return permissionJSONError(err)
	}
	return err
}

func permissionNamespace(name, description string) *cobra.Command {
	cmd := &cobra.Command{Use: name, Short: description, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	cmd.PersistentFlags().Bool("json", false, "Emit one JSON object; errors are JSON on stderr.")
	return cmd
}
func permissionRun(run func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		err := run(cmd, args)
		if err == nil {
			return nil
		}
		jsonOutput, _ := cmd.Flags().GetBool("json")
		if !jsonOutput {
			return err
		}
		return permissionJSONError(err)
	}
}

func permissionJSONError(err error) error {
	code := "invalid_input"
	details := any(nil)
	if remote, ok := errors.AsType[*httpclient.Error](err); ok {
		code = remote.Code
		if code == "" {
			code = "request_failed"
		}
		if len(remote.Details) > 0 {
			details = remote.Details
		}
	}
	if impact, ok := errors.AsType[*permissionImpactError](err); ok {
		code = "confirmation_required"
		details = map[string]any{"grants": impact.grants}
	}
	raw, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": err.Error(), "details": details}})
	return &formattedPermissionError{text: string(raw), cause: err}
}
func permissionOutput(cmd *cobra.Command, value any) error {
	jsonOutput, _ := cmd.Flags().GetBool("json")
	if jsonOutput {
		return writeJSON(cmd.OutOrStdout(), value)
	}
	out := cmd.OutOrStdout()
	switch v := value.(type) {
	case api.PermissionRolesResponse:
		for _, r := range v.Roles {
			fmt.Fprintf(out, "%s\t%s\t%s\n", r.ID, r.Name, strings.Join(r.Permissions, ", "))
		}
	case api.PermissionRole:
		fmt.Fprintf(out, "%s (%s)\n%s\nPermissions: %s\nRevision: %d\nAssignments: %d\n", v.Name, v.ID, v.Description, strings.Join(v.Permissions, ", "), v.Revision, v.GrantCount)
	case api.PermissionGroupsResponse:
		for _, g := range v.Groups {
			fmt.Fprintf(out, "%s\t%s\t%d members\n", g.ID, g.Name, len(g.MemberIDs))
		}
	case api.PermissionGroup:
		fmt.Fprintf(out, "%s (%s)\nRevision: %d\nMembers: %s\n", v.Name, v.ID, v.Revision, strings.Join(v.MemberIDs, ", "))
	case api.PermissionGroupMembersResponse:
		for _, id := range v.MemberIDs {
			fmt.Fprintln(out, id)
		}
	case api.RoleGrantsResponse:
		for _, g := range v.Grants {
			if err := permissionOutput(cmd, g); err != nil {
				return err
			}
		}
	case api.RoleGrant:
		writeGrantSummary(out, v)

	case api.MemberPermissionsResponse:
		fmt.Fprintf(out, "Member %s (owner=%t, %s)\n", v.MembershipID, v.IsOwner, v.Status)
		for _, scope := range v.Scopes {
			target := scope.Agent
			if target == "" {
				target = "Entire environment"
			}
			fmt.Fprintf(out, "%s/%s/%s: %s\n", scope.Project, scope.Environment, target, strings.Join(scope.Permissions, ", "))
			for _, source := range scope.Sources {
				fmt.Fprintf(out, "  %s: %s → %s (%s %s): %s\n", source.Kind, source.GroupName, source.RoleName, source.TargetKind, source.Agent, strings.Join(source.Permissions, ", "))
			}
		}
		fmt.Fprintln(out, v.Notice)
	default:
		return writeJSON(out, value)
	}
	return nil
}
func writeGrantSummary(out io.Writer, v api.RoleGrant) {

	target := strings.Join(v.AgentKeys, ", ")
	if target == "" {
		target = v.TargetKind
	}
	subject := v.GroupName
	if v.SubjectKind == "api_key" {
		subject = v.APIKeyName
	}
	if v.SubjectKind == "member" {
		subject = v.MemberName
	}
	if v.SubjectKind == "all_members" {
		subject = "All members"
	}
	if v.TargetKind == "organization" {
		target = "Organization"
	}
	fmt.Fprintf(out, "%s\t%s → %s\t%s/%s/%s\n", v.ID, subject, v.RoleName, v.Project, v.Environment, target)
	for _, t := range v.Targets {
		selection := t.Mode
		if t.Mode == "selected" {
			selection = strings.Join(t.AgentKeys, ", ")
			if selection == "" {
				selection = "No remaining Agent targets"
			}
		}
		fmt.Fprintf(out, "  %s: %s; actions: %s\n", t.ResourceType, selection, strings.Join(t.Permissions, ", "))
	}
	fmt.Fprintf(out, "  Assignment revision: %d; Role revision: %d\n", v.Revision, v.RoleRevision)

}

func grantPermissionPreview(g api.RoleGrant, permissions []string) api.RoleGrant {
	g.Permissions = permissions
	g.Targets = []api.GrantTarget{}
	for _, p := range permissions {
		resource := auth.PermissionResourceType(auth.Permission(p))
		i := -1
		for j, t := range g.Targets {
			if t.ResourceType == resource {
				i = j
			}
		}
		if i < 0 {
			t := api.GrantTarget{ResourceType: resource, Mode: "environment"}
			if g.EnvironmentID == "" {
				t.Mode = "organization"
			}
			if auth.IsAgentPermission(auth.Permission(p)) {
				t.Mode = g.AgentsMode
				t.AgentIDs = g.AgentIDs
				t.AgentKeys = g.AgentKeys
				if t.Mode == "none" {
					t.Mode = "targets required"
				}
			}
			g.Targets = append(g.Targets, t)
			i = len(g.Targets) - 1
		}
		g.Targets[i].Permissions = append(g.Targets[i].Permissions, p)
	}
	return g
}

func permissionID(value, kind string) (string, error) {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil() {
		return "", fmt.Errorf("%s must be a nonzero UUID", kind)
	}
	return id.String(), nil
}
func permissionRole(ctx context.Context, c *client.Client, name, id, builtin string) (api.PermissionRole, error) {
	count := 0
	for _, v := range []string{name, id, builtin} {
		if v != "" {
			count++
		}
	}
	if count != 1 {
		return api.PermissionRole{}, errors.New("select exactly one role name, --role-id or --builtin")
	}
	if id != "" {
		normalized, err := permissionID(id, "role ID")
		if err != nil {
			return api.PermissionRole{}, err
		}
		return c.GetPermissionRole(ctx, normalized)
	}
	list, err := c.ListPermissionRoles(ctx)
	if err != nil {
		return api.PermissionRole{}, err
	}
	for _, role := range list.Roles {
		if name != "" && strings.EqualFold(role.Name, name) || builtin != "" && role.BuiltinKey != nil && *role.BuiltinKey == builtin {
			return c.GetPermissionRole(ctx, role.ID)
		}
	}
	if builtin != "" {
		return api.PermissionRole{}, fmt.Errorf("built-in role %q not found", builtin)
	}
	return api.PermissionRole{}, fmt.Errorf("role %q not found", name)
}
func permissionGroup(ctx context.Context, c *client.Client, name, id string) (api.PermissionGroup, error) {
	if (name == "") == (id == "") {
		return api.PermissionGroup{}, errors.New("select exactly one group name or --group-id")
	}
	if id != "" {
		normalized, err := permissionID(id, "group ID")
		if err != nil {
			return api.PermissionGroup{}, err
		}
		return c.GetPermissionGroup(ctx, normalized)
	}
	list, err := c.ListPermissionGroups(ctx)
	if err != nil {
		return api.PermissionGroup{}, err
	}
	for _, group := range list.Groups {
		if strings.EqualFold(group.Name, name) {
			return c.GetPermissionGroup(ctx, group.ID)
		}
	}
	return api.PermissionGroup{}, fmt.Errorf("group %q not found", name)
}
func permissionUser(ctx context.Context, c *client.Client, email, id string, self bool) (string, error) {
	count := 0
	if email != "" {
		count++
	}
	if id != "" {
		count++
	}
	if self {
		count++
	}
	if count != 1 {
		return "", errors.New("select exactly one --user, --member-id or --self")
	}
	if self {
		me, err := c.GetOrganizationContext(ctx)
		return me.CurrentMember.ID, err
	}
	if id != "" {
		return permissionID(id, "member ID")
	}
	members, err := c.ListMembers(ctx)
	if err != nil {
		return "", err
	}
	matches := []string{}
	for _, member := range members.Members {
		if member.Status == "active" && member.Email != "" && strings.EqualFold(member.Email, email) {
			matches = append(matches, member.MembershipID)
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("email lookup matched %d members; use --member-id (members may have no email)", len(matches))
	}
	return matches[0], nil
}
func allRoleGrants(ctx context.Context, c *client.Client, filter url.Values) (api.RoleGrantsResponse, error) {
	out := api.RoleGrantsResponse{Grants: []api.RoleGrant{}}
	filter.Set("limit", "100")
	for {
		page, err := c.ListRoleGrants(ctx, filter)
		if err != nil {
			return out, err
		}
		out.Grants = append(out.Grants, page.Grants...)
		if page.NextCursor == "" {
			return out, nil
		}
		filter.Set("cursor", page.NextCursor)
	}
}

type permissionImpactError struct{ grants []api.RoleGrant }

func (e *permissionImpactError) Error() string {
	return fmt.Sprintf("requires --yes; %d role grants are affected", len(e.grants))
}
func permissionConfirm(cmd *cobra.Command, grants []api.RoleGrant) error {
	yes, _ := cmd.Flags().GetBool("yes")
	if yes {
		return nil
	}
	jsonOutput, _ := cmd.Flags().GetBool("json")
	if !jsonOutput {
		for _, g := range grants {
			writeGrantSummary(cmd.ErrOrStderr(), g)
		}
	}
	return &permissionImpactError{grants: grants}
}
func roleCommand() *cobra.Command {
	root := permissionNamespace("role", "Manage reusable roles and scoped role grants.")
	for _, action := range []string{"list", "get", "create", "update", "copy", "delete"} {
		root.AddCommand(roleActionCommand(action))
	}
	root.AddCommand(roleGrantCommand())
	return root
}
func roleActionCommand(action string) *cobra.Command {
	var id, builtin, name, description, revision string
	var permissions []string
	cmd := &cobra.Command{Use: action, Short: action + " roles."}
	switch action {
	case "get", "update", "copy", "delete":
		cmd.Use += " [ROLE]"
		cmd.Flags().StringVar(&id, "role-id", "", "Stable role ID instead of ROLE.")
		cmd.Flags().StringVar(&builtin, "builtin", "", "Standard role key instead of ROLE.")
	case "create":
		cmd.Use += " NAME"
	}
	if action == "create" || action == "update" {
		cmd.Flags().StringVar(&description, "description", "", "Role description.")
		cmd.Flags().StringArrayVar(&permissions, "permission", nil, "Public permission ID; repeat for the complete set. "+permissionHelp())
		_ = cmd.RegisterFlagCompletionFunc("permission", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			ids := []string{}
			for _, cap := range auth.PermissionCatalog() {
				ids = append(ids, string(cap.ID)+"\t"+cap.Name)
			}
			return ids, cobra.ShellCompDirectiveNoFileComp
		})
	}
	if action == "update" || action == "copy" {
		cmd.Flags().StringVar(&name, "name", "", "New role name.")
	}
	if action == "update" {
		cmd.Flags().StringVar(&revision, "if-match", "", "Revision from an earlier read; otherwise use the fetched revision.")
	}
	if action == "delete" || action == "update" {
		cmd.Flags().Bool("yes", false, "Confirm deletion or changed actions on assigned roles.")
	}
	cmd.RunE = permissionRun(func(cmd *cobra.Command, args []string) error {
		if action == "list" {
			if len(args) != 0 {
				return errors.New("list takes no arguments")
			}
		} else if action == "create" {
			if len(args) != 1 {
				return errors.New("create requires NAME")
			}
			if len(permissions) == 0 {
				return errors.New("at least one --permission is required")
			}
		} else if len(args) > 1 {
			return errors.New("expected at most one ROLE")
		}
		c, err := sessionControlPlaneClient(cmd)
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		if action == "list" {
			out, err := c.ListPermissionRoles(ctx)
			if err != nil {
				return err
			}
			return permissionOutput(cmd, out)
		}
		if action == "create" {
			out, err := c.CreatePermissionRole(ctx, api.PermissionRoleInput{Name: args[0], Description: description, Permissions: permissions})
			if err != nil {
				return err
			}
			return permissionOutput(cmd, out)
		}
		roleName := ""
		if len(args) == 1 {
			roleName = args[0]
		}
		role, err := permissionRole(ctx, c, roleName, id, builtin)
		if err != nil {
			return err
		}
		switch action {
		case "get":
			return permissionOutput(cmd, role)
		case "copy":
			if name == "" {
				return errors.New("copy requires --name")
			}
			out, err := c.CreatePermissionRole(ctx, api.PermissionRoleInput{Name: name, Description: role.Description, Permissions: role.Permissions})
			if err != nil {
				return err
			}
			return permissionOutput(cmd, out)
		case "update":
			patch := api.PermissionRolePatch{}
			if cmd.Flags().Changed("name") {
				patch.Name = &name
			}
			if cmd.Flags().Changed("description") {
				patch.Description = &description
			}
			if cmd.Flags().Changed("permission") {
				patch.Permissions = &permissions
			}
			if patch.Name == nil && patch.Description == nil && patch.Permissions == nil {
				return errors.New("provide --name, --description or --permission")
			}
			if revision == "" {
				revision = strconv.FormatInt(role.Revision, 10)
			}
			impact, err := allRoleGrants(ctx, c, url.Values{"role_id": {role.ID}})
			if err != nil {
				return err
			}
			jsonOutput, _ := cmd.Flags().GetBool("json")
			if !jsonOutput {
				fmt.Fprintf(cmd.ErrOrStderr(), "%d role grants affected\n", len(impact.Grants))
				for _, grant := range impact.Grants {
					writeGrantSummary(cmd.ErrOrStderr(), grant)
				}
			}
			if patch.Permissions != nil && len(impact.Grants) > 0 {
				previews := make([]api.RoleGrant, len(impact.Grants))
				for i, g := range impact.Grants {
					previews[i] = grantPermissionPreview(g, *patch.Permissions)
				}
				if !jsonOutput {
					fmt.Fprintf(cmd.ErrOrStderr(), "Role revision: %s; proposed actions: %s\n", revision, strings.Join(*patch.Permissions, ", "))
				}
				if err := permissionConfirm(cmd, previews); err != nil {
					return err
				}
			}
			out, err := c.UpdatePermissionRole(ctx, role.ID, patch, revision)
			if err != nil {
				return err
			}
			if jsonOutput {
				return permissionOutput(cmd, struct {
					api.PermissionRole
					AffectedGrants []api.RoleGrant `json:"affected_grants"`
				}{out, impact.Grants})
			}
			return permissionOutput(cmd, out)
		case "delete":
			impact, err := allRoleGrants(ctx, c, url.Values{"role_id": {role.ID}})
			if err != nil {
				return err
			}
			if err = permissionConfirm(cmd, impact.Grants); err != nil {
				return err
			}
			if err = c.DeletePermissionRole(ctx, role.ID); err != nil {
				return err
			}
			return permissionOutput(cmd, map[string]any{"deleted": role.ID})
		}
		return nil
	})
	return cmd
}
func permissionHelp() string {
	ids := []string{}
	for _, cap := range auth.PermissionCatalog() {
		ids = append(ids, string(cap.ID))
	}
	return strings.Join(ids, ", ")
}

func groupCommand() *cobra.Command {
	root := permissionNamespace("group", "Manage permission groups and their members.")
	for _, action := range []string{"list", "get", "create", "update", "delete"} {
		root.AddCommand(groupActionCommand(action))
	}
	members := &cobra.Command{Use: "member", Short: "Manage group membership."}
	for _, action := range []string{"list", "add", "remove"} {
		members.AddCommand(groupMemberCommand(action))
	}
	root.AddCommand(members)
	return root
}
func groupActionCommand(action string) *cobra.Command {
	var id, name, revision string
	cmd := &cobra.Command{Use: action, Short: action + " groups."}
	if action == "create" {
		cmd.Use += " NAME"
	} else if action != "list" {
		cmd.Use += " [GROUP]"
		cmd.Flags().StringVar(&id, "group-id", "", "Stable group ID instead of GROUP.")
	}
	if action == "update" {
		cmd.Flags().StringVar(&name, "name", "", "New group name.")
		cmd.Flags().StringVar(&revision, "if-match", "", "Revision from an earlier read.")
	}
	if action == "delete" {
		cmd.Flags().Bool("yes", false, "Confirm deletion of the group and its role grants.")
	}
	cmd.RunE = permissionRun(func(cmd *cobra.Command, args []string) error {
		if action == "create" && len(args) != 1 {
			return errors.New("create requires NAME")
		}
		if action == "list" && len(args) != 0 || len(args) > 1 {
			return errors.New("unexpected arguments")
		}
		c, err := sessionControlPlaneClient(cmd)
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		if action == "list" {
			out, err := c.ListPermissionGroups(ctx)
			if err != nil {
				return err
			}
			return permissionOutput(cmd, out)
		}
		if action == "create" {
			out, err := c.SavePermissionGroup(ctx, "", args[0], "")
			if err != nil {
				return err
			}
			return permissionOutput(cmd, out)
		}
		ref := ""
		if len(args) == 1 {
			ref = args[0]
		}
		group, err := permissionGroup(ctx, c, ref, id)
		if err != nil {
			return err
		}
		switch action {
		case "get":
			return permissionOutput(cmd, group)
		case "update":
			if !cmd.Flags().Changed("name") {
				return errors.New("update requires --name")
			}
			if revision == "" {
				revision = strconv.FormatInt(group.Revision, 10)
			}
			out, err := c.SavePermissionGroup(ctx, group.ID, name, revision)
			if err != nil {
				return err
			}
			return permissionOutput(cmd, out)
		case "delete":
			impact, err := allRoleGrants(ctx, c, url.Values{"group_id": {group.ID}})
			if err != nil {
				return err
			}
			if err = permissionConfirm(cmd, impact.Grants); err != nil {
				return err
			}
			if err = c.DeletePermissionGroup(ctx, group.ID); err != nil {
				return err
			}
			return permissionOutput(cmd, map[string]any{"deleted": group.ID, "removed_grants": len(impact.Grants)})
		}
		return nil
	})
	return cmd
}
func groupMemberCommand(action string) *cobra.Command {
	var groupID, email, userID string
	cmd := &cobra.Command{Use: action + " [GROUP]", Short: action + " group members."}
	cmd.Flags().StringVar(&groupID, "group-id", "", "Stable group ID instead of GROUP.")
	if action != "list" {
		cmd.Flags().StringVar(&email, "user", "", "Exact member email.")
		cmd.Flags().StringVar(&userID, "member-id", "", "Stable member ID.")
	}
	if action == "remove" {
		cmd.Flags().Bool("yes", false, "Confirm removal when the group holds role grants.")
	}
	cmd.RunE = permissionRun(func(cmd *cobra.Command, args []string) error {
		if len(args) > 1 {
			return errors.New("expected at most one GROUP")
		}
		c, err := sessionControlPlaneClient(cmd)
		if err != nil {
			return err
		}
		ref := ""
		if len(args) == 1 {
			ref = args[0]
		}
		group, err := permissionGroup(cmd.Context(), c, ref, groupID)
		if err != nil {
			return err
		}
		if action == "list" {
			out, err := c.ListPermissionGroupMembers(cmd.Context(), group.ID)
			if err != nil {
				return err
			}
			return permissionOutput(cmd, out)
		}
		user, err := permissionUser(cmd.Context(), c, email, userID, false)
		if err != nil {
			return err
		}
		if action == "remove" {
			impact, err := allRoleGrants(cmd.Context(), c, url.Values{"group_id": {group.ID}})
			if err != nil {
				return err
			}
			if len(impact.Grants) > 0 {
				if err = permissionConfirm(cmd, impact.Grants); err != nil {
					return err
				}
			}
		}
		if err = c.SetPermissionGroupMember(cmd.Context(), group.ID, user, action == "add"); err != nil {
			return err
		}
		return permissionOutput(cmd, map[string]any{"group_id": group.ID, "member_id": user, "member": action == "add"})
	})
	return cmd
}

func roleGrantCommand() *cobra.Command {
	root := &cobra.Command{Use: "grant", Short: "Manage role assignments to people, Groups, API keys or All members at an explicit scope."}
	for _, action := range []string{"list", "get", "create", "update", "delete"} {
		root.AddCommand(roleGrantActionCommand(action))
	}
	return root
}
func roleGrantActionCommand(action string) *cobra.Command {
	var id, revision, groupName, groupID, roleName, roleID, project, env, memberEmail, memberID, apiKeyID string
	var agentKeys []string
	var allUsers, allAgents, organization bool
	cmd := &cobra.Command{Use: action, Short: action + " role assignments with targets by resource type."}
	cmd.Flags().StringVar(&id, "id", "", "Assignment ID for get, update or delete.")
	cmd.Flags().StringVar(&revision, "if-match", "", "Observed assignment revision; defaults to the fetched revision.")
	cmd.Flags().StringVar(&memberEmail, "user", "", "Exact active member email.")
	cmd.Flags().StringVar(&memberID, "member-id", "", "Stable membership ID.")
	cmd.Flags().StringVar(&apiKeyID, "api-key-id", "", "Stable API key identity ID, not its secret.")
	cmd.Flags().StringVar(&groupName, "group", "", "Exact group name.")
	cmd.Flags().StringVar(&groupID, "group-id", "", "Stable group ID.")
	cmd.Flags().BoolVar(&allUsers, "all-users", false, "All active organization members; selected Agent use only.")
	cmd.Flags().StringVar(&roleName, "role", "", "Exact role name.")
	cmd.Flags().StringVar(&roleID, "role-id", "", "Stable role ID.")
	cmd.Flags().StringVar(&project, "project", "", "Project identifier.")
	cmd.Flags().StringVar(&env, "env", "", "One environment identifier.")
	cmd.Flags().StringArrayVar(&agentKeys, "agent", nil, "Selected Agent key; repeat for the complete target set.")
	cmd.Flags().BoolVar(&allAgents, "all-agents", false, "Explicitly include all current and future Agents in this environment.")
	cmd.Flags().BoolVar(&organization, "organization", false, "Organization administration assignment.")
	cmd.Flags().Bool("yes", false, "Confirm assignment update or deletion after reviewing targets.")
	cmd.RunE = permissionRun(func(cmd *cobra.Command, args []string) error {
		if len(args) != 0 {
			return errors.New("grant commands take flags, not positional arguments")
		}
		c, err := sessionControlPlaneClient(cmd)
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		input := api.RoleGrantInput{AgentsMode: "none", AgentIDs: []string{}}
		var old api.RoleGrant
		if action == "get" || action == "update" || action == "delete" {
			normalized, err := permissionID(id, "assignment ID")
			if err != nil {
				return err
			}
			old, err = c.GetRoleGrant(ctx, normalized)
			if err != nil {
				return err
			}
			if action == "get" {
				return permissionOutput(cmd, old)
			}
			if revision == "" {
				revision = strconv.FormatInt(old.Revision, 10)
			}
			if action == "delete" {
				if err := permissionConfirm(cmd, []api.RoleGrant{old}); err != nil {
					return err
				}
				if err := c.DeleteRoleGrant(ctx, old.ID, revision); err != nil {
					return err
				}
				return permissionOutput(cmd, map[string]any{"deleted": old.ID})
			}
			if apiKeyID != "" || memberEmail != "" || memberID != "" || groupName != "" || groupID != "" || allUsers || roleName != "" || roleID != "" || project != "" || env != "" || organization {
				return errors.New("update preserves recipient, role and environment; supply the complete Agent selection")
			}
			input.Subject = api.GrantSubject{Type: old.SubjectKind, MemberID: old.MemberID, GroupID: old.GroupID, APIKeyID: old.APIKeyID}
			roleID = old.RoleID
			project = old.Project
			env = old.Environment
			organization = old.TargetKind == "organization"
		}
		filter := url.Values{}
		if action != "update" {
			count := 0
			if memberEmail != "" || memberID != "" {
				member, err := permissionUser(ctx, c, memberEmail, memberID, false)
				if err != nil {
					return err
				}
				input.Subject = api.GrantSubject{Type: "member", MemberID: &member}
				filter.Set("member_id", member)
				count++
			}
			if groupName != "" || groupID != "" {
				group, err := permissionGroup(ctx, c, groupName, groupID)
				if err != nil {
					return err
				}
				input.Subject = api.GrantSubject{Type: "group", GroupID: &group.ID}
				filter.Set("group_id", group.ID)
				count++
			}
			if apiKeyID != "" {
				key, err := permissionID(apiKeyID, "API key ID")
				if err != nil {
					return err
				}
				input.Subject = api.GrantSubject{Type: "api_key", APIKeyID: &key}
				filter.Set("api_key_id", key)
				count++
			}
			if allUsers {
				input.Subject = api.GrantSubject{Type: "all_members"}
				filter.Set("subject_kind", "all_members")
				count++
			}
			if count > 1 || action == "create" && count != 1 {
				return errors.New("select exactly one person, group, --api-key-id or --all-users")
			}
		}
		var role api.PermissionRole
		if roleName != "" || roleID != "" {
			role, err = permissionRole(ctx, c, roleName, roleID, "")
			if err != nil {
				return err
			}
			input.RoleID = role.ID
			input.ExpectedRoleRevision = role.Revision
			filter.Set("role_id", role.ID)
		} else if action != "list" {
			return errors.New("select --role or --role-id")
		}
		if organization && (project != "" || env != "" || len(agentKeys) > 0 || allAgents) {
			return errors.New("organization assignments have no workload targets")
		}
		if env != "" && project == "" || len(agentKeys) > 0 && env == "" {
			return errors.New("agent selection requires --project and --env")
		}
		if project != "" {
			selected, err := resolveProject(ctx, c, project)
			if err != nil {
				return err
			}
			project = selected.Project
			filter.Set("project", project)
		}
		if env != "" {
			targets, err := c.ListAccessTargets(ctx, project)
			if err != nil {
				return err
			}
			input.ProjectID = &targets.ProjectID
			var selected *api.AccessTarget
			for i := range targets.Targets {
				t := &targets.Targets[i]
				if t.Environment == env || t.EnvironmentID == env {
					selected = t
				}
			}
			if selected == nil {
				return fmt.Errorf("environment %q not found in project %q", env, project)
			}
			input.EnvironmentID = &selected.EnvironmentID
			filter.Set("environment", selected.Environment)
			for _, key := range agentKeys {
				found := ""
				for _, a := range selected.Agents {
					if a.Key == key {
						found = a.ID
					}
				}
				if found == "" {
					return fmt.Errorf("agent %q not found in the selected environment", key)
				}
				input.AgentIDs = append(input.AgentIDs, found)
			}
		}
		if action == "list" {
			if len(agentKeys) > 1 {
				return errors.New("list accepts at most one --agent filter")
			}
			if len(agentKeys) == 1 {
				filter.Set("agent", agentKeys[0])
			}
			out, err := allRoleGrants(ctx, c, filter)
			if err != nil {
				return err
			}
			return permissionOutput(cmd, out)
		}
		hasAgents := false
		for _, p := range role.Permissions {
			if auth.IsAgentPermission(auth.Permission(p)) {
				hasAgents = true
			}
		}
		if hasAgents {
			if allAgents == (len(agentKeys) > 0) {
				return errors.New("choose --all-agents or one or more --agent targets")
			}
			input.AgentsMode = "selected"
			if allAgents {
				input.AgentsMode = "all"
			}
		} else if allAgents || len(agentKeys) > 0 {
			return errors.New("this role has no explicit Agent permissions")
		}
		if !organization && input.EnvironmentID == nil {
			return errors.New("workload assignments require --project and --env")
		}
		if action == "update" {
			preview := old
			preview.AgentsMode = input.AgentsMode
			preview.AgentIDs = input.AgentIDs
			preview.AgentKeys = agentKeys
			preview = grantPermissionPreview(preview, role.Permissions)
			preview.RoleRevision = role.Revision
			if err := permissionConfirm(cmd, []api.RoleGrant{preview}); err != nil {
				return err
			}
			out, err := c.UpdateRoleGrant(ctx, old.ID, input, revision)
			if err != nil {
				return err
			}
			return permissionOutput(cmd, out)
		}
		out, err := c.CreateRoleGrants(ctx, input)
		if err != nil {
			return err
		}
		return permissionOutput(cmd, out)
	})
	return cmd
}
func memberCommand() *cobra.Command {
	root := permissionNamespace("member", "Inspect organization member permissions.")
	var email, id, project, env, agent string
	var self bool
	cmd := &cobra.Command{Use: "permissions", Short: "Show current effective permissions and their sources; this does not authorize runtime admission."}
	cmd.Flags().StringVar(&email, "user", "", "Exact member email (requires member-management access).")
	cmd.Flags().StringVar(&id, "member-id", "", "Stable member ID.")
	cmd.Flags().BoolVar(&self, "self", false, "Inspect the logged-in member.")
	cmd.Flags().StringVar(&project, "project", "", "Project identifier.")
	cmd.Flags().StringVar(&env, "env", "", "Environment slug.")
	cmd.Flags().StringVar(&agent, "agent", "", "Exact Agent key.")
	cmd.RunE = permissionRun(func(cmd *cobra.Command, args []string) error {
		if len(args) != 0 {
			return errors.New("permissions takes flags, not positional arguments")
		}
		if env != "" && project == "" || agent != "" && (project == "" || env == "") {
			return errors.New("--env requires --project; --agent requires --project and --env")
		}
		c, err := sessionControlPlaneClient(cmd)
		if err != nil {
			return err
		}
		user, err := permissionUser(cmd.Context(), c, email, id, self)
		if err != nil {
			return err
		}
		filter := url.Values{}
		if project != "" {
			selected, err := resolveProject(cmd.Context(), c, project)
			if err != nil {
				return err
			}
			filter.Set("project", selected.Project)
		}
		if env != "" {
			filter.Set("environment", env)
		}
		if agent != "" {
			filter.Set("agent", agent)
		}
		out, err := c.MemberPermissions(cmd.Context(), user, filter)
		if err != nil {
			return err
		}
		return permissionOutput(cmd, out)
	})
	root.AddCommand(cmd)
	return root
}

type formattedPermissionError struct {
	text  string
	cause error
}

func (e *formattedPermissionError) Error() string { return e.text }
func (e *formattedPermissionError) Unwrap() error { return e.cause }
