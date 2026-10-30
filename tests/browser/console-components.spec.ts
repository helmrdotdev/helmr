import { isAuthenticationPath, authenticationResponse } from "./fixtures/authentication";
import { expect, test, type Page } from "@playwright/test";

// Keyboard and focus behavior of the shared Console parts (popovers, menus,
// selects, dialogs and tabs) against a synthetic API fixture.

const projectID = "console-demo";
const environmentID = "production";
const deploymentID = "00000000-0000-7000-8000-000000000501";
const secretID = "SYNTHETIC_TOKEN";
const base = `/api/projects/${projectID}/environments/${environmentID}`;
const now = "2026-10-14T00:00:00Z";
const rawKey = "hk_test_synthetic_raw_key_value";
const cities = ["Amsterdam", "Berlin", "Chicago", "Dublin", "Frankfurt", "London", "Madrid", "Oslo", "Paris", "Seoul", "Sydney", "Tokyo"];

type Fixture = {
  revocations: number;
  apiKeyRequests: unknown[];
  sessionCursors: (string | null)[];
  secretWrites: { path: string; body: Record<string, unknown> }[];
  deletions: string[];
  invitations: { emails: string[] }[];
  // Fail every Sessions request, or only the request for the second page.
  failSessions: boolean;
  failNextSessions: boolean;
  // While set, Sessions requests wait for it.
  holdSessions: Promise<void> | null;
};

async function fixture(page: Page, accessAdmin = false): Promise<Fixture> {
  const state: Fixture = { revocations: 0, apiKeyRequests: [], sessionCursors: [], secretWrites: [], deletions: [], invitations: [], failSessions: false, failNextSessions: false, holdSessions: null };
  const session = (index: number) => ({ id: `00000000-0000-7000-8000-00000000060${index}`, agent: "assistant", template: "browser", deployment_id: deploymentID, computer_id: null, status: "open", holds: [], created_at: now });
  const environment = { environment: environmentID, project: projectID, name: "Production", color_hex: "#008000", is_default: true, created_at: now, updated_at: now };
  const project = { project: projectID, name: "Test", default_region_id: "amsterdam", is_default: true, environments: [environment], created_at: now, updated_at: now };
  const deployment = { id: deploymentID, version: "test-version", bundle_digest: `sha256:${"b".repeat(64)}`, created_at: now };
  await page.route("**/api/**", async route => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    const method = route.request().method();
    let json: unknown;
    if (isAuthenticationPath(path)) json = { user_id: "local", display_name: "Local", org_id: "org", membership_id: "membership-local", is_owner: true, permissions: [...(accessAdmin ? ["members.read", "members.remove", "invitations.read", "invitations.create", "invitations.revoke", "groups.read", "groups.create", "groups.update", "groups.delete", "groups.add_member", "groups.remove_member", "roles.read", "roles.create", "roles.update", "roles.delete", "assignments.read", "assignments.create", "assignments.update", "assignments.delete"] : []), "projects.read", "projects.create", "projects.update", "environments.read", "environments.create", "environments.update"], permission_scopes: [{ project: projectID, environment: environmentID, permissions: ["api_keys.read", "api_keys.create", "api_keys.update", "api_keys.rotate", "api_keys.revoke", "secrets.read", "secrets.create", "secrets.rotate", "secrets.revoke", "deployments.read"] }], admin: false, organization_required: false, project_required: false, email_delivery: true };
    else if (path === "/api/groups") json = { groups: [] };
    else if (path === "/api/role-grants") json = { grants: [] };
    else if (path === "/api/roles") json = { roles: [{id:"viewer",name:"Session viewer",permissions:["sessions.read"],assignment_levels:["environment"],api_key_assignable:true,revision:7}], catalog:[{id:"sessions.read",name:"View all sessions",resource_type:"sessions",scope_types: ["agent"], assignment_levels: ["environment"],api_key_assignable:true}] };
    else if (path === `/api/projects/${projectID}/access-targets`) json = {project_id:projectID,targets:[{environment_id:environmentID,environment:environmentID,name:"Production",agents:[{id:"agent-1",key:"assistant"}]}]};
    else if (path === "/api/projects") json = { projects: [project] };
    else if (path === `/api/projects/${projectID}`) json = project;
    else if (path === `/api/projects/${projectID}/settings`) json = { history_retention: "30d" };
    else if (path === base) json = environment;
    else if (path === "/api/regions") json = { regions: cities.map(city => ({ id: city.toLowerCase(), display_name: city })) };
    else if (path.startsWith(`${base}/secrets`) && method === "POST" && !path.endsWith("/revoke")) {
      const body = route.request().postDataJSON();
      state.secretWrites.push({ path, body });
      json = { secret: body.secret ?? secretID, status: "active", created_at: now };
    }
    else if (path === `${base}/secrets`) json = { secrets: [{ secret: secretID, status: "active", created_at: now }] };
    else if (path === `${base}/secrets/${secretID}/revoke` && method === "POST") { state.revocations++; json = { secret: secretID, status: "revoked", created_at: now }; }
    else if (path === `${base}/api-keys` && method === "POST") {
      state.apiKeyRequests.push(route.request().postDataJSON());
      json = { id: "key-1", name: "Deploy bot", key_prefix: "hk_test", project: projectID, environment: environmentID, role_names: [], status: "active", created_at: now, last_used_at: null, expires_at: null, revoked_at: null, raw_key: rawKey };
    }
    else if (method === "DELETE") { state.deletions.push(path); await route.fulfill({ json: {} }); return; }
    else if (path === `${base}/api-keys`) json = { api_keys: [{ id: "key-1", name: "Deploy bot", key_prefix: "hk_test", project: projectID, environment: environmentID, role_names: ["Session viewer"], status: "active", created_at: now, last_used_at: null, expires_at: null, revoked_at: null }] };
    else if (path === "/api/members") json = { members: [
      { user_id: "local", display_name: "Local", email: "local@example.test", membership_id: "membership-local", is_owner: true, status: "active", created_at: now, updated_at: now },
      { user_id: "member-2", display_name: "Second Member", email: "second@example.test", membership_id: "membership-2", is_owner: false, status: "active", created_at: now, updated_at: now },
    ] };
    else if (path === "/api/invitations" && method === "POST") {
      const body = route.request().postDataJSON() as Fixture["invitations"][number];
      state.invitations.push(body);
      // second@example.test is already a member; every other address is invited.
      json = { results: body.emails.map(email => email === "second@example.test"
        ? { email, status: "failed", error: { code: "already_member", message: "active member already exists for email" } }
        : { email, status: "invited" }) };
    }
    else if (path === "/api/invitations") {
      const created = state.invitations.flatMap(body => body.emails.filter(email => email !== "second@example.test").map(email => ({ id: `inv-${email}`, email, status: "pending", created_at: now, expires_at: now })));
      json = { invitations: [...created, { id: "inv-1", email: "pending@example.test", status: "pending", created_at: now, expires_at: now }] };
    }
    else if (path === `${base}/sessions`) {
      const cursor = url.searchParams.get("cursor");
      state.sessionCursors.push(cursor);
      await state.holdSessions;
      if (state.failSessions || (cursor === "page-2" && state.failNextSessions)) {
        await route.fulfill({ status: 500, json: { error: { code: "internal_error", message: "Sessions are unavailable." } } });
        return;
      }
      json = cursor === "page-2" ? { sessions: [session(3)] } : { sessions: [session(1), session(2)], next_cursor: "page-2" };
    }
    else if (path === `${base}/deployments/${deploymentID}`) json = deployment;
    else if (path === `${base}/deployments/current`) json = deployment;
    else if (path === `${base}/agents`) json = { deployment_id: deploymentID, agents: [{ agent: "assistant", template: "browser" }] };
    else if (path === `${base}/templates`) json = { deployment_id: deploymentID, templates: [{ template: "browser", resources: { milliCpu: 1000, memoryMiB: 512 } }] };
    else if (path.endsWith("/events")) json = { events: [] };
    else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } }); return; }
    await route.fulfill({ json: authenticationResponse(path, json) });
  });
  return state;
}

