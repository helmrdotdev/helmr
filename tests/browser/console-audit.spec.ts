import { expect, test, type Page } from "@playwright/test";

// The audit events page against a synthetic API fixture.

const projectID = "console-demo";
const environmentUUID = "00000000-0000-7000-8000-000000000e01";
const roleID = "00000000-0000-7000-8000-0000000000r1".replace("r", "a");
const userID = "00000000-0000-7000-8000-0000000000u1".replace("u", "b");
const sessionID = "00000000-0000-7000-8000-000000000702";
const now = Date.now();
const ago = (minutes: number) => new Date(now - minutes * 60_000).toISOString();

const base = {
  org_id: "org", project_id: null, retention_seconds: 7_776_000, expires_at: new Date(now + 90 * 86_400_000).toISOString(),
  actor_api_key_id: null, actor_session_id: null, actor_worker_host_id: null, actor_component: null, runtime_generation: null, process_epoch: null,
  deployment_id: null, session_id: null, turn_id: null, computer_id: null, cause_event_id: null, schema_version: 1, target_user_id: null,
  slack_team_id: null, slack_user_id: null, target_resolution: "resolved", source_ip: "203.0.113.24", channel: "http", causality: null, details: null,
  authentication: { result: "verified", origin: "request", provider: null, credential_kind: "login_session", credential_id: null, credential_version: 3 },
};
const denied = {
  ...base, id: "00000000-0000-7000-8000-00000000e001", environment_id: environmentUUID, occurred_at: ago(2), recorded_at: ago(2), operation_id: "00000000-0000-7000-8000-00000000f001",
  actor_user_id: userID, actor_identity: { display_name: "Sato Kenji", email: "sato@acme.dev", source: "console" },
  action: "session.content_read", outcome: "denied", target_kind: "session", target_id: sessionID, session_id: sessionID,
  authorization: { decision: "denied", checks: [
    { type: "permission", permission: "sessions.read", result: "failed", reason: "no_matching_grant", subject_user_id: userID, scope: { org_id: "org", environment_id: environmentUUID, agent_id: "demo-triage" } },
    { type: "membership_attribute", member_id: "m-sato", attribute: "is_owner", expected: true, observed: false, result: "failed" },
  ] },
};
const granted = {
  ...base, cause_event_id: "00000000-0000-7000-8000-00000000e001", id: "00000000-0000-7000-8000-00000000e002", environment_id: null, occurred_at: ago(9), recorded_at: ago(9), operation_id: "00000000-0000-7000-8000-00000000f002",
  actor_user_id: "00000000-0000-7000-8000-00000000000c", actor_identity: { display_name: "Aiko Mori", email: "aiko@acme.dev", source: "console" },
  action: "role.updated", outcome: "succeeded", target_kind: "role", target_id: roleID,
  authorization: { decision: "allowed", checks: [{ type: "permission", permission: "roles.update", result: "passed", subject_user_id: "00000000-0000-7000-8000-00000000000c", scope: { org_id: "org" }, evidence: { subject: "member", role_id: "role-admin", role_revision: 3, assignment_id: "00000000-0000-7000-8000-0000000041aa", assignment_revision: 2 } }] },
};
const actions = [
  { action: "session.content_read", target_kind: "session", outcomes: ["succeeded", "denied"], category: "agents" },
  { action: "role.updated", target_kind: "role", outcomes: ["succeeded", "denied"], category: "access" },
  { action: "role.created", target_kind: "role", outcomes: ["succeeded", "denied"], category: "access" },
  { action: "identity.sign_in", target_kind: "user", outcomes: ["succeeded", "failed"], category: "sign_in" },
];

