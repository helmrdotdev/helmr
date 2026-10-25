import { expect, test, type Page } from "@playwright/test";

// The access pages (members, groups, roles, an agent's access) against a
// synthetic API fixture.

const projectID = "console-demo";
const environmentID = "production";
const environmentUUID = "00000000-0000-7000-8000-000000000e01";
const agentID = "00000000-0000-7000-8000-000000000a01";
const now = "2026-10-14T00:00:00Z";

const catalog = [
  { resource_type: "assignments", id: "assignments.create", name: "Manage access", description: "Manage members and access.", scope_type: "organization", api_key_assignable: false },
  { resource_type: "agents", id: "agents.use", name: "Use agents", description: "Use assigned agents.", scope_type: "agent", api_key_assignable: true },
  { resource_type: "sessions", id: "sessions.read", name: "View all sessions", description: "Read all sessions.", scope_type: "agent", api_key_assignable: true },
  { resource_type: "secrets", id: "secrets.rotate", name: "Manage secrets", description: "Manage secrets.", scope_type: "environment", api_key_assignable: true },
];
const role = (id: string, name: string, permissions: string[], builtin: string | null, extra: Record<string, unknown> = {}) =>
  ({ id, name, description: `${name} description`, builtin_key: builtin, permissions, api_key_assignable: !permissions.includes("assignments.create"), grant_count: 0, group_count: 0, updated_at: now, revision: 1, ...extra });
const roles = [
  role("role-user", "Agent user", ["agents.use"], "agent_user", { grant_count: 1 }),
  role("role-viewer", "Session viewer", ["sessions.read"], "session_viewer", { grant_count: 1 }),
  role("role-admin", "Access administrator", ["assignments.create"], "access_administrator", { grant_count: 1 }),
  role("role-ops", "Agent and secrets operator", ["agents.use", "secrets.rotate"], null, { grant_count: 1 }),
];
const grant = (id: string, subject: Record<string, unknown>, roleID: string, extra: Record<string, unknown> = {}) => {
  const r = roles.find(x => x.id === roleID)!;
  return {
    id, revision: 1, group_id: null, role_id: roleID, role_name: r.name, permissions: r.permissions, project_id: "p", project: projectID, project_name: "Test",
    environment_id: environmentUUID, environment: environmentID, environment_name: "Production", target_kind: "environment", agents_mode: "selected",
    agent_ids: [agentID], agent_keys: ["assistant"], targets: [], ...subject, ...extra,
  };
};
const grants = [
  grant("grant-admin", { subject_kind: "member", member_id: "m-local", member_name: "Local" }, "role-admin", { target_kind: "organization", agents_mode: "none", agent_ids: [], agent_keys: [], environment_id: "", environment_name: "" }),
  grant("grant-ops", { subject_kind: "group", group_id: "group-release" }, "role-ops"),
  grant("grant-all", { subject_kind: "all_members" }, "role-user"),
  grant("grant-viewer", { subject_kind: "member", member_id: "m-second", member_name: "Second Member" }, "role-viewer"),
];