const scope = `project=${projectID}&environment=${environmentID}`;

async function openNewProject(page: Page) {
  await page.goto(`/settings/secrets?${scope}`);
  await page.getByRole("button", { name: "Project: Test" }).click();
  await page.getByRole("dialog", { name: "Choose project" }).getByRole("button", { name: "New project", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "New project" });
  await expect(dialog).toBeVisible();
  return dialog;
}

test("Escape closes a Select inside a dialog before the dialog, and the dialog returns focus to its opener", async ({ page }) => {
  await fixture(page);
  const dialog = await openNewProject(page);
  const region = dialog.getByRole("combobox", { name: "Region", exact: true });
  await region.click();
  await expect(page.getByRole("listbox", { name: "Region" })).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("listbox")).toHaveCount(0);
  await expect(dialog).toBeVisible();
  await expect(region).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Project: Test" })).toBeFocused();
});

test("a dialog keeps Tab inside it, and Tab out of a Select returns to its trigger", async ({ page }) => {
  await fixture(page);
  const dialog = await openNewProject(page);
  const name = dialog.getByLabel("Name", { exact: true });
  const close = dialog.getByRole("button", { name: "Close modal", exact: true });
  const cancel = dialog.getByRole("button", { name: "Cancel", exact: true });
  await expect(name).toBeFocused();
  await page.keyboard.press("Shift+Tab");
  await expect(close).toBeFocused();
  await page.keyboard.press("Shift+Tab");
  await expect(cancel).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(close).toBeFocused();

  const region = dialog.getByRole("combobox", { name: "Region", exact: true });
  await region.click();
  await expect(page.getByRole("listbox", { name: "Region" })).toBeVisible();
  await page.keyboard.press("Tab");
  await expect(page.getByRole("listbox")).toHaveCount(0);
  await expect(region).toBeFocused();
  await expect(dialog).toBeVisible();
});

test("a long Select filters its options and picks with the keyboard", async ({ page }) => {
  await fixture(page);
  const dialog = await openNewProject(page);
  const region = dialog.getByRole("combobox", { name: "Region", exact: true });
  await expect(region).toHaveText("Amsterdam");
  await region.click();
  const filter = page.getByRole("searchbox", { name: "Filter options" });
  await expect(filter).toBeFocused();
  await expect(page.getByRole("option")).toHaveCount(cities.length);
  await filter.fill("lon");
  await expect(page.getByRole("option")).toHaveText(["London"]);
  await page.keyboard.press("Enter");
  await expect(page.getByRole("listbox")).toHaveCount(0);
  await expect(region).toHaveText("London");
  await expect(region).toBeFocused();

  await region.press("ArrowDown");
  await expect(filter).toBeFocused();
  await page.keyboard.press("ArrowDown");
  await expect(page.getByRole("option", { name: "Amsterdam", exact: true })).toBeFocused();
  await page.keyboard.press("ArrowDown");
  await expect(page.getByRole("option", { name: "Berlin", exact: true })).toBeFocused();
  await page.keyboard.press("End");
  await expect(page.getByRole("option", { name: "Tokyo", exact: true })).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(region).toHaveText("Tokyo");
});

test("a short Select moves with arrow keys, Home, End and typeahead", async ({ page }) => {
  await fixture(page);
  await page.goto(`/settings/project?${scope}`);
  await page.getByRole("combobox", { name: "History retention", exact: true }).click();
  await page.getByRole("option", { name: "Custom", exact: true }).click();
  const unit = page.getByRole("combobox", { name: "Unit", exact: true });
  await expect(unit).toHaveText("Days");
  await unit.focus();
  await page.keyboard.press("ArrowDown");
  await expect(page.getByRole("option", { name: "Days", exact: true })).toBeFocused();
  await page.keyboard.press("ArrowDown");
  await expect(page.getByRole("option", { name: "Hours", exact: true })).toBeFocused();
  await page.keyboard.press("End");
  await expect(page.getByRole("option", { name: "Seconds", exact: true })).toBeFocused();
  await page.keyboard.press("Home");
  await expect(page.getByRole("option", { name: "Days", exact: true })).toBeFocused();
  await page.keyboard.press("m");
  await expect(page.getByRole("option", { name: "Minutes", exact: true })).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("listbox")).toHaveCount(0);
  await expect(unit).toHaveText("Minutes");
  await expect(unit).toBeFocused();

  await unit.press("s");
  await expect(page.getByRole("option", { name: "Seconds", exact: true })).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(unit).toHaveText("Seconds");
});

