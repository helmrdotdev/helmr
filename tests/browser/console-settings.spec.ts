import { isAuthenticationPath, authenticationResponse } from "./fixtures/authentication";
import { expect, test, type Page } from "@playwright/test";

// Settings navigation by scope level and the in-page Environment selector of
// Environment-specific settings, against a synthetic API fixture.

const projectID = "settings-demo";
const now = "2026-10-14T00:00:00Z";
const environment = (id: string, name: string, color: string, isDefault: boolean) =>
  ({ environment: id, project: projectID, name, color_hex: color, is_default: isDefault, created_at: now, updated_at: now });
const environments = [environment("production", "Production", "#315FCE", true), environment("staging", "Staging", "#F59E0B", false)];
const project = { project: projectID, name: "Settings Demo", default_region_id: "amsterdam", is_default: true, environments, created_at: now, updated_at: now };
const secretsByEnvironment: Record<string, string> = { production: "PROD_TOKEN", staging: "STAGING_TOKEN" };

const apiKey = (environmentID: string) => ({ id: `key-${environmentID}`, name: `Key ${environmentID}`, key_prefix: "hk_test", project: projectID, environment: environmentID, role_names: ["Session viewer"], status: "active", created_at: now, last_used_at: null, expires_at: null, revoked_at: null });

// Records every Environment-scoped request as `<method> <environment><rest of path>`.
async function fixture(page: Page, options: { signedOut?: boolean } = {}) {
  const requests: string[] = [];
  await page.route("**/api/**", async route => {
    const path = new URL(route.request().url()).pathname;
    const method = route.request().method();
    const scoped = path.match(/^\/api\/projects\/settings-demo\/environments\/([^/]+)(\/.*)$/);
    if (scoped) requests.push(`${method} ${scoped[1]}${scoped[2]}`);
    const secrets = path.match(/^\/api\/projects\/settings-demo\/environments\/([^/]+)\/secrets$/);
    const secretAction = path.match(/^\/api\/projects\/settings-demo\/environments\/[^/]+\/secrets\/([^/]+)\/(rotate|revoke)$/);
    const apiKeys = path.match(/^\/api\/projects\/settings-demo\/environments\/([^/]+)\/api-keys$/);
    let json: unknown;
    if (path === "/api/me" && options.signedOut) { await route.fulfill({ status: 401, json: { error: { code: "unauthorized", message: "sign in" } } }); return; }
    else if (isAuthenticationPath(path)) json = { user_id: "user-1", display_name: "Dana Example", org_id: "org", org_name: "Research Org", is_owner: true, permission_scopes: environments.map(e => ({ project: projectID, environment: e.environment, permissions: ["secrets.read", "secrets.create", "secrets.rotate", "secrets.revoke", "api_keys.read", "api_keys.create", "api_keys.revoke"] })), permissions: ["projects.read", "projects.create", "projects.update", "projects.update_retention", "environments.read", "environments.create", "environments.update"], admin: false, organization_required: false, project_required: false };
    else if (path === "/api/projects") json = { projects: [project] };
    else if (path === `/api/projects/${projectID}`) json = project;
    else if (path === `/api/projects/${projectID}/settings`) json = { history_retention: "30d" };
    else if (path === "/api/regions") json = { regions: [{ id: "amsterdam", display_name: "Amsterdam" }] };
    else if (secrets && method === "POST") json = { secret: route.request().postDataJSON().secret, status: "active", created_at: now };
    else if (secrets) json = { secrets: [{ secret: secretsByEnvironment[secrets[1]!], status: "active", created_at: now }] };
    else if (secretAction) json = { secret: secretAction[1], status: secretAction[2] === "revoke" ? "revoked" : "active", created_at: now };
    else if (apiKeys && method === "POST") json = { ...apiKey(apiKeys[1]!), raw_key: "hk_test_synthetic_raw_key_value" };
    else if (apiKeys) json = { api_keys: [apiKey(apiKeys[1]!)] };
    else if (scoped && method === "DELETE") json = {};
    else if (path === "/api/account/credentials") json = { credentials: [{ id: "cred-1", method: "passkey", label: "Laptop key", created_at: now, last_used_at: now }, { id: "cred-2", method: "email", label: "fixture@example.test", created_at: now }] };
    else if (path === "/api/ownership/members") json = { members: [{ membership_id: "member-1", display_name: "Dana Example", is_owner: true }, { membership_id: "member-2", display_name: "Riley Example", is_owner: false }] };
    else if (path === "/api/slack/user-links") json = { links: [] };
    else if (path === "/api/slack/link-workspaces") json = { configured: true, workspaces: [] };
    else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } }); return; }
    await route.fulfill({ json: authenticationResponse(path, json) });
  });
  return { requests };
}

