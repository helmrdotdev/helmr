import { authenticationResponse, isAuthenticationPath } from "./fixtures/authentication";
import { expect, test, type Page } from "@playwright/test";

const project = "console-demo";
const environment = "production";
const deployment = "00000000-0000-7000-8000-000000000501";
const historical = "00000000-0000-7000-8000-000000000502";
const base = `/api/projects/${project}/environments/${environment}`;
const scope = `project=${project}&environment=${environment}`;
const now = "2026-10-12T00:00:00Z";

async function catalog(page: Page, options: { current?: boolean; defined?: boolean; manageConnection?: boolean; connectionPermissions?: string[] } = {}) {
  const current = options.current ?? true;
  const defined = options.defined ?? true;
  const record = { agent: "support", current_deployment_id: current ? deployment : null, defined_in_current: defined && current };
  const paths: string[] = [];
  await page.route("**/api/**", async route => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    paths.push(path + url.search);
    let json: unknown;
    const env = { environment: environment, project: project, name: "Development", is_default: true, created_at: now, updated_at: now };
    const proj = { project: project, name: "Test", is_default: true, environments: [env], created_at: now, updated_at: now };
    if (isAuthenticationPath(path)) json = { user_id: "user", display_name: "User", org_id: "org", is_owner: false, permissions: [], permission_scopes: [{ project, environment, permissions: ["sessions.read", "agents.read", "sessions.create", "deployments.read", "templates.read", "computers.read", ...(options.connectionPermissions ?? (options.manageConnection === false ? [] : ["agent_connections.read", "agent_connections.configure"]))] }], organization_required: false, project_required: false };
    else if (path === "/api/projects") json = { projects: [proj] };
    else if (path === `/api/projects/${project}`) json = proj;
    else if (path === base) json = env;
    else if (path === `${base}/deployments/current` && !current) { await route.fulfill({ status: 404, json: { error: { code: "no_current_deployment", message: "No current deployment" } } }); return; }
    else if (path.startsWith(`${base}/deployments/`)) json = { id: path.endsWith("current") ? deployment : path.split("/").at(-1), version: "version", bundle_digest: `sha256:${"a".repeat(64)}`, created_at: now };
    else if (path === `${base}/agents` && !current && url.searchParams.get("view") !== "all" && !url.searchParams.has("deployment_id")) { await route.fulfill({ status: 404, json: { error: { code: "no_current_deployment", message: "No current deployment" } } }); return; }
    else if (path === `${base}/agents`) json = url.searchParams.get("view") === "all" ? { agents: [record] } : { deployment_id: url.searchParams.get("deployment_id") ?? deployment, agents: defined ? [{ agent: "support", template: "browser" }] : [] };
    else if (path === `${base}/agents/support/status`) json = record;
    else if (path === `${base}/agents/support`) json = { agent: "support", template: "browser", deployment_id: url.searchParams.get("deployment_id") ?? deployment };
    else if (path === `${base}/templates`) json = { deployment_id: url.searchParams.get("deployment_id") ?? deployment, templates: [{ template: "browser", resources: { milliCpu: 1000, memoryMiB: 512 } }] };
    else if (path === `${base}/agents/support/slack`) json = { connection: null };
    else if (path === `${base}/triggers`) json = url.searchParams.has("cursor") ? { triggers: [] } : { triggers: [{ id: historical, agent: "support", deployment_id: historical, trigger_key: "daily", cron: { pattern: "0 9 * * *", timezone: "UTC" }, input: [], active_from: now, active_until: now }], next_cursor: "older" };
    else if (path === `${base}/sessions`) json = { sessions: [] };
    else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } }); return; }
    await route.fulfill({ json: authenticationResponse(path,json) });
  });
  return paths;
}