test("a row menu moves with arrow keys, Home and End, and Tab or Escape close it onto its trigger", async ({ page }) => {
  await fixture(page);
  await page.goto(`/settings/secrets?${scope}`);
  const trigger = page.getByRole("button", { name: `Actions for ${secretID}` });
  const menu = page.getByRole("menu", { name: `Actions for ${secretID}` });
  const update = menu.getByRole("menuitem", { name: "Update", exact: true });
  const copy = menu.getByRole("menuitem", { name: "Copy ID", exact: true });
  const revoke = menu.getByRole("menuitem", { name: "Revoke", exact: true });

  await trigger.click();
  await expect(update).toBeFocused();
  await page.keyboard.press("ArrowDown");
  await expect(copy).toBeFocused();
  await page.keyboard.press("ArrowDown");
  await expect(revoke).toBeFocused();
  await page.keyboard.press("ArrowDown");
  await expect(update).toBeFocused();
  await page.keyboard.press("ArrowUp");
  await expect(revoke).toBeFocused();
  await page.keyboard.press("Home");
  await expect(update).toBeFocused();
  await page.keyboard.press("End");
  await expect(revoke).toBeFocused();

  await page.keyboard.press("Tab");
  await expect(menu).toHaveCount(0);
  await expect(trigger).toBeFocused();
  await expect(trigger).toHaveAttribute("aria-expanded", "false");

  await trigger.click();
  await expect(update).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(menu).toHaveCount(0);
  await expect(trigger).toBeFocused();
});