const scope = `project=${projectID}&environment=production`;
const scopeTrigger = (page: Page) => page.locator("header").getByRole("button", { name: /^Environment:/ });
const subtitle = (page: Page) => page.locator("h1").locator("xpath=../following-sibling::div[1]");
const environmentSelect = (page: Page) => page.getByRole("combobox", { name: "Environment", exact: true });
async function chooseEnvironment(page: Page, name: string) {
  await environmentSelect(page).click();
  await page.getByRole("option", { name: new RegExp(`^${name}\\b`) }).click();
}

test("on a phone settings navigation is one control that opens the grouped pages", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await fixture(page);
  await page.goto(`/settings/secrets?${scope}`);
  const control = page.getByText("Settings · Secrets", { exact: true });
  await expect(control).toBeVisible();
  const sections = page.locator("nav[aria-label='Settings sections']:visible");
  await expect(sections).toHaveCount(0);
  await control.click();
  await expect(sections).toHaveCount(1);
  await sections.getByRole("link", { name: "API keys", exact: true }).click();
  await expect(page).toHaveURL("/settings/api-keys");
  const after = page.getByText("Settings · API keys", { exact: true });
  await expect(after).toBeVisible();
  await expect(sections).toHaveCount(0);
  // Focus returns to the control instead of falling to the page.
  await expect(page.locator("summary")).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(sections).toHaveCount(1);
  await page.keyboard.press("Escape");
  await expect(sections).toHaveCount(0);
  // At the breakpoint itself only the sidebar shows.
  await page.setViewportSize({ width: 960, height: 812 });
  await expect(sections).toHaveCount(1);
  await expect(page.locator("summary")).toBeHidden();
  await page.setViewportSize({ width: 375, height: 812 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});

test("settings navigation groups pages by Account, Project and Organization", async ({ page }) => {
  await fixture(page);
  await page.goto(`/settings?${scope}`);
  await expect(page).toHaveURL("/settings/project");
  await expect(page.getByRole("heading", { name: "General", exact: true })).toBeVisible();
  await expect(page.getByText("Region Amsterdam")).toBeVisible();

  const navigation = page.getByRole("navigation", { name: "Settings sections" });
  const groups = navigation.getByRole("group");
  await expect(groups).toHaveCount(3);
  // Every page reserves the scrollbar's width, so short and long pages line up.
  expect(await page.evaluate(() => getComputedStyle(document.documentElement).scrollbarGutter)).toBe("stable");
  const account = navigation.getByRole("group", { name: "Account", exact: true });
  const projectGroup = navigation.getByRole("group", { name: "Project", exact: true });
  // The organization group names the organization the settings belong to.
  const organization = navigation.getByRole("group", { name: "Organization · Research Org", exact: true });
  await expect(account.getByRole("link")).toHaveText(["Profile", "Sign-in and security"]);
  await expect(projectGroup).not.toContainText("Settings Demo");
  await expect(projectGroup.getByRole("link")).toHaveText(["General", "Environments", "Secrets", "API keys"]);
  await expect(organization.getByRole("link")).toHaveText(["Members", "Groups", "Roles", "Ownership", "Audit events"]);
  await expect(projectGroup.getByRole("link", { name: "General", exact: true })).toHaveAttribute("aria-current", "page");

  const settings = page.locator("header").getByRole("link", { name: "Settings" });
  await expect(settings).toHaveAttribute("href", "/settings/project");
  await expect(settings).toHaveAttribute("data-open", "true");
  await expect(settings).toHaveAttribute("aria-current", "page");

  await page.getByRole("button", { name: "Open profile menu" }).click();
  await expect(page.getByRole("menuitem", { name: "Slack account" })).toHaveCount(0);
  await expect(page.getByRole("menuitem", { name: "Switch organization…", exact: true })).toHaveAttribute("href", "/organizations");
  await page.getByRole("menuitem", { name: "Profile", exact: true }).click();
  await expect(page).toHaveURL("/settings/account");
  await expect(account.getByRole("link", { name: "Profile", exact: true })).toHaveAttribute("aria-current", "page");
});

test("Profile shows the profile, the membership and the Slack identity section", async ({ page }) => {
  await fixture(page);
  await page.goto(`/settings/account?${scope}`);
  await expect(page.getByRole("heading", { name: "Profile", exact: true })).toBeVisible();
  const profile = page.getByRole("region", {name:"Your profile",exact:true});
  await expect(profile.getByLabel("Display name", {exact:true})).toHaveValue("Dana Example");
  await expect(profile).toContainText("fixture@example.test");
  await expect(profile).not.toContainText("Research Org");
  const membership = page.getByRole("region", { name: "Membership", exact: true });
  await expect(membership).toContainText("Owner");
  await expect(membership.getByRole("link", { name: "View my access", exact: true })).toHaveAttribute("href", /\/settings\/members\//);
  // Sign-in methods and ownership live on their own pages.
  await expect(page.getByRole("region", { name: /Sign-in methods|Organization ownership|Confirm your account/ })).toHaveCount(0);
  await expect(page.getByRole("heading", { name: "Your Slack identity" })).toBeVisible();
  await expect(page.getByText("No Slack identities linked.")).toBeVisible();
});

test("Sign-in and security lists sign-in methods and confirms removal and sign-out everywhere", async ({ page }) => {
  await fixture(page);
  await page.goto(`/settings/security?${scope}`);
  await expect(page.getByRole("heading", { name: "Sign-in and security", exact: true })).toBeVisible();
  const table = page.getByRole("table");
  await expect(table.getByRole("row")).toHaveCount(3);
  await expect(table.getByRole("row").nth(1)).toContainText("Laptop key");
  await expect(table.getByRole("row").nth(1)).toContainText("passkey");
  await expect(page.getByText("Showing 2 sign-in methods", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Actions for Laptop key", exact: true }).click();
  await page.getByRole("menuitem", { name: "Remove", exact: true }).click();
  const remove = page.getByRole("dialog", { name: "Remove sign-in method", exact: true });
  await expect(remove).toContainText("Laptop key");
  await remove.getByRole("button", { name: "Cancel", exact: true }).click();
  await page.getByRole("button", { name: "Add passkey", exact: true }).click();
  const add = page.getByRole("dialog", { name: "Add passkey", exact: true });
  await expect(add.getByLabel("Passkey name", { exact: true })).toHaveValue("Passkey 1");
  await add.getByLabel("Passkey name", { exact: true }).fill("");
  await expect(add.getByRole("button", { name: "Add passkey", exact: true })).toBeDisabled();
  await add.getByRole("button", { name: "Cancel", exact: true }).click();
  const danger = page.getByRole("region", { name: "Sign out everywhere", exact: true });
  await danger.getByRole("button", { name: "Sign out everywhere…", exact: true }).click();
  await expect(page.getByRole("dialog", { name: "Sign out everywhere", exact: true })).toBeVisible();
});

test("Ownership lists owners for an owner and confirms each change", async ({ page }) => {
  await fixture(page);
  await page.goto(`/settings/ownership?${scope}`);
  await expect(page.getByRole("heading", { name: "Ownership", exact: true })).toBeVisible();
  const owner = page.getByRole("row").filter({ hasText: "Dana Example" });
  await expect(owner.getByText("owner", { exact: true })).toBeVisible();
  await expect(owner).toContainText("Owner");
  await page.getByRole("button", { name: "Actions for Riley Example", exact: true }).click();
  await page.getByRole("menuitem", { name: "Make owner", exact: true }).click();
  await expect(page.getByRole("dialog", { name: "Make owner", exact: true })).toContainText("Riley Example");
  await page.getByRole("dialog").getByRole("button", { name: "Cancel", exact: true }).click();
  await page.getByRole("button", { name: "Recover my access administration", exact: true }).click();
  await expect(page.getByRole("dialog", { name: "Recover access administration", exact: true })).toBeVisible();
});

const MISSING_PATHS = ["/no-such-page", "/settings/no-such-topic"];

test("a path that does not exist shows Page not found inside the console", async ({ page }) => {
  await fixture(page);
  for (const path of MISSING_PATHS) {
    await page.goto(path);
    await expect(page).toHaveURL(path);
    await expect(page.getByRole("heading", { name: "Page not found", exact: true })).toBeVisible();
    await expect(page.getByRole("navigation", { name: "Sections" })).toBeVisible();
    await expect(page.getByRole("navigation", { name: "Settings sections" })).toHaveCount(0);
    await expect(page.getByRole("main").getByRole("link", { name: "Go to overview", exact: true })).toHaveAttribute("href", "/");
    await expect(page.getByRole("main").getByRole("link", { name: "Sessions", exact: true })).toHaveAttribute("href", "/sessions");
  }
});

test("a signed-out visitor sees Page not found in a window without being sent to sign in", async ({ page }) => {
  await fixture(page, { signedOut: true });
  await page.goto(MISSING_PATHS[0]!);
  await expect(page).toHaveURL(MISSING_PATHS[0]!);
  await expect(page.getByRole("heading", { name: "Page not found", exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: "Go to the console", exact: true })).toHaveAttribute("href", "/");
});

test("the Secrets Environment selector edits another Environment without changing the tab's scope", async ({ page }) => {
  const state = await fixture(page);
  await page.goto(`/settings/secrets?${scope}`);
  await expect(scopeTrigger(page)).toContainText("Production");
  const select = environmentSelect(page);
  await expect(select).toContainText("Production");
  await expect(select.getByText("working", { exact: true })).toBeVisible();
  await expect(page.getByText("You are working in", { exact: false })).toHaveCount(0);
  await expect(page.getByRole("cell", { name: "PROD_TOKEN", exact: true })).toBeVisible();
  await expect(subtitle(page)).toHaveText("Values are never displayed after saving.");
  await expect(page.getByRole("button", { name: "Set secret in Production", exact: true })).toBeVisible();

  await chooseEnvironment(page, "Staging");
  await expect(select).toContainText("Staging");
  await expect(page.getByText("You are working in Production.", { exact: false })).toBeVisible();
  await expect(page.getByRole("cell", { name: "STAGING_TOKEN", exact: true })).toBeVisible();
  await expect(page.getByRole("cell", { name: "PROD_TOKEN", exact: true })).toHaveCount(0);
  await expect(subtitle(page)).toHaveText("Values are never displayed after saving.");
  await expect(page.getByRole("button", { name: "Set secret in Staging", exact: true })).toBeVisible();
  await expect(scopeTrigger(page)).toContainText("Production");
  await expect(scopeTrigger(page)).not.toContainText("Staging");
  await expect(page).toHaveURL("/settings/secrets");

  await page.getByRole("button", { name: "Set secret in Staging", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Set secret" });
  await expect(dialog.getByLabel("Secret target environment")).toContainText("Staging");
  await dialog.getByLabel("Secret ID", { exact: true }).fill("NEW_TOKEN");
  await dialog.getByLabel("Value", { exact: true }).fill("synthetic-value");
  await dialog.getByRole("button", { name: "Save", exact: true }).click();
  await expect(dialog).toHaveCount(0);

  await page.getByRole("button", { name: "Actions for STAGING_TOKEN" }).click();
  await page.getByRole("menuitem", { name: "Update", exact: true }).click();
  const update = page.getByRole("dialog", { name: "Update secret" });
  await expect(update.getByLabel("Secret target environment")).toContainText("Staging");
  await update.getByLabel("Value", { exact: true }).fill("synthetic-rotated");
  await update.getByRole("button", { name: "Save", exact: true }).click();
  await expect(update).toHaveCount(0);

  await page.getByRole("button", { name: "Actions for STAGING_TOKEN" }).click();
  await page.getByRole("menuitem", { name: "Revoke", exact: true }).click();
  const revoke = page.getByRole("dialog", { name: "Revoke secret" });
  await expect(revoke.getByText("Staging", { exact: true })).toBeVisible();
  await revoke.getByRole("button", { name: "Revoke", exact: true }).click();
  await expect(revoke).toHaveCount(0);
  expect(state.requests.filter(request => !request.startsWith("GET "))).toEqual([
    "POST staging/secrets",
    "POST staging/secrets/STAGING_TOKEN/rotate",
    "POST staging/secrets/STAGING_TOKEN/revoke",
  ]);

  // A change of the tab's scope starts the selector over from the new working Environment.
  await scopeTrigger(page).click();
  await page.getByRole("dialog", { name: "Choose environment" }).getByRole("option", { name: "Staging" }).click();
  await expect(scopeTrigger(page)).toContainText("Staging");
  await expect(select).toContainText("Staging");
  await scopeTrigger(page).click();
  await page.getByRole("dialog", { name: "Choose environment" }).getByRole("option", { name: "Production" }).click();
  await expect(scopeTrigger(page)).toContainText("Production");
  await expect(select).toContainText("Production");
  await expect(page.getByRole("button", { name: "Set secret in Production", exact: true })).toBeVisible();
  await expect(page.getByRole("cell", { name: "PROD_TOKEN", exact: true })).toBeVisible();
});

test("on narrow screens the Environment selector stays a Select above the table", async ({ page }) => {
  await fixture(page);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`/settings/secrets?${scope}`);
  const select = environmentSelect(page);
  // The working tag follows the name of the tab's Environment.
  await expect(select).toContainText("Production");
  await expect(select.getByText("working", { exact: true })).toBeVisible();
  await select.click();
  await expect(page.getByRole("option", { name: /^Production\b/ }).getByText("working", { exact: true })).toBeVisible();
  await page.getByRole("option", { name: /^Staging\b/ }).click();
  await expect(select).toContainText("Staging");
  await expect(select.getByText("working", { exact: true })).toHaveCount(0);
  await expect(page.getByRole("cell", { name: "STAGING_TOKEN", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Set secret in Staging", exact: true })).toBeVisible();
  await expect(scopeTrigger(page)).toContainText("Production");
  await page.getByRole("button", { name: "Back to Production", exact: true }).click();
  await expect(select).toContainText("Production");
  await expect(select).toBeFocused();
  await expect(page.getByRole("cell", { name: "PROD_TOKEN", exact: true })).toBeVisible();
});

test("API keys list, generate and revoke in the selected Environment, not the tab's", async ({ page }) => {
  const state = await fixture(page);
  await page.goto(`/settings/api-keys?${scope}`);
  await expect(page.getByRole("cell", { name: "Key production", exact: true })).toBeVisible();
  await chooseEnvironment(page, "Staging");
  await expect(page.getByRole("cell", { name: "Key staging", exact: true })).toBeVisible();
  await expect(scopeTrigger(page)).toContainText("Production");

  await page.getByRole("button", { name: "Generate API key in Staging", exact: true }).click();
  const form = page.getByRole("dialog", { name: "Generate API key" });
  await expect(form.getByLabel("API key target environment")).toContainText("Staging");
  await form.getByLabel("Name", { exact: true }).fill("Deploy bot");
  await form.getByRole("button", { name: "Generate key", exact: true }).click();
  const reveal = page.getByRole("dialog", { name: "API key generated" });
  await reveal.getByRole("button", { name: "Done", exact: true }).click();
  await expect(reveal).toHaveCount(0);

  await page.getByRole("button", { name: "Actions for Key staging", exact: true }).click();
  await page.getByRole("menuitem", { name: "Revoke", exact: true }).click();
  const revoke = page.getByRole("dialog", { name: "Revoke API key" });
  await expect(revoke.getByText("Staging", { exact: true })).toBeVisible();
  await revoke.getByRole("button", { name: "Revoke", exact: true }).click();
  await expect(revoke).toHaveCount(0);

  expect(state.requests).toContain("GET staging/api-keys");
  expect(state.requests.filter(request => !request.startsWith("GET "))).toEqual([
    "POST staging/api-keys",
    "DELETE staging/api-keys/key-staging",
  ]);
});
