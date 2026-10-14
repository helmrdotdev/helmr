import { expect, test, type Page } from "@playwright/test";

const project = "console-demo";
const environment = "production";
const deployment = "00000000-0000-7000-8000-000000000501";
const historical = "00000000-0000-7000-8000-000000000502";
const base = `/api/projects/${project}/environments/${environment}`;
const scope = `project=${project}&environment=${environment}`;
const now = "2026-10-12T00:00:00Z";

async function catalog(page: Page, options: { current?: boolean; defined?: boolean; role?: string } = {}) {
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
    if (path === "/api/me") json = { user_id: "user", display_name: "User", org_id: "org", role: options.role ?? "owner", permissions: ["sessions.read", "deployments.write"], organization_required: false, project_required: false };
    else if (path === "/api/projects") json = { projects: [proj] };
    else if (path === `/api/projects/${project}`) json = proj;
    else if (path === base) json = env;
    else if (path === `${base}/deployments/current` && !current) { await route.fulfill({ status: 404, json: { error: { code: "no_current_deployment", message: "No current Deployment" } } }); return; }
    else if (path.startsWith(`${base}/deployments/`)) json = { id: path.endsWith("current") ? deployment : path.split("/").at(-1), version: "version", bundle_digest: `sha256:${"a".repeat(64)}`, created_at: now };
    else if (path === `${base}/agents`) json = url.searchParams.get("view") === "all" ? { agents: [record] } : { deployment_id: url.searchParams.get("deployment_id") ?? deployment, agents: defined ? [{ agent: "support", template: "browser" }] : [] };
    else if (path === `${base}/agents/support/status`) json = record;
    else if (path === `${base}/agents/support`) json = { agent: "support", template: "browser", deployment_id: url.searchParams.get("deployment_id") ?? deployment };
    else if (path === `${base}/templates/browser`) json = { template: "browser", resources: { milliCpu: 1000, memoryMiB: 512 }, deployment_id: url.searchParams.get("deployment_id") ?? deployment };
    else if (path === `${base}/agents/support/slack`) json = { connection: null };
    else if (path === `${base}/triggers`) json = url.searchParams.has("cursor") ? { triggers: [] } : { triggers: [{ id: historical, agent: "support", deployment_id: historical, trigger_key: "daily", cron: { pattern: "0 9 * * *", timezone: "UTC" }, input: [], active_from: now, active_until: now }], next_cursor: "older" };
    else if (path === `${base}/sessions`) json = { sessions: [] };
    else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } }); return; }
    await route.fulfill({ json });
  });
  return paths;
}

test("current Agent navigation opens the Agent with its Sessions, triggers, Slack and definition tabs", async ({ page }) => {
  const paths = await catalog(page);
  await page.goto(`/agents?${scope}`);
  await expect(page.getByRole("group", { name: "Show Agents" }).getByRole("button", { name: "Current Deployment" })).toHaveAttribute("aria-pressed", "true");
  const row = page.getByRole("row").filter({ hasText: "support" });
  await expect(row.getByText("In current Deployment", { exact: true })).toBeVisible();
  await expect(row.getByRole("link", { name: "browser", exact: true })).toHaveAttribute("href", "/templates/browser");
  await page.getByRole("link", { name: "support", exact: true }).click();
  await expect(page).toHaveURL("/agents/support");

  await expect(page.getByRole("navigation", { name: "Breadcrumb" }).getByRole("link")).toHaveText(["Agents"]);
  await expect(page.getByRole("heading", { name: "support", exact: true })).toBeVisible();
  await expect(page.getByText("In current Deployment", { exact: true })).toBeVisible();
  await expect(page.getByText("Defined in Deployment", { exact: false })).toBeVisible();
  await expect(page.getByRole("button", { name: "Copy link" })).toBeVisible();
  const tabs = page.getByRole("tablist", { name: "Agent" });
  await expect(tabs.getByRole("tab")).toHaveText(["Sessions", "Triggers", "Slack", "Definition"]);
  await expect(tabs.getByRole("tab", { name: "Sessions", exact: true })).toHaveAttribute("aria-selected", "true");
  await expect(page.getByRole("tabpanel", { name: "Sessions" })).toContainText("This Agent has no Sessions yet.");
  await expect.poll(() => paths.some(path => path.includes("/sessions?agent=support"))).toBe(true);
  expect(paths.some(path => path.includes("agent_id="))).toBe(false);

  await tabs.getByRole("tab", { name: "Sessions", exact: true }).focus();
  await page.keyboard.press("ArrowRight");
  await expect(page).toHaveURL("/agents/support/triggers");
  await expect(tabs.getByRole("tab", { name: "Triggers", exact: true })).toBeFocused();
  await expect(page.getByText("daily", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Load more triggers" }).click();
  await expect(page.getByRole("button", { name: "Load more triggers" })).toHaveCount(0);
  expect(paths.some(path => path.endsWith("/triggers?agent=support"))).toBe(true);
  expect(paths.some(path => path.endsWith("/triggers?cursor=older&agent=support"))).toBe(true);

  await tabs.getByRole("tab", { name: "Slack", exact: true }).click();
  await expect(page).toHaveURL("/agents/support/slack");
  await expect(page.getByRole("button", { name: "Connect Slack", exact: true })).toBeVisible();
  await expect(page.getByRole("dialog")).toHaveCount(0);

  await tabs.getByRole("tab", { name: "Definition", exact: true }).click();
  await expect(page).toHaveURL("/agents/support/definition");
  const definition = page.getByRole("tabpanel", { name: "Definition" });
  await expect(definition.getByRole("link", { name: "browser", exact: true })).toHaveAttribute("href", "/templates/browser");
  await expect(definition.getByRole("link", { name: "000000000501", exact: true })).toHaveAttribute("href", `/deployments/${deployment}`);
  expect(paths.some(path => path.endsWith(`/agents/support?deployment_id=${deployment}`))).toBe(true);

  await page.goto(`/agents/support/slack/?${scope}`);
  await expect(tabs.getByRole("tab", { name: "Slack", exact: true })).toHaveAttribute("aria-selected", "true");
  await page.goto(`/agents/support/definition?${scope}`);
  await expect(page).toHaveURL("/agents/support/definition");
  await expect(tabs.getByRole("tab", { name: "Definition", exact: true })).toHaveAttribute("aria-selected", "true");
});

test("the Agents list filters loaded Agents by name", async ({ page }) => {
  await catalog(page);
  await page.goto(`/agents?${scope}`);
  const filter = page.getByRole("searchbox", { name: "Filter Agents" });
  await filter.fill("supp");
  await expect(page.getByRole("link", { name: "support", exact: true })).toBeVisible();
  await filter.fill("billing");
  await expect(page.getByText("No loaded Agents match this filter.", { exact: true })).toBeVisible();
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
    await expect(page.getByText(current ? "No Agents in the current Deployment." : "No current Deployment.", { exact: true })).toBeVisible();
    await page.getByRole("group", { name: "Show Agents" }).getByRole("button", { name: "All Agents" }).click();
    await expect(page.getByRole("row").filter({ hasText: "support" }).getByText("Not in current Deployment", { exact: true })).toBeVisible();
    await page.getByRole("link", { name: "support", exact: true }).click();
    await expect(page.getByText("New independent Sessions cannot start.", { exact: false })).toBeVisible();
    await page.getByRole("tab", { name: "Definition", exact: true }).click();
    await expect(page.getByText("This Agent is not in the current Deployment.", { exact: true })).toBeVisible();
    await expect(page.getByRole("tabpanel", { name: "Definition" }).getByRole("link", { name: "View Deployments" })).toHaveAttribute("href", "/deployments");
    await page.getByRole("tab", { name: "Slack", exact: true }).click();
    await expect(page.getByRole("button", { name: "Connect Slack", exact: true })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("retained-agent.png"), fullPage: true });
  });
}