async function fixture(page: Page) {
  const posts: unknown[] = [];
  const patches: { path: string; body: unknown }[] = [];
  const environment = { environment: environmentID, project: projectID, name: "Production", color_hex: "#008000", is_default: true, created_at: now, updated_at: now };
  const project = { project: projectID, name: "Test", default_region_id: "amsterdam", is_default: true, environments: [environment], created_at: now, updated_at: now };
  await page.route("**/api/**", async route => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    const method = route.request().method();
    let json: unknown;
    if (path === "/api/me") json = { user_id: "local", membership_id: "m-local", display_name: "Local", org_id: "org", is_owner: true, permissions: ["members.read", "members.remove", "invitations.read", "invitations.create", "invitations.revoke", "groups.read", "groups.create", "groups.update", "groups.delete", "groups.add_member", "groups.remove_member", "roles.read", "roles.create", "roles.update", "roles.delete", "assignments.read", "assignments.create", "assignments.update", "assignments.delete", "projects.read", "projects.create", "projects.update", "projects.update_retention", "environments.read", "environments.create", "environments.update"], admin: false, organization_required: false, project_required: false, email_delivery: true };
    else if (path === "/api/projects") json = { projects: [project] };
    else if (path === `/api/projects/${projectID}`) json = project;
    else if (path === `/api/projects/${projectID}/access-targets`) json = { project_id: "p", targets: [{ environment_id: environmentUUID, environment: environmentID, name: "Production", agents: [{ id: agentID, key: "assistant" }, { id: "agent-2", key: "triage" }] }] };
    else if (path === "/api/roles") json = { roles, catalog };
    else if (path === "/api/groups") json = { groups: [{ id: "group-release", name: "Release team", member_ids: ["m-local"], revision: 1 }] };
    else if (path === "/api/role-grants" && method === "POST") { posts.push(route.request().postDataJSON()); json = { grants: [], created: true }; }
    else if (path.startsWith("/api/role-grants/") && method === "PATCH") { patches.push({ path, body: route.request().postDataJSON() }); json = { grants: [] }; }
    else if (path === "/api/role-grants") json = { grants };
    else if (path === "/api/members") json = { members: [
      { user_id: "local", membership_id: "m-local", display_name: "Local", email: "local@example.test", is_owner: true, status: "active", created_at: now, updated_at: now },
      { user_id: "second", membership_id: "m-second", display_name: "Second Member", email: "second@example.test", is_owner: false, status: "active", created_at: now, updated_at: now },
    ] };
    else if (path === "/api/invitations") json = { invitations: [] };
    else if (path === "/api/members/m-local/permissions") json = { user_id: "local", membership_id: "m-local", is_owner: true, status: "active", notice: "", scopes: [
      { type: "organization", project: "", environment: "", permissions: ["assignments.create"], sources: [{ kind: "role_grant", subject_kind: "member", grant_id: "grant-admin", role_name: "Access administrator", permissions: ["assignments.create"] }] },
      { type: "agent", project: projectID, environment: environmentID, agent: "assistant", permissions: ["agents.use", "secrets.rotate"], sources: [
        { kind: "role_grant", subject_kind: "group", group_id: "group-release", group_name: "Release team", grant_id: "grant-ops", role_name: "Agent and secrets operator", permissions: ["agents.use", "secrets.rotate"] },
        { kind: "role_grant", subject_kind: "all_members", grant_id: "grant-all", role_name: "Agent user", permissions: ["agents.use"] },
      ] },
    ] };
    else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: "not found" } } }); return; }
    await route.fulfill({ json });
  });
  return { posts, patches };
}

