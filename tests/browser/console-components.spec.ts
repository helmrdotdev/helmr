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
const inviteURL = "http://127.0.0.1/invite?token=synthetic-invite-token";
const cities = ["Amsterdam", "Berlin", "Chicago", "Dublin", "Frankfurt", "London", "Madrid", "Oslo", "Paris", "Seoul", "Sydney", "Tokyo"];

type Fixture = {
  revocations: number;
  apiKeyRequests: unknown[];
  sessionCursors: (string | null)[];
  secretWrites: { path: string; body: Record<string, unknown> }[];
  deletions: string[];
  invitations: unknown[];
  // Fail every Sessions request, or only the request for the second page.
  failSessions: boolean;
  failNextSessions: boolean;
};

async function fixture(page: Page): Promise<Fixture> {
  const state: Fixture = { revocations: 0, apiKeyRequests: [], sessionCursors: [], secretWrites: [], deletions: [], invitations: [], failSessions: false, failNextSessions: false };
  const session = (index: number) => ({ id: `00000000-0000-7000-8000-00000000060${index}`, agent: "assistant", template: "browser", deployment_id: deploymentID, computer_id: null, status: "open", holds: [], created_at: now });
  const environment = { environment: environmentID, project: projectID, name: "Production", color_hex: "#008000", is_default: true, created_at: now, updated_at: now };
  const project = { project: projectID, name: "Test", default_region_id: "amsterdam", is_default: true, environments: [environment], created_at: now, updated_at: now };
  const deployment = { id: deploymentID, version: "test-version", bundle_digest: `sha256:${"b".repeat(64)}`, created_at: now };
  await page.route("**/api/**", async route => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    const method = route.request().method();
    let json: unknown;
    if (path === "/api/me") json = { user_id: "local", display_name: "Local", org_id: "org", role: "owner", permissions: ["projects.manage", "secrets.read", "secrets.write", "secrets.revoke", "deployments.read"], admin: false, organization_required: false, project_required: false };
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
      json = { id: "key-1", name: "Deploy bot", key_prefix: "hk_test", project: projectID, environment: environmentID, permissions: [], status: "active", created_at: now, last_used_at: null, expires_at: null, revoked_at: null, raw_key: rawKey };
    }
    else if (method === "DELETE") { state.deletions.push(path); await route.fulfill({ json: {} }); return; }
    else if (path === `${base}/api-keys`) json = { api_keys: [{ id: "key-1", name: "Deploy bot", key_prefix: "hk_test", project: projectID, environment: environmentID, permissions: [{ scopes: ["sessions:read"] }], status: "active", created_at: now, last_used_at: null, expires_at: null, revoked_at: null }] };
    else if (path === "/api/members") json = { members: [
      { user_id: "local", display_name: "Local", email: "local@example.test", role: "owner", status: "active", created_at: now, updated_at: now },
      { user_id: "member-2", display_name: "Second Member", email: "second@example.test", role: "developer", status: "active", created_at: now, updated_at: now },
    ] };
    else if (path === "/api/invitations" && method === "POST") {
      const body = route.request().postDataJSON();
      state.invitations.push(body);
      json = { id: "inv-2", email: body.email, role: body.role, status: "pending", created_at: now, expires_at: now, invite_url: inviteURL };
    }
    else if (path === "/api/invitations") json = { invitations: [{ id: "inv-1", email: "pending@example.test", role: "viewer", status: "pending", created_at: now, expires_at: now }] };
    else if (path === `${base}/sessions`) {
      const cursor = url.searchParams.get("cursor");
      state.sessionCursors.push(cursor);
      if (state.failSessions || (cursor === "page-2" && state.failNextSessions)) {
        await route.fulfill({ status: 500, json: { error: { code: "internal_error", message: "Sessions are unavailable." } } });
        return;
      }
      json = cursor === "page-2" ? { sessions: [session(3)] } : { sessions: [session(1), session(2)], next_cursor: "page-2" };
    }
    else if (path === `${base}/deployments/${deploymentID}`) json = deployment;
    else if (path === `${base}/deployments/current`) json = deployment;
    else if (path === `${base}/agents`) json = { deployment_id: deploymentID, agents: [{ agent: "assistant", template: "browser" }] };
    else if (path === `${base}/templates`) json = { deployment_id: deploymentID, templates: [{ template: "browser" }] };
    else if (path.endsWith("/events")) json = { events: [] };
    else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } }); return; }
    await route.fulfill({ json });
  });
  return state;
}