async function fixture(page: Page, options: { permissions?: string[]; paged?: boolean } = {}) {
  const searches: URLSearchParams[] = [];
  const environment = { environment: "production", project: projectID, name: "Production", color_hex: "#008000", is_default: true };
  const project = { project: projectID, name: "Demo", default_region_id: "local", is_default: true, environments: [environment] };
  const permissions = options.permissions ?? ["audit_events.read", "roles.read", "members.read", "groups.read"];
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    let json: unknown;
    if (path === "/api/me") json = { user_id: "local", membership_id: "m-local", display_name: "Local", org_id: "org", is_owner: false, permissions, admin: false, organization_required: false, project_required: false };
    else if (path === "/api/projects") json = { projects: [project] };
    else if (path === `/api/projects/${projectID}`) json = project;
    else if (path === `/api/projects/${projectID}/access-targets`) json = { project_id: "p", targets: [{ environment_id: environmentUUID, environment: "production", name: "Production", agents: [] }] };
    else if (path === "/api/audit-events/actions") json = { actions };
    else if (path === "/api/audit-events") {
      if (!permissions.includes("audit_events.read")) { await route.fulfill({ status: 403, json: { error: { code: "forbidden", message: "denied" } } }); return; }
      searches.push(url.searchParams);
      const target = url.searchParams.get("target_id");
      const outcome = url.searchParams.get("outcome");
      const wanted = url.searchParams.getAll("action");
      const operation = url.searchParams.get("operation_id");
      const id = url.searchParams.get("event_id");
      const events = [denied, granted].filter((e) => (!target || e.target_id === target) && (!outcome || e.outcome === outcome) && (!wanted.length || wanted.includes(e.action)) && (!operation || e.operation_id === operation) && (!id || e.id === id));
      if (options.paged) json = url.searchParams.has("before_id") ? { events: events.slice(1) } : { events: events.slice(0, 1), next_before: { recorded_at: events[0]!.recorded_at, id: events[0]!.id } };
      else json = { events };
    } else if (path === "/api/roles") json = { roles: [{ id: roleID, name: "Release manager", description: "", builtin_key: null, permissions: [], assignment_levels: ["environment"], api_key_assignable: true, grant_count: 0, group_count: 0, updated_at: ago(0), revision: 1 }, { id: "role-admin", name: "Access Administrator", description: "", builtin_key: "access_administrator", permissions: [], assignment_levels: ["organization"], api_key_assignable: false, grant_count: 1, group_count: 0, updated_at: ago(0), revision: 1 }], catalog: [] };
    else if (path === "/api/groups") json = { groups: [] };
    else if (path === "/api/members") json = { members: [{ user_id: userID, membership_id: "m-sato", display_name: "Sato Kenji", email: "sato@acme.dev", is_owner: false, status: "active", created_at: ago(1000) }] };
    else if (path === "/api/role-grants") json = { grants: [] };
    else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } }); return; }
    await route.fulfill({ json });
  });
  return { searches };
}

const open = (page: Page, query = "") => page.goto(`/settings/audit-events?project=${projectID}&environment=production${query ? `&${query}` : ""}`);

test("the audit page opens on the last 24 hours with readable rows", async ({ page }) => {
  const { searches } = await fixture(page);
  await open(page);
  const rows = page.getByRole("row").filter({ has: page.getByRole("cell") });
  await expect(rows).toHaveCount(2);
  await expect(rows.nth(0)).toContainText("Sato Kenji");
  await expect(rows.nth(0)).toContainText("Refused: Read session content · session 000000000702");
  await expect(rows.nth(0)).toContainText("Denied");
  await expect(rows.nth(0)).toContainText("Production");
  await expect(rows.nth(1)).toContainText("Updated role · Release manager");
  await expect(rows.nth(1)).toContainText("Organization");
  const first = searches[0]!;
  const span = Date.parse(first.get("to")!) - Date.parse(first.get("from")!);
  expect(span).toBeGreaterThan(23 * 3_600_000);
  expect(span).toBeLessThan(25 * 3_600_000);
});

test("a row opens beneath itself with the event's fields, its authority checks and the JSON", async ({ page }) => {
  await fixture(page);
  await open(page);
  await page.getByRole("button", { name: /^Show details: Refused/ }).click();
  await expect(page).toHaveURL(/event=00000000-0000-7000-8000-00000000e001/);
  const details = page.locator("#audit-row-00000000-0000-7000-8000-00000000e001");
  await expect(details).toContainText("Read session content");
  await expect(details).toContainText("session.content_read");
  await expect(details).toContainText("Sato Kenji");
  await expect(details).toContainText("Verified on this request");
  await expect(details.getByRole("link", { name: "session 000000000702" })).toHaveAttribute("href", `/sessions/${sessionID}`);
  await details.getByRole("tab", { name: "Authorization, 2" }).click();
  await expect(details).toContainText("2 recorded, 2 failed");
  await expect(details).toContainText("sessions.read");
  await expect(details).toContainText("Agent demo-triage in Production");
  await expect(details).toContainText("No role assignment gives this permission here");
  await details.getByRole("tab", { name: "JSON" }).click();
  await expect(details).toContainText("session.content_read");
  await page.getByRole("button", { name: /^Hide details: Refused/ }).click();
  await expect(details).toHaveCount(0);
});

