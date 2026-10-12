import { expect, test, type Page } from "@playwright/test";

const project = "00000000-0000-7000-8000-000000000301";
const environment = "00000000-0000-7000-8000-000000000401";
const deployment = "00000000-0000-7000-8000-000000000501";
const historical = "00000000-0000-7000-8000-000000000502";
const base = `/api/projects/${project}/environments/${environment}`;
const scope = `project_id=${project}&environment_id=${environment}`;
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
    const env = { id: environment, project_id: project, slug: "dev", name: "Development", is_default: true, created_at: now, updated_at: now };
    const proj = { id: project, slug: "test", name: "Test", is_default: true, environments: [env], created_at: now, updated_at: now };
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
    else if (path === `${base}/agents/support/slack`) json = { publication: null };
    else if (path === `${base}/schedules`) json = url.searchParams.has("cursor") ? { schedules: [] } : { schedules: [{ id: historical, agent: "support", deployment_id: historical, trigger_key: "daily", cron: { pattern: "0 9 * * *", timezone: "UTC" }, input: [], active_from: now, active_until: now }], next_cursor: "older" };
    else if (path === `${base}/sessions`) json = { sessions: [] };
    else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } }); return; }
    await route.fulfill({ json });
  });
  return paths;
}

test("current Agent navigation owns connections and filters schedules and Sessions by authored ID", async ({ page }) => {
  const paths = await catalog(page);
  await page.goto(`/agents?${scope}`);
  await expect(page.getByRole("combobox", { name: "Show Agents" })).toHaveValue("current");
  await page.getByRole("link", { name: "support", exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`/agents/support\\?`));
  await expect(page.getByRole("heading", { name: "Schedules", exact: true })).toBeVisible();
  await expect(page.getByText("daily", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Load more schedules" }).click();
  await expect(page.getByRole("button", { name: "Load more schedules" })).toHaveCount(0);
  expect(paths.some(path => path.endsWith("/schedules?agent=support"))).toBe(true);
  expect(paths.some(path => path.endsWith("/schedules?cursor=older&agent=support"))).toBe(true);
  await page.getByRole("button", { name: "Manage Slack connection" }).click();
  await expect(page.getByRole("dialog", { name: "Slack · support" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Connect Slack", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Close modal", exact: true }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  const definition = page.getByRole("link", { name: "View current definition" });
  await expect(definition).toHaveAttribute("href", `/deployments/${deployment}/agents/support?${scope}`);
  await page.getByRole("link", { name: "View Sessions for this Agent" }).click();
  await expect(page).toHaveURL(new RegExp("/sessions\\?.*agent=support"));
  await expect.poll(() => paths.some(path => path.includes("/sessions?agent=support"))).toBe(true);
  expect(paths.some(path => path.includes("agent_id="))).toBe(false);
});

for (const current of [true, false]) {
  test(`retained Agent remains manageable with ${current ? "no current definition" : "no current Deployment"}`, async ({ page }, testInfo) => {
    await catalog(page, { current, defined: false });
    await page.goto(`/agents?${scope}`);
    await expect(page.getByText(current ? "No Agents in the current Deployment." : "No current Deployment.", { exact: true })).toBeVisible();
    await page.getByRole("combobox", { name: "Show Agents" }).selectOption("all");
    await page.getByRole("link", { name: "support", exact: true }).click();
    await expect(page.getByText("New independent Sessions cannot start.", { exact: false })).toBeVisible();
    await expect(page.getByRole("link", { name: "View current definition" })).toHaveCount(0);
    await page.getByRole("button", { name: "Manage Slack connection" }).click();
    await expect(page.getByRole("dialog", { name: "Slack · support" })).toBeVisible();
    await page.getByRole("button", { name: "Close modal", exact: true }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
    await page.screenshot({ path: testInfo.outputPath("retained-agent.png"), fullPage: true });
  });
}

test("viewer can inspect retained Agent without connection controls", async ({ page }) => {
  const paths = await catalog(page, { current: false, defined: false, role: "viewer" });
  await page.goto(`/agents/support?${scope}`);
  await expect(page.getByRole("heading", { name: "support", exact: true })).toBeVisible();
  await expect(page.getByText("An organization owner or admin can manage this connection.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Manage Slack connection" })).toHaveCount(0);
  expect(paths.some(path => path.endsWith("/slack"))).toBe(false);
});

test("Deployment details are read-only and historical definition links keep exact pins", async ({ page }, testInfo) => {
  const paths = await catalog(page);
  await page.goto(`/deployments/${historical}?${scope}`);
  await page.getByRole("link", { name: "support", exact: true }).click();
  await expect(page).toHaveURL(`/deployments/${historical}/agents/support?${scope}`);
  await expect(page.getByText("This definition belongs to an earlier or unpromoted Deployment.", { exact: false })).toBeVisible();
  await expect(page.getByRole("button", { name: "Manage Slack connection" })).toHaveCount(0);
  await page.getByRole("link", { name: "browser", exact: true }).click();
  await expect(page).toHaveURL(`/deployments/${historical}/templates/browser?${scope}`);
  await expect(page.getByText("1 vCPU", { exact: false })).toBeVisible();
  expect(paths.some(path => path.endsWith(`/agents/support?deployment_id=${historical}`))).toBe(true);
  expect(paths.some(path => path.endsWith(`/templates/browser?deployment_id=${historical}`))).toBe(true);
  await page.goto(`/deployments/${historical}?${scope}`);
  await expect(page.getByRole("button", { name: "Events", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: /Promote|Slack connection|Schedules/ })).toHaveCount(0);
  expect(paths.some(path => path.includes("/schedules"))).toBe(false);
  await page.screenshot({ path: testInfo.outputPath("deployment-read-only.png"), fullPage: true });
});