const scope = `project=${projectID}&environment=${environmentID}`;

async function openNewProject(page: Page) {
  await page.goto(`/settings/secrets?${scope}`);
  await page.getByRole("button", { name: "Project Test / Env Production" }).click();
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
  await expect(page.getByRole("button", { name: "Project Test / Env Production" })).toBeFocused();
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
  await page.goto(`/settings/projects?${scope}`);
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
  const menu = page.getByRole("group", { name: `Actions for ${secretID}` });
  const update = menu.getByRole("button", { name: "Update", exact: true });
  const copy = menu.getByRole("button", { name: "Copy ID", exact: true });
  const revoke = menu.getByRole("button", { name: "Revoke", exact: true });

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
  await page.getByRole("button", { name: "Update", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Update secret" });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByLabel("Value", { exact: true })).toBeFocused();
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(trigger).toBeFocused();

  await trigger.click();
  await page.getByRole("button", { name: "Update", exact: true }).click();
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

test("revoking a secret confirms in a dialog that names the secret and its Environment", async ({ page }) => {
  const state = await fixture(page);
  await page.goto(`/settings/secrets?${scope}`);
  const trigger = page.getByRole("button", { name: `Actions for ${secretID}` });
  const dialog = page.getByRole("dialog", { name: "Revoke secret" });

  await trigger.click();
  await page.getByRole("button", { name: "Revoke", exact: true }).click();
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText(secretID);
  await expect(dialog.getByText("Production", { exact: true })).toBeVisible();
  await expect(dialog.getByText("protected", { exact: true })).toBeVisible();
  await expect(dialog).toContainText("Pending inputs are retained");
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(state.revocations).toBe(0);

  await trigger.click();
  await page.getByRole("button", { name: "Revoke", exact: true }).click();
  await dialog.getByRole("button", { name: "Revoke", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(state.revocations).toBe(1);
});

test("a generated API key is revealed once in a dialog with Copy", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  const state = await fixture(page);
  await page.goto(`/settings/api-keys?${scope}`);
  await page.getByRole("button", { name: "Generate new API key", exact: true }).click();
  const form = page.getByRole("dialog", { name: "Generate API key" });
  await form.getByLabel("Name", { exact: true }).fill("Deploy bot");
  const readSessions = form.getByRole("checkbox", { name: "Read sessions", exact: true });
  await expect(readSessions).toBeChecked();
  await expect(readSessions).toHaveAccessibleDescription("Allow automation to read Session state in the selected project and environment.");
  await expect(form.getByText("Allow automation to delete Computers.", { exact: true })).toBeVisible();
  await form.getByRole("checkbox", { name: "Select all Computers" }).check();
  await form.getByRole("button", { name: "Generate", exact: true }).click();

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
    permissions: [{ scopes: ["sessions:create", "sessions:read", "computers:read", "computers:create", "computers:update", "computers:delete", "computer-exec:create"] }],
  }]);
  await reveal.getByRole("button", { name: "Done", exact: true }).click();
  await expect(reveal).toHaveCount(0);
});

test("a table footer shows the row count and loads the next page", async ({ page }) => {
  const state = await fixture(page);
  await page.goto(`/sessions?${scope}`);
  const table = page.getByRole("table");
  await expect(table.getByRole("row")).toHaveCount(3);
  await expect(page.getByText("Showing 2 Sessions", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Load more Sessions", exact: true }).click();
  await expect(table.getByRole("row")).toHaveCount(4);
  await expect(page.getByText("Showing 3 Sessions", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Load more Sessions", exact: true })).toHaveCount(0);
  expect(state.sessionCursors).toContain("page-2");
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
  await page.getByRole("button", { name: "Set secret", exact: true }).click();
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
  await page.getByRole("button", { name: "Update", exact: true }).click();
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
  const notice = page.getByRole("alert").filter({ hasText: "Sessions are unavailable." });
  await expect(notice).toBeVisible();
  await expect(table.getByRole("row")).toHaveCount(3);
  expect(await content!.evaluate(element => element.isConnected)).toBe(true);

  state.failSessions = false;
  await page.clock.fastForward(5_000);
  await expect(notice).toHaveCount(0);
  await expect(table.getByRole("row")).toHaveCount(3);
  expect(await content!.evaluate(element => element.isConnected)).toBe(true);
});

test("a failed Load more is reported in the footer and retries only the next page", async ({ page }) => {
  await page.clock.install();
  const state = await fixture(page);
  state.failNextSessions = true;
  await page.goto(`/sessions?${scope}`);
  const table = page.getByRole("table");
  await expect(table.getByRole("row")).toHaveCount(3);
  await page.getByRole("button", { name: "Load more Sessions", exact: true }).click();
  await expect(page.getByRole("alert")).toHaveText("Could not load more Sessions.");
  await expect(table.getByRole("row")).toHaveCount(3);
  await expect(page.getByText("Sessions are unavailable.")).toHaveCount(0);

  state.failNextSessions = false;
  state.sessionCursors.length = 0;
  await page.getByRole("button", { name: "Retry loading more Sessions", exact: true }).click();
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
    await page.getByRole("button", { name: "Revoke", exact: true }).click();
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

test("removing a member and revoking an invitation each confirm before the request", async ({ page }) => {
  const state = await fixture(page);
  await page.goto(`/settings/members?${scope}`);
  await page.getByRole("button", { name: "Actions for Second Member", exact: true }).click();
  await page.getByRole("button", { name: "Remove", exact: true }).click();
  const remove = page.getByRole("dialog", { name: "Remove member" });
  await expect(remove).toContainText("Second Member");
  await remove.getByRole("button", { name: "Cancel", exact: true }).click();
  expect(state.deletions).toEqual([]);
  await page.getByRole("button", { name: "Actions for Second Member", exact: true }).click();
  await page.getByRole("button", { name: "Remove", exact: true }).click();
  await remove.getByRole("button", { name: "Remove", exact: true }).click();
  await expect(remove).toHaveCount(0);
  expect(state.deletions).toEqual(["/api/members/member-2"]);

  await page.getByRole("button", { name: "Actions for pending@example.test", exact: true }).click();
  await page.getByRole("button", { name: "Revoke", exact: true }).click();
  const revoke = page.getByRole("dialog", { name: "Revoke invitation" });
  await expect(revoke).toContainText("pending@example.test");
  await revoke.getByRole("button", { name: "Cancel", exact: true }).click();
  expect(state.deletions).toEqual(["/api/members/member-2"]);
  await page.getByRole("button", { name: "Actions for pending@example.test", exact: true }).click();
  await page.getByRole("button", { name: "Revoke", exact: true }).click();
  await revoke.getByRole("button", { name: "Revoke", exact: true }).click();
  await expect(revoke).toHaveCount(0);
  expect(state.deletions).toEqual(["/api/members/member-2", "/api/invitations/inv-1"]);
});

test("an invite link is revealed once with Copy", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  const state = await fixture(page);
  await page.goto(`/settings/members?${scope}`);
  await page.getByRole("button", { name: "Invite member", exact: true }).click();
  const form = page.getByRole("dialog", { name: "Invite member" });
  await form.getByLabel("Email", { exact: true }).fill("new@example.test");
  await form.getByRole("button", { name: "Create invite", exact: true }).click();
  const reveal = page.getByRole("dialog", { name: "Invite created" });
  await expect(form).toHaveCount(0);
  await expect(reveal).toContainText(inviteURL);
  await expect(reveal).toContainText("It is not shown again.");
  await reveal.getByRole("button", { name: "Copy", exact: true }).click();
  await expect(reveal.getByRole("button", { name: "Copied", exact: true })).toBeVisible();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(inviteURL);
  expect(state.invitations).toEqual([{ email: "new@example.test", role: "developer" }]);
  await reveal.getByRole("button", { name: "Done", exact: true }).click();
  await expect(reveal).toHaveCount(0);
});