test("a passed check shows the role that allowed it", async ({ page }) => {
  await fixture(page);
  await open(page);
  await page.getByRole("button", { name: /^Show details: Updated role/ }).click();
  const details = page.locator("#audit-row-00000000-0000-7000-8000-00000000e002");
  await details.getByRole("tab", { name: "Authorization, 1" }).click();
  await expect(details).toContainText("The whole organization");
  await expect(details).toContainText("A direct assignment");
  await expect(details.getByRole("link", { name: "Access Administrator" })).toHaveAttribute("href", "/settings/roles/role-admin");
});

test("filters search by outcome and by every action in a category, and live in the URL", async ({ page }) => {
  const { searches } = await fixture(page);
  await open(page);
  await page.getByRole("group", { name: "Outcome" }).getByRole("button", { name: "Denied" }).click();
  await expect(page).toHaveURL(/outcome=denied/);
  await expect.poll(() => searches.at(-1)?.get("outcome")).toBe("denied");
  await page.getByRole("combobox", { name: "Action" }).click();
  await page.getByRole("option", { name: "Access" }).click();
  await expect(page).toHaveURL(/category=access/);
  await expect.poll(() => searches.at(-1)?.getAll("action").sort()).toEqual(["role.created", "role.updated"]);
});

test("an object's menu opens the audit page about that object", async ({ page }) => {
  const { searches } = await fixture(page);
  await open(page, `target=${roleID}&target_kind=role`);
  await expect(page.getByText("About Release manager")).toBeVisible();
  await expect.poll(() => searches.at(-1)?.get("target_id")).toBe(roleID);
  await expect(page.getByRole("row").filter({ has: page.getByRole("cell") })).toHaveCount(1);
  await page.getByRole("button", { name: "Clear the object filter" }).click();
  await expect(page.getByRole("row").filter({ has: page.getByRole("cell") })).toHaveCount(2);
});

test("without audit access the page says so", async ({ page }) => {
  await fixture(page, { permissions: ["roles.read"] });
  await open(page);
  await expect(page.getByText("You need Read audit events permission for an organization or environment.")).toBeVisible();
});

test("selecting an event loaded with Load more keeps the list and does not search again", async ({ page }) => {
  const { searches } = await fixture(page, { paged: true });
  await open(page);
  await page.getByRole("button", { name: /Load more/ }).click();
  const rows = page.getByRole("row").filter({ has: page.getByRole("cell") });
  await expect(rows).toHaveCount(2);
  const before = searches.length;
  await page.getByRole("button", { name: /^Show details: Updated role/ }).click();
  await expect(page.locator("#audit-row-00000000-0000-7000-8000-00000000e002")).toBeVisible();
  await expect(page.getByRole("button", { name: /^Hide details: Updated role/ })).toBeVisible();
  expect(searches.filter((s) => !s.has("operation_id")).length).toBe(before);
});

test("opening the cause shows it whatever the list was filtered on, and rows stay selectable", async ({ page }) => {
  const { searches } = await fixture(page);
  await open(page, "outcome=succeeded");
  await page.getByRole("button", { name: /^Show details: Updated role/ }).click();
  await page.getByRole("button", { name: "Open the cause" }).click();
  await expect(page.getByText("One linked event")).toBeVisible();
  // The list's searches, not the count of the opened event's operation.
  const list = () => searches.filter((s) => !s.has("operation_id"));
  await expect.poll(() => list().at(-1)?.get("event_id")).toBe("00000000-0000-7000-8000-00000000e001");
  expect(list().at(-1)?.get("outcome")).toBeNull();
  await expect(page.getByRole("button", { name: /^Hide details: Refused/ })).toBeFocused();
  await page.getByRole("button", { name: "Show recent events" }).click();
  await expect(page.getByRole("button", { name: /details: / })).toHaveCount(2);
  await page.getByRole("button", { name: /^Show details: Updated role/ }).click();
  await expect(page.getByRole("button", { name: /^Show details: / })).toHaveCount(1);
});