test("closing a dialog opened from a row menu returns focus to the menu trigger", async ({ page }) => {
  await fixture(page);
  await page.goto(`/settings/secrets?${scope}`);
  const trigger = page.getByRole("button", { name: `Actions for ${secretID}` });
  await trigger.click();
  await page.getByRole("menuitem", { name: "Update", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Update secret" });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByLabel("Value", { exact: true })).toBeFocused();
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(trigger).toBeFocused();

  await trigger.click();
  await page.getByRole("menuitem", { name: "Update", exact: true }).click();
  await expect(dialog).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(trigger).toBeFocused();
});

test("page tabs move with arrow keys, Home and End and show the selected panel", async ({ page }) => {
  await fixture(page);
  await page.goto(`/deployments/${deploymentID}?${scope}`);
  const tabs = page.getByRole("tablist", { name: "Deployment sections" });
  const agents = tabs.getByRole("tab", { name: "Agents", exact: true });
  const templates = tabs.getByRole("tab", { name: "Templates", exact: true });
  const events = tabs.getByRole("tab", { name: "Events", exact: true });
  await expect(agents).toHaveAttribute("aria-selected", "true");
  await expect(page.getByRole("tabpanel", { name: "Agents" })).toContainText("assistant");

  await agents.focus();
  await page.keyboard.press("ArrowRight");
  await expect(templates).toBeFocused();
  await expect(templates).toHaveAttribute("aria-selected", "true");
  await expect(agents).toHaveAttribute("aria-selected", "false");
  await expect(page.getByRole("tabpanel", { name: "Templates" })).toContainText("browser");
  await page.keyboard.press("End");
  await expect(events).toBeFocused();
  await expect(events).toHaveAttribute("aria-selected", "true");
  await page.keyboard.press("ArrowRight");
  await expect(agents).toBeFocused();
  await page.keyboard.press("ArrowLeft");
  await expect(events).toBeFocused();
  await page.keyboard.press("Home");
  await expect(agents).toBeFocused();
  await expect(agents).toHaveAttribute("aria-selected", "true");
  await expect(tabs.getByRole("tab", { selected: true })).toHaveCount(1);
  await expect(page.getByRole("tabpanel")).toHaveCount(1);
});

test("a generated API key is revealed once in a dialog with Copy", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  const state = await fixture(page);
  await page.goto(`/settings/api-keys?${scope}`);
  await page.getByRole("button", { name: "Generate API key in Production", exact: true }).click();
  const form = page.getByRole("dialog", { name: "Generate API key" });
  await form.getByLabel("Name", { exact: true }).fill("Deploy bot");
  await expect(form).toContainText("access administrator");
  await expect(form.getByRole("checkbox")).toHaveCount(0);
  await page.setViewportSize({ width: 1280, height: 600 });
  await expect(form.getByRole("button", { name: "Generate key", exact: true })).toBeInViewport();
  await form.getByRole("button", { name: "Generate key", exact: true }).click();

  const reveal = page.getByRole("dialog", { name: "API key generated" });
  await expect(form).toHaveCount(0);
  await expect(reveal).toContainText(rawKey);
  await expect(reveal).toContainText("will not be shown again");
  await reveal.getByRole("button", { name: "Copy", exact: true }).click();
  await expect(reveal.getByRole("button", { name: "Copied", exact: true })).toBeVisible();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(rawKey);
  expect(state.apiKeyRequests).toEqual([{
    name: "Deploy bot",
    expires_in_days: null,
  }]);
  await reveal.getByRole("button", { name: "Done", exact: true }).click();
  await expect(reveal).toHaveCount(0);
});

test("a table footer shows the row count and loads the next page", async ({ page }) => {
  const state = await fixture(page);
  await page.goto(`/sessions?${scope}`);
  const table = page.getByRole("table");
  await expect(table.getByRole("row")).toHaveCount(3);
  await expect(page.getByText("Showing 2 sessions", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Load more sessions", exact: true }).click();
  await expect(table.getByRole("row")).toHaveCount(4);
  await expect(page.getByText("Showing 3 sessions", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Load more sessions", exact: true })).toHaveCount(0);
  expect(state.sessionCursors).toContain("page-2");
});

test("a slow table shows its column headers over skeleton rows, then the rows", async ({ page }) => {
  const state = await fixture(page);
  let release!: () => void;
  state.holdSessions = new Promise(resolve => { release = resolve; });
  await page.goto(`/sessions?${scope}`);
  const loading = page.getByRole("status").filter({ hasText: "Loading sessions" });
  await expect(loading).toBeVisible();
  await expect(loading).toHaveAttribute("aria-busy", "true");
  await expect(loading.getByText("Session", { exact: true })).toBeVisible();
  release();
  await expect(loading).toHaveCount(0);
  await expect(page.getByText("Showing 2 sessions", { exact: true })).toBeVisible();
});

test("a mistyped sign-in email is the person's to fix: no technical details", async ({ page }) => {
  await page.route("**/api/auth/email/start", route => route.fulfill({ status: 400, json: { error: { code: "bad_request", message: "invalid email" } } }));
  await page.goto("/login");
  await page.getByLabel("Email").fill("someone@example.test");
  await page.getByRole("button", { name: "Send verification code" }).click();
  await expect(page.getByRole("alert")).toHaveText("Enter a valid email address.");
  await expect(page.getByText("Technical details")).toHaveCount(0);
});

test("a failed sign-in gives a next step and folds the server message into technical details", async ({ page }) => {
  await fixture(page);
  await page.route("**/api/auth/github/finish", route => route.fulfill({ status: 400, json: { error: { code: "invalid_request", message: "state mismatch" } } }));
  await page.goto("/auth/github/callback?code=old&state=old");
  await expect(page).toHaveURL("/auth/github/callback");
  await expect(page.getByRole("heading", { name: "Sign in failed" })).toBeVisible();
  await expect(page.getByRole("alert")).toHaveText("Sign in could not be completed.");
  await expect(page.getByText("If you started from the CLI, run helmr login again in your terminal.")).toBeVisible();
  await expect(page.getByRole("link", { name: "Sign in to the console" })).toHaveAttribute("href", "/login");
  const details = page.locator("details").filter({ hasText: "Technical details" });
  await expect(details.getByText("invalid_request: state mismatch")).toBeHidden();
  await details.getByText("Technical details", { exact: true }).click();
  await expect(details.getByText("invalid_request: state mismatch")).toBeVisible();
});

test("a CLI approval link without its code explains how to start again", async ({ page }) => {
  await fixture(page);
  await page.goto("/auth/device");
  await expect(page.getByRole("heading", { name: "This approval link is incomplete" })).toBeVisible();
  await expect(page.getByText("Run helmr login again in your terminal", { exact: false })).toBeVisible();
  await expect(page.getByRole("link", { name: "Go to the console" })).toHaveAttribute("href", "/");
  await expect(page.getByRole("button", { name: "Approve", exact: true })).toHaveCount(0);
});

test("a multi-line secret value is masked and sent with its line breaks intact", async ({ page }) => {
  const state = await fixture(page);
  const value = "-----BEGIN PRIVATE KEY-----\nline-one\nline-two\n-----END PRIVATE KEY-----\n";
  await page.goto(`/settings/secrets?${scope}`);
  await page.getByRole("button", { name: "Set secret in Production", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Set secret" });
  await dialog.getByLabel("Secret ID", { exact: true }).fill("TLS_KEY");
  const field = dialog.getByLabel("Value", { exact: true });
  await field.fill(value);
  await expect(field).toHaveAttribute("autocomplete", "off");
  await expect(field).toHaveAttribute("spellcheck", "false");
  await expect(field).toHaveCSS("-webkit-text-security", "disc");
  await dialog.getByRole("button", { name: "Save", exact: true }).click();
  await expect(dialog).toHaveCount(0);

  await page.getByRole("button", { name: `Actions for ${secretID}` }).click();
  await page.getByRole("menuitem", { name: "Update", exact: true }).click();
  const update = page.getByRole("dialog", { name: "Update secret" });
  await expect(update.getByLabel("Value", { exact: true })).toHaveValue("");
  await update.getByLabel("Value", { exact: true }).fill(value);
  await update.getByRole("button", { name: "Save", exact: true }).click();
  await expect(update).toHaveCount(0);

  expect(state.secretWrites.map(write => [write.path, write.body.secret, write.body.value])).toEqual([
    [`${base}/secrets`, "TLS_KEY", value],
    [`${base}/secrets/${secretID}/rotate`, undefined, value],
  ]);
});

test("a failed refresh keeps the loaded rows with a notice that clears on recovery", async ({ page }) => {
  await page.clock.install();
  const state = await fixture(page);
  await page.goto(`/sessions?${scope}`);
  const table = page.getByRole("table");
  await expect(table.getByRole("row")).toHaveCount(3);
  // The table itself stays mounted; rows follow the data of each poll.
  const content = await table.elementHandle();

  state.failSessions = true;
  await page.clock.fastForward(5_000);
  const notice = page.getByRole("alert").filter({ hasText: "Could not load sessions." });
  await expect(notice).toBeVisible();
  await expect(table.getByRole("row")).toHaveCount(3);
  expect(await content!.evaluate(element => element.isConnected)).toBe(true);

  state.failSessions = false;
  await page.clock.fastForward(5_000);
  await expect(notice).toHaveCount(0);
  await expect(table.getByRole("row")).toHaveCount(3);
  expect(await content!.evaluate(element => element.isConnected)).toBe(true);
});

test("the Sessions page filters by Agent through the URL", async ({ page }) => {
  await fixture(page);
  const agentsAsked: (string | null)[] = [];
  await page.route(`**${base}/agents?*`, route => route.fulfill({ json: { agents: [{ agent: "assistant", defined_in_current: true }, { agent: "reviewer", defined_in_current: true }] } }));
  await page.route(`**${base}/sessions?*`, route => {
    agentsAsked.push(new URL(route.request().url()).searchParams.get("agent"));
    return route.fulfill({ json: { sessions: [] } });
  });
  await page.goto(`/sessions?agent=ghost&${scope}`);
  await expect(page).toHaveURL("/sessions?agent=ghost");
  const agentFilter = page.getByRole("combobox", { name: "Filter by agent" });
  await expect(agentFilter).toContainText("ghost");
  await expect.poll(() => agentsAsked.at(-1)).toBe("ghost");

  await agentFilter.click();
  await page.getByRole("option", { name: "reviewer", exact: true }).click();
  await expect(page).toHaveURL("/sessions?agent=reviewer");
  await expect.poll(() => agentsAsked.at(-1)).toBe("reviewer");

  await agentFilter.click();
  await page.getByRole("option", { name: "All agents", exact: true }).click();
  await expect(page).toHaveURL("/sessions");
  await expect.poll(() => agentsAsked.at(-1)).toBeNull();
});

test("a failed first load says what failed, offers Retry and folds the server message away", async ({ page }) => {
  const state = await fixture(page);
  state.failSessions = true;
  await page.goto(`/sessions?${scope}`);
  const alert = page.getByRole("alert");
  await expect(alert.getByText("Could not load sessions.", { exact: true })).toBeVisible();
  await expect(page.getByText("Sessions are unavailable.")).toBeHidden();
  await alert.getByText("Technical details", { exact: true }).click();
  await expect(alert.getByText("internal_error: Sessions are unavailable.", { exact: true })).toBeVisible();
  state.failSessions = false;
  await alert.getByRole("button", { name: "Retry", exact: true }).click();
  await expect(page.getByText("Showing 2 sessions", { exact: true })).toBeVisible();
});

test("a denied read without create permission is a neutral notice without Retry", async ({ page }) => {
  await fixture(page);
  await page.route(url => isAuthenticationPath(url.pathname), route => route.fulfill({ json: authenticationResponse(new URL(route.request().url()).pathname, { user_id: "local", org_id: "org", permissions: [], organization_required: false, project_required: false }) }));
  await page.route(`**${base}/secrets`, route => route.fulfill({ status: 403, json: { error: { code: "forbidden", message: "denied" } } }));
  await page.goto(`/settings/secrets?${scope}`);
  await expect(page.getByRole("status").filter({ hasText: "You do not have permission to view secrets." })).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Retry" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^Set secret/ })).toHaveCount(0);
});

test("an unknown computer is not found in the environment, with the way back", async ({ page }) => {
  await fixture(page);
  await page.goto(`/computers/00000000-0000-7000-8000-000000000999?${scope}`);
  await expect(page.getByRole("heading", { level: 1, name: "Computer not found" })).toBeVisible();
  await expect(page.getByText("No computer 000000000999 in Production. It may be in another environment, or the link is wrong.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Copy link" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Retry" })).toHaveCount(0);
  await page.getByRole("link", { name: "Go to computers", exact: true }).click();
  await expect(page).toHaveURL(/\/computers/);
});

test("a failed Load more is reported in the footer and retries only the next page", async ({ page }) => {
  await page.clock.install();
  const state = await fixture(page);
  state.failNextSessions = true;
  await page.goto(`/sessions?${scope}`);
  const table = page.getByRole("table");
  await expect(table.getByRole("row")).toHaveCount(3);
  await page.getByRole("button", { name: "Load more sessions", exact: true }).click();
  await expect(page.getByRole("alert")).toHaveText("Could not load more sessions.");
  await expect(table.getByRole("row")).toHaveCount(3);
  await expect(page.getByText("Sessions are unavailable.")).toHaveCount(0);

  state.failNextSessions = false;
  state.sessionCursors.length = 0;
  await page.getByRole("button", { name: "Retry loading more sessions", exact: true }).click();
  await expect(table.getByRole("row")).toHaveCount(4);
  await expect(page.getByRole("alert")).toHaveCount(0);
  expect(state.sessionCursors).toEqual(["page-2"]);
});

test("revoking an API key confirms with the key and its Environment", async ({ page }) => {
  const state = await fixture(page);
  await page.goto(`/settings/api-keys?${scope}`);
  const dialog = page.getByRole("dialog", { name: "Revoke API key" });
  const open = async () => {
    await page.getByRole("button", { name: "Actions for Deploy bot", exact: true }).click();
    await page.getByRole("menuitem", { name: "Revoke", exact: true }).click();
    await expect(dialog).toContainText("Deploy bot");
    await expect(dialog.getByText("Production", { exact: true })).toBeVisible();
  };
  await open();
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(state.deletions).toEqual([]);
  await open();
  await dialog.getByRole("button", { name: "Revoke", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(state.deletions).toEqual([`${base}/api-keys/key-1`]);
});

test("ownership alone does not expose member-management actions", async ({ page }) => {
  await fixture(page);
  await page.goto(`/settings/members?${scope}`);
  await expect(page.getByText("You need Read members permission to view members.", { exact: true })).toBeVisible();
  await expect(page.getByRole("table")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Invite members", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^Actions for / })).toHaveCount(0);
});

test("members and pending invitations share one table told apart by status", async ({ page }) => {
  await fixture(page, true);
  await page.goto(`/settings/members?${scope}`);
  await expect(page.getByRole("heading", { level: 2 })).toHaveCount(0);
  const table = page.getByRole("table");
  await expect(table).toHaveCount(1);
  await expect(table.getByRole("columnheader")).toHaveText(["Member", "Email", "Organization roles", "Status", "Since", "Actions"]);
  const rows = table.getByRole("row");
  await expect(rows).toHaveCount(4);
  await expect(rows.nth(1)).toContainText("Local");
  await expect(rows.nth(1)).toContainText("you");
  // Owners cannot be removed; ownership changes on its own page, separately from ordinary roles.
  await page.getByRole("button", { name: "Actions for Local", exact: true }).click();
  await expect(page.getByRole("menuitem")).toHaveText(["Manage ownership", "View related work", "Grant access", "Revoke sessions"]);
  await expect(page.getByRole("menuitem", { name: "Manage ownership", exact: true })).toHaveAttribute("href", "/settings/ownership");
  await page.keyboard.press("Escape");
  await expect(rows.nth(1).getByRole("cell").nth(3)).toHaveText("Active");
  await expect(rows.nth(2).getByRole("cell").nth(0)).toHaveText("Second Member");
  await expect(rows.nth(2).getByRole("cell").nth(1)).toHaveText("second@example.test");
  await expect(rows.nth(2).getByRole("cell").nth(3)).toHaveText("Active");
  // The invitation follows the members: its invited email is the member, its
  // status Invited.
  await expect(rows.nth(3).getByRole("cell").nth(0)).toHaveText("pending@example.test");
  await expect(rows.nth(3).getByRole("cell").nth(2)).toHaveText("—");
  await expect(rows.nth(3).getByRole("cell").nth(3)).toHaveText("Invited");
  await expect(page.getByText("Showing 2 members, 1 invitation", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Load more invitations", exact: true })).toHaveCount(0);
});

test("without pending invitations the members footer counts members only", async ({ page }) => {
  await fixture(page, true);
  await page.route("**/api/invitations", route => route.request().method() === "GET" ? route.fulfill({ json: { invitations: [] } }) : route.fallback());
  await page.goto(`/settings/members?${scope}`);
  await expect(page.getByRole("table").getByRole("row")).toHaveCount(3);
  await expect(page.getByText("Showing 2 members", { exact: true })).toBeVisible();
  await expect(page.getByText(/invitation/)).toHaveCount(0);
});

test("the members table footer loads more invitations and counts every row", async ({ page }) => {
  await fixture(page, true);
  const earlier = new Date(Date.parse(now) - 60_000).toISOString();
  await page.route("**/api/invitations**", async route => {
    const url = new URL(route.request().url());
    if (route.request().method() !== "GET" || url.pathname !== "/api/invitations") return route.fallback();
    await route.fulfill({ json: url.searchParams.get("cursor") === "invitations-2"
      ? { invitations: [{ id: "inv-3", email: "earlier@example.test", status: "pending", created_at: earlier, expires_at: earlier }] }
      : { invitations: [{ id: "inv-1", email: "pending@example.test", status: "pending", created_at: now, expires_at: now }], next_cursor: "invitations-2" } });
  });
  await page.goto(`/settings/members?${scope}`);
  await expect(page.getByText("Showing 2 members, 1 invitation", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Load more invitations", exact: true }).click();
  await expect(page.getByText("Showing 2 members, 2 invitations", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Load more invitations", exact: true })).toHaveCount(0);
  const rows = page.getByRole("table").getByRole("row");
  // The server lists invitations newest first; the older page is appended last.
  await expect(rows).toHaveCount(5);
  await expect(rows.nth(3).getByRole("cell").first()).toHaveText("pending@example.test");
  await expect(rows.nth(4).getByRole("cell").first()).toHaveText("earlier@example.test");
  await expect(rows.nth(4).getByRole("cell").nth(3)).toHaveText("Invited");
});

test("invitations that fail to load leave the members and offer Retry", async ({ page }) => {
  await fixture(page, true);
  let fail = true;
  await page.route("**/api/invitations", route => {
    if (route.request().method() !== "GET") return route.fallback();
    if (fail) return route.fulfill({ status: 503, json: { error: { code: "unavailable", message: "Unavailable" } } });
    return route.fulfill({ json: { invitations: [{ id: "inv-1", email: "pending@example.test", status: "pending", created_at: now, expires_at: now }] } });
  });
  await page.goto(`/settings/members?${scope}`);
  const notice = page.getByRole("alert").filter({ hasText: "Could not load invitations." });
  await expect(notice).toBeVisible();
  await expect(page.getByRole("table").getByRole("row")).toHaveCount(3);
  await expect(page.getByText("Showing 2 members", { exact: true })).toBeVisible();
  fail = false;
  await notice.getByRole("button", { name: "Retry", exact: true }).click();
  await expect(notice).toHaveCount(0);
  await expect(page.getByRole("table").getByRole("row")).toHaveCount(4);
  await expect(page.getByText("Showing 2 members, 1 invitation", { exact: true })).toBeVisible();
});

test("removing a member and revoking an invitation each confirm before the request", async ({ page }) => {
  const state = await fixture(page, true);
  await page.goto(`/settings/members?${scope}`);
  await page.getByRole("button", { name: "Actions for Second Member", exact: true }).click();
  await page.getByRole("menuitem", { name: "Remove member", exact: true }).click();
  const remove = page.getByRole("dialog", { name: "Remove member" });
  await expect(remove).toContainText("Second Member");
  await remove.getByRole("button", { name: "Cancel", exact: true }).click();
  expect(state.deletions).toEqual([]);
  await page.getByRole("button", { name: "Actions for Second Member", exact: true }).click();
  await page.getByRole("menuitem", { name: "Remove member", exact: true }).click();
  await remove.getByRole("button", { name: "Remove", exact: true }).click();
  await expect(remove).toHaveCount(0);
  expect(state.deletions).toEqual(["/api/members/membership-2"]);

  await page.getByRole("button", { name: "Actions for pending@example.test", exact: true }).click();
  await page.getByRole("menuitem", { name: "Revoke invitation", exact: true }).click();
  const revoke = page.getByRole("dialog", { name: "Revoke invitation" });
  await expect(revoke).toContainText("pending@example.test");
  await revoke.getByRole("button", { name: "Cancel", exact: true }).click();
  expect(state.deletions).toEqual(["/api/members/membership-2"]);
  await page.getByRole("button", { name: "Actions for pending@example.test", exact: true }).click();
  await page.getByRole("menuitem", { name: "Revoke invitation", exact: true }).click();
  await revoke.getByRole("button", { name: "Revoke", exact: true }).click();
  await expect(revoke).toHaveCount(0);
  expect(state.deletions).toEqual(["/api/members/membership-2", "/api/invitations/inv-1"]);
});

test("the invite form splits pasted addresses and flags invalid and duplicate ones", async ({ page }) => {
  const state = await fixture(page, true);
  await page.goto(`/settings/members?${scope}`);
  await page.getByRole("button", { name: "Invite members", exact: true }).click();
  const form = page.getByRole("dialog", { name: "Invite members" });
  const emails = form.getByLabel("Emails", { exact: true });
  const submit = form.getByRole("button", { name: "Invite", exact: true });
  await expect(submit).toBeDisabled();

  await emails.fill("not-an-address");
  await expect(form.getByText("0 to invite · 1 invalid", { exact: true })).toBeVisible();
  await expect(submit).toBeDisabled();

  await emails.fill("Ada@Example.test, not-an-address\nlin@team.test ada@example.test;grace@team.test");
  await expect(form.getByText("3 to invite · 1 invalid · 1 duplicate", { exact: true })).toBeVisible();
  const recipients = form.getByRole("list", { name: "Recipients" }).getByRole("listitem");
  await expect(recipients).toHaveText(["ada@example.test", "not-an-address· invalid", "lin@team.test", "ada@example.test· duplicate", "grace@team.test"]);
  await expect(submit).toBeEnabled();

  await emails.fill(Array.from({ length: 51 }, (_, index) => `person-${index}@team.test`).join("\n"));
  await expect(form.getByText("Invite at most 50 people at a time.", { exact: true })).toBeVisible();
  await expect(submit).toBeDisabled();
  expect(state.invitations).toEqual([]);
});

test("inviting several members summarizes who was invited and who was not, then refreshes the table", async ({ page }) => {
  const state = await fixture(page, true);
  await page.goto(`/settings/members?${scope}`);
  await expect(page.getByText("Showing 2 members, 1 invitation", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Invite members", exact: true }).click();
  const form = page.getByRole("dialog", { name: "Invite members" });
  await form.getByLabel("Emails", { exact: true }).fill("ada@example.test\nlin@team.test, second@example.test grace@team.test bad-address");
  await form.getByRole("button", { name: "Invite", exact: true }).click();

  const results = page.getByRole("dialog", { name: "Invitations" });
  await expect(form).toHaveCount(0);
  expect(state.invitations).toEqual([{ emails: ["ada@example.test", "lin@team.test", "second@example.test", "grace@team.test"] }]);
  await expect(results.getByText("Invited 3 people.", { exact: true })).toBeVisible();
  await expect(results.getByText("Could not invite 1:", { exact: true })).toBeVisible();
  await expect(results.getByRole("list", { name: "Not invited" }).getByRole("listitem")).toHaveText(["second@example.test · Already a member"]);
  await expect(results.getByRole("table")).toHaveCount(0);
  await expect(results.getByRole("button", { name: /copy/i })).toHaveCount(0);

  // The table behind the dialog already lists the new invitations.
  await expect(page.getByText("Showing 2 members, 4 invitations", { exact: true })).toBeVisible();
  await results.getByRole("button", { name: "Done", exact: true }).click();
  await expect(results).toHaveCount(0);
  await expect(page.getByRole("cell", { name: "grace@team.test" }).first()).toBeVisible();
});

test("when no invitation email can be sent the form stays open with its addresses", async ({ page }) => {
  await fixture(page, true);
  let posts = 0;
  await page.route("**/api/invitations", route => {
    if (route.request().method() !== "POST") return route.fallback();
    posts++;
    return route.fulfill({ json: { results: [
      { email: "ada@team.test", status: "failed", error: { code: "email_failed", message: "Invitation emails could not be sent" } },
      { email: "lin@team.test", status: "failed", error: { code: "email_rejected", message: "The email server refused this address" } },
    ] } });
  });
  await page.goto(`/settings/members?${scope}`);
  await page.getByRole("button", { name: "Invite members", exact: true }).click();
  const form = page.getByRole("dialog", { name: "Invite members" });
  const emails = form.getByLabel("Emails", { exact: true });
  await emails.fill("Ada <ada@team.test>, lin@team.test");
  await expect(form.getByRole("list", { name: "Recipients" }).getByRole("listitem")).toHaveText(["ada@team.test", "lin@team.test"]);
  await form.getByRole("button", { name: "Invite", exact: true }).click();
  await expect(form.getByRole("alert")).toHaveText("Invitation emails could not be sent. Nobody was invited. The email server refused lin@team.test.");
  await expect(emails).toHaveValue("Ada <ada@team.test>, lin@team.test");
  await expect(page.getByRole("dialog", { name: "Invitations" })).toHaveCount(0);
  expect(posts).toBe(1);
  await expect(form.getByRole("button", { name: "Invite", exact: true })).toBeEnabled();
});

test("without email delivery, inviting members is unavailable and says why", async ({ page }) => {
  await fixture(page, true);
  await page.route(url => isAuthenticationPath(url.pathname), route => route.fulfill({ json: authenticationResponse(new URL(route.request().url()).pathname, { user_id: "local", display_name: "Local", org_id: "org", membership_id: "membership-local", is_owner: true, permissions: ["members.read", "members.remove", "invitations.read", "invitations.create", "invitations.revoke", "groups.read", "groups.create", "groups.update", "groups.delete", "groups.add_member", "groups.remove_member", "roles.read", "roles.create", "roles.update", "roles.delete", "assignments.read", "assignments.create", "assignments.update", "assignments.delete"], admin: false, organization_required: false, project_required: false, email_delivery: false }) }));
  await page.goto(`/settings/members?${scope}`);
  await expect(page.getByText("Email delivery is not set up in Helmr, so members can't be invited.", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Invite members", exact: true })).toBeDisabled();
  await expect(page.getByText("Showing 2 members, 1 invitation", { exact: true })).toBeVisible();
});

test("an access administrator must choose a role for a new key", async ({ page }) => {
  const state = await fixture(page, true);
  await page.goto(`/settings/api-keys?${scope}`);
  await page.getByRole("button", { name: "Generate API key in Production", exact: true }).click();
  const form = page.getByRole("dialog", {name:"Generate API key"});
  await form.getByLabel("Name", {exact:true}).fill("Automation");
  await form.getByRole("combobox",{name:"Role",exact:true}).click();
  await expect(page.getByRole("option",{name:/No access/})).toHaveCount(0);
  await page.keyboard.press("Escape");
  await form.getByRole("button",{name:"Generate key",exact:true}).click();
  await expect(form).toContainText("Choose a role and its targets");
  await form.getByRole("combobox",{name:"Role",exact:true}).click();
  await page.getByRole("option",{name:"Session viewer",exact:true}).click();
  await form.getByRole("button",{name:"Generate key",exact:true}).click();
  await expect(form).toContainText("Choose a role and its targets");
  expect(state.apiKeyRequests).toHaveLength(0);
  await form.getByRole("checkbox",{name:"assistant",exact:true}).check();
  await form.getByRole("button",{name:"Generate key",exact:true}).click();
  await expect(page.getByRole("dialog",{name:"API key generated"})).toBeVisible();
  expect(state.apiKeyRequests).toEqual([{name:"Automation",expires_in_days:null,role:{role_id:"viewer",expected_role_revision:7,agents_mode:"selected",agent_ids:["agent-1"]}}]);
});

test("an issuer who can assign but not read roles or assignments creates a key without a role", async ({ page }) => {
  const state = await fixture(page);
  await page.route(url => isAuthenticationPath(url.pathname), route => route.fulfill({ json: authenticationResponse(new URL(route.request().url()).pathname, { user_id: "local", display_name: "Local", org_id: "org", membership_id: "membership-local", is_owner: false, permissions: ["assignments.create", "projects.read", "environments.read"], permission_scopes: [{ project: projectID, environment: environmentID, permissions: ["api_keys.read", "api_keys.create"] }], admin: false, organization_required: false, project_required: false, email_delivery: true }) }));
  await page.goto(`/settings/api-keys?${scope}`);
  await page.getByRole("button", { name: "Generate API key in Production", exact: true }).click();
  const form = page.getByRole("dialog", {name:"Generate API key"});
  await form.getByLabel("Name", {exact:true}).fill("Automation");
  await expect(form.getByRole("combobox",{name:"Role",exact:true})).toHaveCount(0);
  await expect(form).toContainText("This key will have no workload access. An access administrator must grant a role before it can be used.");
  await form.getByRole("button",{name:"Generate key",exact:true}).click();
  await expect(page.getByRole("dialog",{name:"API key generated"})).toBeVisible();
  expect(state.apiKeyRequests).toEqual([{name:"Automation",expires_in_days:null}]);
});

test("secret read permission alone offers no creation action in an empty environment", async ({ page }) => {
  await fixture(page);
  await page.route(url => isAuthenticationPath(url.pathname), route => route.fulfill({ json: authenticationResponse(new URL(route.request().url()).pathname, {
    user_id: "local", org_id: "org", permissions: [], organization_required: false, project_required: false,
    permission_scopes: [{ project: projectID, environment: environmentID, permissions: ["secrets.read"] }],
  }) }));
  await page.route(`**${base}/secrets`, route => route.fulfill({ json: { secrets: [] } }));
  await page.goto(`/settings/secrets?${scope}`);
  await expect(page.getByText("No secrets in Production yet.", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: /^Set secret/ })).toHaveCount(0);
});

test("secret create permission remains usable when reading secrets is denied", async ({ page }) => {
  const state = await fixture(page);
  await page.route(url => isAuthenticationPath(url.pathname), route => route.fulfill({ json: authenticationResponse(new URL(route.request().url()).pathname, {
    user_id: "local", org_id: "org", permissions: [], organization_required: false, project_required: false,
    permission_scopes: [{ project: projectID, environment: environmentID, permissions: ["secrets.create"] }],
  }) }));
  await page.route(`**${base}/secrets`, route => route.request().method() === "GET"
    ? route.fulfill({ status: 403, json: { error: { code: "forbidden", message: "denied" } } }) : route.fallback());
  await page.goto(`/settings/secrets?${scope}`);
  await expect(page.getByRole("status").filter({ hasText: "You do not have permission to view secrets." })).toBeVisible();
  await page.getByRole("button", { name: "Set secret in Production", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Set secret" });
  await dialog.getByLabel("Secret ID", { exact: true }).fill("WRITE_ONLY");
  await dialog.getByLabel("Value", { exact: true }).fill("synthetic-value");
  await dialog.getByRole("button", { name: "Save", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(state.secretWrites).toEqual([{ path: `${base}/secrets`, body: { secret: "WRITE_ONLY", value: "synthetic-value", idempotency_key: expect.any(String) } }]);
});

test("member suspension and session revocation confirm with their own text while resumption runs directly",async({page})=>{
 await fixture(page,true);
 let status="active";const writes:string[]=[];
 await page.route("**/api/members**",async route=>{
 const path=new URL(route.request().url()).pathname;
 if(path==="/api/members"&&route.request().method()==="GET")return route.fulfill({json:{members:[{user_id:"member-2",membership_id:"membership-2",display_name:"Second Member",is_owner:false,status,created_at:now,updated_at:now}]}});
 if(route.request().method()==="POST"&&path.startsWith("/api/members/membership-2/")){writes.push(path);if(path.endsWith("/suspend"))status="suspended";if(path.endsWith("/resume"))status="active";return route.fulfill({status:204});}
 return route.fallback();
 });
 await page.goto(`/settings/members?${scope}`);
 for(const [label,action,text] of [["Suspend member","suspend","cannot use it until resumed"],["Revoke sessions","revoke-sessions","must sign in again"]] as const){
 await page.getByRole("button",{name:"Actions for Second Member",exact:true}).click();await page.getByRole("menuitem",{name:label,exact:true}).click();
 const dialog=page.getByRole("dialog",{name:label,exact:true});await expect(dialog).toContainText(text);await expect(dialog).toContainText("linked Slack identities");expect(writes).not.toContain(`/api/members/membership-2/${action}`);
 await dialog.getByRole("button",{name:label,exact:true}).click();await expect(dialog).toHaveCount(0);
 expect(writes).toContain(`/api/members/membership-2/${action}`);
 if(action==="suspend"){
 await expect(page.getByRole("table").getByRole("row").nth(1)).toContainText("Suspended");
 await page.getByRole("button",{name:"Actions for Second Member",exact:true}).click();await page.getByRole("menuitem",{name:"Resume member",exact:true}).click();
 await expect(page.getByRole("dialog")).toHaveCount(0);
 await expect.poll(()=>writes).toContain("/api/members/membership-2/resume");
 await expect(page.getByRole("table").getByRole("row").nth(1)).toContainText("Active");
 }
 }
});

test("related work exposes an independent command stop action",async({page})=>{
 const state=await fixture(page,true);
 const user="00000000-0000-7000-8000-000000000777";
 const command="00000000-0000-7000-8000-000000000778";
 let cancelled=0;let filtered=false;
 await page.route(`**${base}/sessions*`,route=>{filtered=new URL(route.request().url()).searchParams.get("related_user_id")===user;return route.fulfill({json:{sessions:[{id:"00000000-0000-7000-8000-000000000601",agent:"assistant",template:"browser",deployment_id:deploymentID,computer_id:null,status:"open",holds:[],created_at:now,related_via:"descendant"}]}});});
 await page.route(`**${base}/related-commands*`,route=>route.fulfill({json:{can_cancel:true,commands:cancelled?[]:[{id:command,computer_id:"00000000-0000-7000-8000-000000000779",status:"running",created_at:now}]}}));
 await page.route(`**${base}/commands/${command}/cancel`,route=>{cancelled++;return route.fulfill({status:204});});
 await page.goto(`/sessions?user_id=${user}`);
 await expect(page.getByText("Started by related work",{exact:true})).toBeVisible();
 expect(filtered).toBe(true);
 await page.getByRole("button",{name:"Stop command…",exact:true}).click();
 expect(cancelled).toBe(0);
 await page.getByRole("dialog",{name:"Stop command",exact:true}).getByRole("button",{name:"Stop command",exact:true}).click();
 await expect(page.getByText("No active direct commands in this environment.",{exact:true})).toBeVisible();
 expect(cancelled).toBe(1);expect(state.revocations).toBe(0);
});