test("viewer sees who manages an Agent's Slack connection without reading it", async ({ page }) => {
  const paths = await catalog(page, { current: false, defined: false, role: "viewer" });
  await page.goto(`/agents/support/slack?${scope}`);
  await expect(page.getByRole("heading", { name: "support", exact: true })).toBeVisible();
  await expect(page.getByText("Organization owners and admins can view and manage this connection.", { exact: false })).toBeVisible();
  await expect(page.getByRole("button", { name: /Connect Slack|Disconnect|Discard setup|Reauthorize/ })).toHaveCount(0);
  expect(paths.some(path => path.endsWith("/slack"))).toBe(false);
});

test("Deployment details are read-only and pinned definitions stay inside their Deployment", async ({ page }, testInfo) => {
  const paths = await catalog(page);
  await page.goto(`/deployments/${historical}?${scope}`);
  await page.getByRole("link", { name: "support", exact: true }).click();
  await expect(page).toHaveURL(`/deployments/${historical}/agents/support`);
  const breadcrumb = page.getByRole("navigation", { name: "Breadcrumb" }).getByRole("link");
  await expect(breadcrumb).toHaveText(["Deployments", "000000000502"]);
  await expect(breadcrumb.nth(1)).toHaveAttribute("href", `/deployments/${historical}`);
  await expect(page.getByText("This definition belongs to an earlier or unpromoted Deployment.", { exact: false })).toBeVisible();
  await expect(page.getByRole("link", { name: "Open Agent" })).toHaveAttribute("href", "/agents/support");
  await expect(page.getByRole("button", { name: /Slack/ })).toHaveCount(0);
  await page.getByRole("link", { name: "browser", exact: true }).click();
  await expect(page).toHaveURL(`/deployments/${historical}/templates/browser`);
  await expect(page.getByRole("navigation", { name: "Breadcrumb" }).getByRole("link")).toHaveText(["Deployments", "000000000502"]);
  await expect(page.getByText("1 vCPU", { exact: false })).toBeVisible();
  expect(paths.some(path => path.endsWith(`/agents/support?deployment_id=${historical}`))).toBe(true);
  expect(paths.some(path => path.endsWith(`/templates/browser?deployment_id=${historical}`))).toBe(true);
  await page.goto(`/deployments/${historical}?${scope}`);
  await expect(page.getByRole("tab", { name: "Events", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: /Promote|Slack connection|Triggers/ })).toHaveCount(0);
  expect(paths.some(path => path.includes("/triggers"))).toBe(false);
  await page.screenshot({ path: testInfo.outputPath("deployment-read-only.png"), fullPage: true });
});

test("the current Template opens inside the Computers section", async ({ page }) => {
  const paths = await catalog(page);
  await page.goto(`/templates/browser?${scope}`);
  await expect(page.getByRole("navigation", { name: "Breadcrumb" }).getByRole("link")).toHaveText(["Computers", "Templates"]);
  await expect(page.getByText("1 vCPU", { exact: false })).toBeVisible();
  await expect(page.getByRole("navigation", { name: "Sections" }).locator("[aria-current=page]")).toHaveText(["Computers"]);
  expect(paths.some(path => path.endsWith(`/templates/browser?deployment_id=${deployment}`))).toBe(true);
});