test("current Agent navigation opens the Agent with its Template and its Sessions, triggers and Slack tabs", async ({ page }) => {
  const paths = await catalog(page);
  await page.goto(`/agents?${scope}`);
  await expect(page.getByRole("group", { name: "Show agents" }).getByRole("button", { name: "Current deployment" })).toHaveAttribute("aria-pressed", "true");
  const row = page.getByRole("row").filter({ hasText: "support" });
  // The filter already says it; rows carry no "In current deployment" badge.
  await expect(row.getByText("In current deployment", { exact: true })).toHaveCount(0);
  await expect(row.getByText("browser", { exact: true })).toBeVisible();
  await expect(row.getByRole("link", { name: "browser" })).toHaveCount(0);
  await page.getByRole("link", { name: "support", exact: true }).click();
  await expect(page).toHaveURL("/agents/support");

  const breadcrumb = page.getByRole("navigation", { name: "Breadcrumb" });
  await expect(breadcrumb.getByRole("link")).toHaveText(["Agents"]);
  // Each separator ends the crumb before it, so no wrapped line starts with "/".
  await expect(breadcrumb.getByRole("listitem")).toHaveText(["Agents/", "support"]);
  await expect(breadcrumb.locator("[aria-current=page]")).toHaveText("support");
  await expect(page.getByRole("heading", { name: "support", exact: true })).toBeVisible();
  await expect(page.getByText("In current deployment", { exact: true })).toBeVisible();
  await expect(page.getByText("Defined in deployment", { exact: false })).toBeVisible();
  await expect(page.getByText("Template browser", { exact: true })).toBeVisible();
  expect(paths.some(path => path.endsWith(`/agents/support?deployment_id=${deployment}`))).toBe(true);
  await page.getByRole("button", { name: "Agent actions", exact: true }).click();
  await expect(page.getByRole("menuitem", { name: "Copy link", exact: true })).toBeVisible();
  await page.keyboard.press("Escape");
  const tabs = page.getByRole("tablist", { name: "Agent" });
  await expect(tabs.getByRole("tab")).toHaveText(["Sessions", "Triggers", "Slack"]);
  await expect(tabs.getByRole("tab", { name: "Sessions", exact: true })).toHaveAttribute("aria-selected", "true");
  await expect(page.getByRole("tabpanel", { name: "Sessions" })).toContainText("This agent has no sessions yet.");
  await expect.poll(() => paths.some(path => path.includes("/sessions?agent=support"))).toBe(true);
  expect(paths.some(path => path.includes("agent_id="))).toBe(false);

  await tabs.getByRole("tab", { name: "Sessions", exact: true }).focus();
  await page.keyboard.press("ArrowRight");
  await expect(page).toHaveURL("/agents/support/triggers");
  await expect(tabs.getByRole("tab", { name: "Triggers", exact: true })).toBeFocused();
  await expect(page.getByText("daily", { exact: true })).toBeVisible();
  await expect(page.getByText("Showing 1 trigger", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Load more triggers" }).click();
  await expect(page.getByRole("button", { name: "Load more triggers" })).toHaveCount(0);
  expect(paths.some(path => path.endsWith("/triggers?agent=support"))).toBe(true);
  expect(paths.some(path => path.endsWith("/triggers?cursor=older&agent=support"))).toBe(true);

  await tabs.getByRole("tab", { name: "Slack", exact: true }).click();
  await expect(page).toHaveURL("/agents/support/slack");
  await expect(page.getByRole("button", { name: "Connect Slack", exact: true })).toBeVisible();
  await expect(page.getByRole("dialog")).toHaveCount(0);

  await page.goto(`/agents/support/slack/?${scope}`);
  await expect(tabs.getByRole("tab", { name: "Slack", exact: true })).toHaveAttribute("aria-selected", "true");
});

test("the Agents list filters loaded Agents by name", async ({ page }) => {
  await catalog(page);
  await page.goto(`/agents?${scope}`);
  const filter = page.getByRole("searchbox", { name: "Filter agents" });
  await filter.fill("supp");
  await expect(page.getByRole("link", { name: "support", exact: true })).toBeVisible();
  await filter.fill("billing");
  await expect(page.getByText("No loaded agents match this filter.", { exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: "support", exact: true })).toHaveCount(0);
  await filter.fill("");
  await expect(page.getByRole("link", { name: "support", exact: true })).toBeVisible();
});

test("the environment Triggers page lists every Agent's activations", async ({ page }) => {
  const paths = await catalog(page);
  await page.goto(`/triggers?${scope}`);
  await expect(page.getByRole("tab", { name: "Triggers", exact: true })).toHaveAttribute("aria-selected", "true");
  const row = page.getByRole("row").filter({ hasText: "daily" });
  await expect(row.getByRole("link", { name: "support", exact: true })).toHaveAttribute("href", "/agents/support/triggers");
  await expect.poll(() => paths.some(path => path.endsWith("/triggers"))).toBe(true);
  expect(paths.filter(path => path.includes("/triggers")).every(path => !path.includes("agent="))).toBe(true);
});

for (const current of [true, false]) {
  test(`retained Agent remains manageable with ${current ? "no current definition" : "no current Deployment"}`, async ({ page }, testInfo) => {
    await catalog(page, { current, defined: false });
    await page.goto(`/agents?${scope}`);
    await expect(page.getByText(current ? "No agents in the current deployment." : "No current deployment.", { exact: true })).toBeVisible();
    await page.getByRole("group", { name: "Show agents" }).getByRole("button", { name: "All agents" }).click();
    await expect(page.getByRole("row").filter({ hasText: "support" }).getByText("Not in current deployment", { exact: true })).toBeVisible();
    await page.getByRole("link", { name: "support", exact: true }).click();
    await expect(page.getByText("New independent Sessions cannot start.", { exact: false })).toBeVisible();
    await expect(page.getByText(/^Template /)).toHaveCount(0);
    await page.getByRole("tab", { name: "Slack", exact: true }).click();
    await expect(page.getByRole("button", { name: "Connect Slack", exact: true })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("retained-agent.png"), fullPage: true });
  });
}

test("viewer sees who manages an Agent's Slack connection without reading it", async ({ page }) => {
  const paths = await catalog(page, { current: false, defined: false, manageConnection: false });
  await page.goto(`/agents/support/slack?${scope}`);
  await expect(page.getByRole("heading", { name: "support", exact: true })).toBeVisible();
  await expect(page.getByText("Members with Agent connection permissions manage this connection.", { exact: false })).toBeVisible();
  await expect(page.getByRole("button", { name: /Connect Slack|Disconnect|Discard setup|Reauthorize/ })).toHaveCount(0);
  expect(paths.some(path => path.endsWith("/slack"))).toBe(false);
});

test("Deployment details are read-only and show every definition field as columns", async ({ page }, testInfo) => {
  const paths = await catalog(page);
  await page.goto(`/deployments/${historical}?${scope}`);
  const agents = page.getByRole("tabpanel", { name: "Agents" });
  await expect(agents.getByRole("columnheader")).toHaveText(["Agent", "Template"]);
  await expect(agents.getByRole("row").filter({ hasText: "support" })).toContainText("browser");
  // The Agent name is the row's primary link, to the Agent's page; nothing else links.
  await expect(agents.getByRole("link")).toHaveCount(1);
  await expect(agents.getByRole("link", { name: "support", exact: true })).toHaveAttribute("href", "/agents/support");
  await expect(agents.getByText("Showing 1 agent", { exact: true })).toBeVisible();
  await page.getByRole("tab", { name: "Templates", exact: true }).click();
  const templates = page.getByRole("tabpanel", { name: "Templates" });
  await expect(templates.getByRole("columnheader")).toHaveText(["Template", "Resources"]);
  await expect(templates.getByRole("row").filter({ hasText: "browser" })).toContainText("1 vCPU · 512 MiB RAM · Default disk");
  await expect(templates.getByRole("link")).toHaveCount(0);
  expect(paths.some(path => path.endsWith(`/agents?deployment_id=${historical}&limit=100`))).toBe(true);
  expect(paths.some(path => path.endsWith(`/templates?deployment_id=${historical}&limit=100`))).toBe(true);
  await expect(page.getByRole("tab", { name: "Events", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: /Promote|Slack connection|Triggers/ })).toHaveCount(0);
  expect(paths.some(path => path.includes("/triggers"))).toBe(false);
  await page.screenshot({ path: testInfo.outputPath("deployment-read-only.png"), fullPage: true });
});

test("the Templates page lists the current Deployment's Templates without links", async ({ page }) => {
  const paths = await catalog(page);
  await page.goto(`/templates?${scope}`);
  const row = page.getByRole("row").filter({ hasText: "browser" });
  await expect(row).toContainText("1 vCPU · 512 MiB RAM · Default disk");
  await expect(row.getByRole("link")).toHaveCount(0);
  await expect(page.getByRole("navigation", { name: "Sections" }).locator("[aria-current=page]")).toHaveText(["Computers"]);
  expect(paths.some(path => path.endsWith(`/templates?limit=100`))).toBe(true);
});

test("trigger history separates ended activations and shows occurrence outcomes", async ({ page }, testInfo) => {
  await catalog(page);
  await page.route("**/triggers**", async route => {
    const url = new URL(route.request().url());
    if (!url.pathname.startsWith(base)) return route.fallback();
    if (url.pathname.endsWith("/occurrences")) {
      await route.fulfill({ json: { occurrences: [{ scheduled_at: now, evaluated_at: now, disposition: "missed", reason: "agent_deleted", session_id: null, turn_id: null }] } });
      return;
    }
    await route.fulfill({ json: { triggers: url.searchParams.get("view") === "all" ? [{ id: historical, agent: "support", deployment_id: historical, trigger_key: "daily", cron: { pattern: "0 9 * * *", timezone: "UTC" }, input: [], active_from: now, active_until: now }] : [] } });
  });
  await page.goto(`/triggers?${scope}`);
  await expect(page.getByText("No trigger activations in this environment.", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "All history", exact: true }).click();
  await page.getByRole("searchbox", { name: "Trigger key", exact: true }).fill("daily");
  await page.getByRole("button", { name: "daily", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Trigger execution history" });
  await expect(dialog).toContainText("missed");
  await expect(dialog).toContainText("agent_deleted");
  await page.screenshot({ path: testInfo.outputPath("trigger-occurrences.png"), fullPage: true });
});

for (const configure of [false, true]) {
  test(`Slack configuration actions require configure, independently of read and delete (${configure})`, async ({ page }) => {
    await catalog(page, { connectionPermissions: ["agent_connections.read", ...(configure ? ["agent_connections.configure"] : [])] });
    let connection: Record<string, unknown> | null = null;
    await page.route(`**${base}/agents/support/slack`, route => route.fulfill({ json: { transport: "http", connection } }));
    await page.goto(`/agents/support/slack?${scope}`);
    const panel = page.getByRole("tabpanel", { name: "Slack" });
    await expect(panel.getByRole("button", { name: "Connect Slack", exact: true })).toBeEnabled({ enabled: configure });

    connection = { id: "connection", registration_id: "registration", installation_id: null, status: "setup_incomplete", transport: "http", credentials_configured: false };
    await page.reload();
    await expect(panel.getByLabel("Client ID", { exact: true })).toBeEnabled({ enabled: configure });
    await expect(page.getByRole("button", { name: "Discard setup", exact: true })).toBeDisabled();

    connection = { ...connection, credentials_configured: true, client_id: "client" };
    await page.reload();
    await expect(panel.getByRole("button", { name: "Install app in Slack", exact: true })).toBeEnabled({ enabled: configure });

    connection = { ...connection, installation_id: "installation", status: "reauthorization_required", direct_messages_authorized: true };
    await page.reload();
    await expect(panel.getByRole("button", { name: "Reauthorize in Slack", exact: true })).toBeEnabled({ enabled: configure });
    await expect(page.getByRole("button", { name: "Disconnect", exact: true })).toBeDisabled();
  });
}