test("groups list all members as a managed group and open a group's access and members", async ({ page }) => {
  await fixture(page);
  await page.goto("/settings/groups");
  const table = page.getByRole("table");
  const everyone = table.getByRole("row").filter({ hasText: "All members" });
  await expect(everyone.getByRole("img", { name: "Managed by Helmr" })).toBeVisible();
  // All members is managed: its row menu only grants access.
  await everyone.getByRole("button", { name: "Actions for All members", exact: true }).click();
  await expect(page.getByRole("menu", { name: "Actions for All members" }).getByRole("menuitem")).toHaveText(["Grant access"]);
  await page.keyboard.press("Escape");
  await table.getByRole("link", { name: "Release team" }).click();
  await expect(page).toHaveURL(/\/settings\/groups\/group-release$/);
  await expect(page.getByRole("heading", { level: 1, name: "Release team" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Groups", exact: true }).first()).toHaveAttribute("aria-current", "page");
  await expect(page.getByRole("row").filter({ hasText: "Agent and secrets operator" })).toContainText("assistant");
  await page.getByRole("tab", { name: "Members, 1" }).click();
  await expect(page.getByRole("table").getByRole("link", { name: "Local" })).toBeVisible();
});

test("roles list the organization's roles first and lock the ones Helmr maintains", async ({ page }) => {
  await fixture(page);
  await page.goto("/settings/roles");
  const rows = page.getByRole("table").getByRole("row");
  await expect(rows.nth(1)).toContainText("Agent and secrets operator");
  await expect(rows.nth(1).getByRole("img", { name: "Managed by Helmr" })).toHaveCount(0);
  await expect(rows.filter({ hasText: "Agent user" }).getByRole("img", { name: "Managed by Helmr" })).toBeVisible();
  await expect(rows.filter({ hasText: "Access administrator" })).toContainText("Organization");
  await rows.filter({ hasText: "Agent user" }).getByRole("link", { name: "Agent user" }).click();
  await expect(page.getByRole("checkbox", { name: "Use agents" })).toBeDisabled();
  await expect(page.getByRole("button", { name: "Save role" })).toHaveCount(0);
  await page.getByRole("button", { name: "Role actions", exact: true }).click();
  const menu = page.getByRole("menu", { name: "Role actions" });
  await expect(menu.getByRole("menuitem", { name: "Duplicate", exact: true })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: "Delete role", exact: true })).toHaveCount(0);
});

test("a member's effective access names where each permission comes from", async ({ page }) => {
  await fixture(page);
  await page.goto("/settings/members");
  await page.getByRole("link", { name: "Local" }).click();
  await expect(page).toHaveURL(/\/settings\/members\/m-local$/);
  const agentRow = page.getByRole("row").filter({ hasText: "assistant" });
  await expect(agentRow).toContainText("Use agents · Manage secrets");
  await expect(agentRow).toContainText("Release team");
  await expect(agentRow).toContainText("All members");
  await expect(page.getByRole("row").filter({ hasText: "Organization" })).toContainText("Direct");
});

test("granting to all members offers only agent use on selected agents", async ({ page }) => {
  const state = await fixture(page);
  await page.goto("/settings/groups/all-members");
  await page.getByRole("button", { name: "Group actions", exact: true }).click();
  await expect(page.getByRole("menu", { name: "Group actions" }).getByRole("menuitem")).toHaveText(["Grant access"]);
  await page.getByRole("menu", { name: "Group actions" }).getByRole("menuitem", { name: "Grant access", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Grant access" });
  await expect(dialog).toContainText("3 roles can’t be granted to all members.");
  await dialog.getByRole("combobox", { name: "Role" }).click();
  await expect(page.getByRole("option")).toHaveCount(1);
  await page.getByRole("option", { name: "Agent user" }).click();
  await expect(dialog.getByRole("button", { name: "All, including new ones" })).toHaveCount(0);
  await dialog.getByRole("checkbox", { name: "triage" }).check();
  // All members already use assistant through this role, so the grant adds triage to that assignment.
  await expect(dialog).toContainText("All members can use agents on assistant, triage in Production.");
  await expect(dialog).toContainText("All members already has this role on assistant.");
  await dialog.getByRole("button", { name: "Grant access" }).click();
  await expect(dialog).toBeHidden();
  expect(state.posts).toEqual([]);
  expect(state.patches).toEqual([{ path: "/api/role-grants/grant-all", body: expect.objectContaining({ role_id: "role-user", subject: { type: "all_members" }, agents_mode: "selected", agent_ids: [agentID, "agent-2"], environment_id: environmentUUID }) }]);
});

test("an agent's access lists who can use it and adds the agent to an existing assignment", async ({ page }) => {
  const state = await fixture(page);
  await page.goto("/agents/assistant/access");
  await expect(page.getByRole("checkbox", { name: /Share with all members/ })).toBeChecked();
  const people = page.getByRole("table");
  await expect(people.getByRole("row").filter({ hasText: "Second Member" })).toContainText("Use agents · View all sessions");
  await expect(people.getByRole("row").filter({ hasText: "Second Member" })).toContainText("Direct");

  await page.getByRole("button", { name: "Grant access" }).click();
  const dialog = page.getByRole("dialog", { name: "Grant access" });
  await dialog.getByRole("combobox", { name: "Who" }).click();
  await page.getByRole("option", { name: "Second Member" }).click();
  await dialog.getByRole("combobox", { name: "Role" }).click();
  // Organization roles and roles without agent permissions don't apply to one agent.
  await expect(page.getByRole("option")).toHaveText(["Agent and secrets operator", "Agent user", "Session viewer"]);
  await page.getByRole("option", { name: "Session viewer" }).click();
  await expect(dialog).toContainText("Second Member already has this role on assistant.");
  await expect(dialog.getByRole("checkbox", { name: "assistant" })).toBeDisabled();
  await dialog.getByRole("checkbox", { name: "triage" }).check();
  await dialog.getByRole("button", { name: "Grant access" }).click();
  await expect(dialog).toBeHidden();
  expect(state.posts).toEqual([]);
  expect(state.patches).toEqual([{ path: "/api/role-grants/grant-viewer", body: expect.objectContaining({ role_id: "role-viewer", agents_mode: "selected", agent_ids: [agentID, "agent-2"] }) }]);
});
