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

const apiKey = (environmentID: string) => ({ id: `key-${environmentID}`, name: `Key ${environmentID}`, key_prefix: "hk_test", project: projectID, environment: environmentID, permissions: [{ scopes: ["sessions:read"] }], status: "active", created_at: now, last_used_at: null, expires_at: null, revoked_at: null });

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
    else if (path === "/api/me") json = { user_id: "user-1", display_name: "Dana Example", org_id: "org", org_name: "Research Org", role: "owner", permissions: ["projects.manage"], admin: false, organization_required: false, project_required: false };
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
    else if (path === "/api/slack/user-links") json = { links: [] };
    else if (path === "/api/slack/link-workspaces") json = { configured: true, workspaces: [] };
    else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } }); return; }
    await route.fulfill({ json });
  });
  return { requests };
}

const scope = `project=${projectID}&environment=production`;
const scopeTrigger = (page: Page) => page.locator("header").getByRole("button", { name: /^Environment:/ });
const subtitle = (page: Page) => page.locator("h1").locator("xpath=../following-sibling::div[1]");
const rail = (page: Page) => page.getByRole("region", { name: "Environment" });
const radio = (page: Page, name: string) => rail(page).getByRole("radio", { name: new RegExp(`^${name}\\b`) });

test("settings navigation groups pages by Account, Project and Organization", async ({ page }) => {
  await fixture(page);
  await page.goto(`/settings?${scope}`);
  await expect(page).toHaveURL("/settings/project");
  await expect(page.getByRole("heading", { name: "Project", exact: true })).toBeVisible();
  await expect(page.getByText("Region Amsterdam")).toBeVisible();

  const navigation = page.getByRole("navigation", { name: "Settings sections" });
  const groups = navigation.getByRole("group");
  await expect(groups).toHaveCount(3);
  const account = navigation.getByRole("group", { name: "Account", exact: true });
  const projectGroup = navigation.getByRole("group", { name: "Project", exact: true });
  const organization = navigation.getByRole("group", { name: "Organization", exact: true });
  await expect(account.getByRole("link")).toHaveText(["My account"]);
  await expect(projectGroup).not.toContainText("Settings Demo");
  await expect(projectGroup.getByRole("link")).toHaveText(["General", "Environments", "Secrets", "API keys"]);
  await expect(organization).not.toContainText("Research Org");
  await expect(organization.getByRole("link")).toHaveText(["Members"]);
  await expect(projectGroup.getByRole("link", { name: "General", exact: true })).toHaveAttribute("aria-current", "page");

  const settings = page.locator("header").getByRole("link", { name: "Settings" });
  await expect(settings).toHaveAttribute("href", "/settings/project");
  await expect(settings).toHaveAttribute("data-open", "true");

  await page.getByRole("button", { name: "Open profile menu" }).click();
  await expect(page.getByRole("menuitem", { name: "Slack account" })).toHaveCount(0);
  await page.getByRole("menuitem", { name: "Account settings", exact: true }).click();
  await expect(page).toHaveURL("/settings/account");
  await expect(account.getByRole("link", { name: "My account", exact: true })).toHaveAttribute("aria-current", "page");
});

test("My account shows the profile and the Slack identity section", async ({ page }) => {
  await fixture(page);
  await page.goto(`/settings/account?${scope}`);
  await expect(page.getByRole("heading", { name: "My account", exact: true })).toBeVisible();
  const profile = page.locator("section").filter({ has: page.getByRole("heading", { name: "Profile", exact: true }) });
  await expect(profile.locator("dd")).toHaveText(["Dana Example", "Owner", "Research Org"]);
  await expect(page.getByRole("heading", { name: "Your Slack identity" })).toBeVisible();
  await expect(page.getByText("No Slack identities linked.")).toBeVisible();
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
    await expect(page.getByRole("main").getByRole("link", { name: "Overview", exact: true })).toHaveAttribute("href", "/");
    await expect(page.getByRole("main").getByRole("link", { name: "Settings", exact: true })).toHaveAttribute("href", "/settings/project");
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
  await expect(rail(page)).toContainText("Opens on the Environment you are working in. Switching here does not change it.");
  const production = radio(page, "Production");
  const staging = radio(page, "Staging");
  await expect(production).toHaveAccessibleName(/^Production\s*protected\s*working$/);
  await expect(staging).toHaveAccessibleName(/^Staging\s*protected$/);
  await expect(production).toBeChecked();
  await expect(page.getByRole("cell", { name: "PROD_TOKEN", exact: true })).toBeVisible();
  await expect(subtitle(page)).toHaveText("Values are never displayed after saving.");
  await expect(page.getByRole("button", { name: "Set secret in Production", exact: true })).toBeVisible();

  await staging.check();
  await expect(staging).toBeChecked();
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
  await page.getByRole("button", { name: "Update", exact: true }).click();
  const update = page.getByRole("dialog", { name: "Update secret" });
  await expect(update.getByLabel("Secret target environment")).toContainText("Staging");
  await update.getByLabel("Value", { exact: true }).fill("synthetic-rotated");
  await update.getByRole("button", { name: "Save", exact: true }).click();
  await expect(update).toHaveCount(0);

  await page.getByRole("button", { name: "Actions for STAGING_TOKEN" }).click();
  await page.getByRole("button", { name: "Revoke", exact: true }).click();
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
  await expect(staging).toBeChecked();
  await scopeTrigger(page).click();
  await page.getByRole("dialog", { name: "Choose environment" }).getByRole("option", { name: "Production" }).click();
  await expect(scopeTrigger(page)).toContainText("Production");
  await expect(production).toBeChecked();
  await expect(page.getByRole("button", { name: "Set secret in Production", exact: true })).toBeVisible();
  await expect(page.getByRole("cell", { name: "PROD_TOKEN", exact: true })).toBeVisible();
});

test("on narrow screens the Environment selector is a Select above the table", async ({ page }) => {
  await fixture(page);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`/settings/secrets?${scope}`);
  await expect(rail(page)).toBeHidden();
  const select = page.getByRole("combobox", { name: "Environment", exact: true });
  await expect(select).toHaveText("Production · protected · working");
  await select.click();
  await page.getByRole("option", { name: "Staging · protected", exact: true }).click();
  await expect(select).toHaveText("Staging · protected");
  await expect(page.getByRole("cell", { name: "STAGING_TOKEN", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Set secret in Staging", exact: true })).toBeVisible();
  await expect(scopeTrigger(page)).toContainText("Production");
});

test("API keys list, generate and revoke in the selected Environment, not the tab's", async ({ page }) => {
  const state = await fixture(page);
  await page.goto(`/settings/api-keys?${scope}`);
  await expect(page.getByRole("cell", { name: "Key production", exact: true })).toBeVisible();
  await radio(page, "Staging").check();
  await expect(page.getByRole("cell", { name: "Key staging", exact: true })).toBeVisible();
  await expect(scopeTrigger(page)).toContainText("Production");

  await page.getByRole("button", { name: "Generate API key in Staging", exact: true }).click();
  const form = page.getByRole("dialog", { name: "Generate API key" });
  await expect(form.getByLabel("API key target environment")).toContainText("Staging");
  await form.getByLabel("Name", { exact: true }).fill("Deploy bot");
  await form.getByRole("button", { name: "Generate", exact: true }).click();
  const reveal = page.getByRole("dialog", { name: "API key generated" });
  await reveal.getByRole("button", { name: "Done", exact: true }).click();
  await expect(reveal).toHaveCount(0);

  await page.getByRole("button", { name: "Actions for Key staging", exact: true }).click();
  await page.getByRole("button", { name: "Revoke", exact: true }).click();
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
