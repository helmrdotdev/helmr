import { expect, test, type Page } from "@playwright/test";

const projectID = "console-demo";
const environmentID = "production";
const deploymentID = "00000000-0000-7000-8000-000000000501";
const base = `/api/projects/${projectID}/environments/${environmentID}`;
const now = "2026-10-09T00:00:00Z";

async function catalog(page: Page, permissions = ["agents.read", "deployments.read", "deployments.write"], current = deploymentID) {
  const environment = { environment: environmentID, project: projectID, name: "Development", color_hex: "#008000", is_default: true, created_at: now, updated_at: now };
  const project = { project: projectID, name: "Test", default_region_id: "local", is_default: true, environments: [environment], created_at: now, updated_at: now };
  const deployment = { id: deploymentID, version: "test-version", bundle_digest: `sha256:${"b".repeat(64)}`, created_at: now };
  await page.route("**/api/**", async route => {
    const path = new URL(route.request().url()).pathname;
    let json: unknown;
    if (path === "/api/me") json = { user_id: "local", display_name: "Local", org_id: "org", permissions, admin: false, organization_required: false, project_required: false };
    else if (path === "/api/projects") json = { projects: [project] };
    else if (path === `/api/projects/${projectID}`) json = project;
    else if (path === base) json = environment;
    else if (path === `${base}/deployments`) json = { deployments: [deployment] };
    else if (path === `${base}/deployments/${deploymentID}`) json = deployment;
    else if (path === `${base}/deployments/current`) json = { ...deployment, id: current };
    else if (path === `${base}/agents`) json = { deployment_id: deploymentID, agents: [{ agent: "assistant", template: "browser" }] };
    else if (path.endsWith("/turns")) json = { turns: [] };
    else if (path.endsWith("/events")) json = { records: [], next_after: 0, has_more: false, retained_after: 0 };
    else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } }); return; }
    await route.fulfill({ json });
  });
}


for (const view of ["list"] as const) {
  test(`Deployment ${view} preserves promotion with confirmation and scope`, async ({ page }) => {
    await catalog(page, undefined, "another-deployment");
    const requests: string[] = [];
    await page.route(`**${base}/deployments/${deploymentID}/promote`, async route => {
      requests.push(route.request().method());
      await route.fulfill({ json: { id: deploymentID, version: "test-version", created_at: now, bundle_digest: `sha256:${"b".repeat(64)}` } });
      await page.route(`**${base}/deployments/current`, route => route.fulfill({ json: { id: deploymentID, version: "test-version", created_at: now, bundle_digest: `sha256:${"b".repeat(64)}` } }));
    });
    await page.goto("/deployments");
    await page.getByRole("button", { name: "Actions for test-version" }).click();
    await page.getByRole("menuitem", { name: "Promote", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Promote deployment" });
    await expect(dialog).toContainText("Existing sessions keep their pinned deployment");
    expect(requests).toEqual([]);
    await dialog.getByRole("button", { name: "Promote", exact: true }).click();
    await expect(dialog).toHaveCount(0);
    expect(requests).toEqual(["POST"]);
    await expect(page.getByText("Current", { exact: true })).toBeVisible();
  });
}

for (const condition of ["current", "no write permission", "unknown current"] as const) {
  test(`Deployment promotion is guarded when ${condition}`, async ({ page }) => {
    await catalog(page, condition === "no write permission" ? ["agents.read", "deployments.read"] : undefined, condition === "current" ? deploymentID : "another-deployment");
    if (condition === "unknown current") await page.route(`**${base}/deployments/current`, route => route.fulfill({ status: 503, json: { error: { code: "unavailable", message: "Unavailable" } } }));
    await page.goto(`/deployments/${deploymentID}`);
    await expect(page.getByText("assistant", { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Promote", exact: true })).toHaveCount(0);
    await expect(page.locator("form")).toHaveCount(0);
  });
}

test("Computer exposes independently paged Sessions and Commands with its Template and residency", async ({ page }) => {
  await catalog(page, ["computers.read", "sessions.read", "deployments.read"]);
  const computer = "00000000-0000-7000-8000-000000000701";
  const session = "00000000-0000-7000-8000-000000000801";
  const command = "00000000-0000-7000-8000-000000000901";
  const requestPaths: string[] = [];
  await page.route(`**${base}/computers/**`, async route => {
    const url = new URL(route.request().url());
    requestPaths.push(url.pathname + url.search);
    if (url.pathname.endsWith("/sessions")) {
      await route.fulfill({ json: url.searchParams.has("cursor") ? { sessions: [] } : { sessions: [{ id: session, usage_state: "unreconciled", created_at: now }], next_cursor: "sessions-page-2" } });
    } else if (url.pathname.endsWith("/commands")) {
      await route.fulfill({ json: { commands: [{ id: command, usage_state: "running", created_at: now }] } });
    } else {
      await route.fulfill({ json: { id: computer, template: "browser", deployment_id: deploymentID, status: "available", residency: "cold", secrets: [], created_at: now, updated_at: now, last_activity_at: now } });
    }
  });
  await page.goto(`/computers/${computer}`);
  // The Computer's tables are tabs, not titled panels over column headers.
  const sections = page.getByRole("tablist", { name: "Computer sections" });
  const tabs = sections.getByRole("tab");
  await expect(tabs).toHaveCount(3);
  await expect(tabs.nth(0)).toHaveAccessibleName("Sessions");
  await expect(tabs.nth(1)).toHaveAccessibleName("Commands");
  await expect(tabs.nth(2)).toHaveAccessibleName("Secret placements, 0");
  await expect(page.getByRole("heading", { name: "Sessions", exact: true })).toHaveCount(0);
  await expect(sections.getByRole("tab", { name: "Sessions", exact: true })).toHaveAttribute("aria-selected", "true");
  await expect(page.getByRole("tabpanel", { name: "Sessions" }).getByText("Unreconciled", { exact: true })).toBeVisible();
  // The header badge shows the residency, not the lifecycle; the rail keeps facts only.
  await expect(page.locator("h1").locator("..").getByText("Cold", { exact: true })).toBeVisible();
  await expect(page.getByRole("complementary", { name: "Computer details" }).getByText("Cold", { exact: true })).toHaveCount(0);
  await expect(page.getByText(/^(Active|Available)$/)).toHaveCount(0);
  await expect(page.getByText("browser", { exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: "browser" })).toHaveCount(0);
  await page.getByRole("button", { name: "Load more sessions", exact: true }).click();
  await expect(page.getByRole("button", { name: "Load more sessions", exact: true })).toHaveCount(0);
  expect(requestPaths.some(path => path.endsWith("/sessions?cursor=sessions-page-2"))).toBe(true);
  // Commands load when their tab opens, and paging Sessions never pages them.
  expect(requestPaths.some(path => path.includes("/commands"))).toBe(false);
  await sections.getByRole("tab", { name: "Commands", exact: true }).click();
  await expect(page.getByRole("tabpanel", { name: "Commands" }).getByText("Running", { exact: true })).toBeVisible();
  expect(requestPaths.some(path => path.includes("/commands?cursor="))).toBe(false);
  await sections.getByRole("tab", { name: /^Secret placements/ }).click();
  await expect(page.getByRole("tabpanel", { name: /^Secret placements/ })).toContainText("No secrets are attached.");
  expect(requestPaths.some(path => path.includes("/members"))).toBe(false);
});

test("a Computer's status is its residency, its lifecycle only while deleting, in the header and the Computers list", async ({ page }) => {
  await catalog(page, ["computers.read", "sessions.read", "deployments.read"]);
  const computer = (id: string, fields: Record<string, unknown>) => ({ id, template: "browser", deployment_id: deploymentID, status: "available", residency: "running", secrets: [], created_at: now, updated_at: now, last_activity_at: now, ...fields });
  const running = computer("00000000-0000-7000-8000-000000000711", {});
  const unavailable = computer("00000000-0000-7000-8000-000000000712", { residency: "unavailable", error: { code: "computer_preparation_failed", message: "The image could not be prepared." } });
  const deleting = computer("00000000-0000-7000-8000-000000000713", { status: "deleting", residency: "cold" });
  await page.route(`**${base}/computers**`, async route => {
    const url = new URL(route.request().url());
    if (url.pathname === `${base}/computers`) await route.fulfill({ json: { computers: [running, unavailable, deleting] } });
    else if (url.pathname.endsWith("/sessions")) await route.fulfill({ json: { sessions: [] } });
    else if (url.pathname.endsWith("/commands")) await route.fulfill({ json: { commands: [] } });
    else await route.fulfill({ json: [running, unavailable, deleting].find(item => url.pathname.endsWith(item.id)) });
  });
  await page.goto("/computers");
  await expect(page.getByRole("columnheader")).toHaveText(["Computer", "Status", "Key", "Template", "Last activity", "Created"]);
  await expect(page.getByRole("columnheader", { name: "Residency" })).toHaveCount(0);
  const row = (id: string) => page.getByRole("row").filter({ hasText: id.slice(-12) });
  await expect(row(running.id).getByRole("cell").nth(1)).toHaveText("Running");
  await expect(row(unavailable.id).getByRole("cell").nth(1)).toHaveText("Unavailable");
  await expect(row(unavailable.id).getByTitle("The image could not be prepared.")).toBeVisible();
  await expect(row(deleting.id).getByRole("cell").nth(1)).toHaveText("Deleting");
  await expect(page.getByText(/^(Active|Available)$/)).toHaveCount(0);

  await page.goto(`/computers/${unavailable.id}`);
  const title = page.locator("h1").locator("..");
  await expect(title.getByText("Unavailable", { exact: true })).toBeVisible();
  await expect(page.getByRole("complementary", { name: "Computer details" })).toContainText("The image could not be prepared.");
  await page.goto(`/computers/${deleting.id}`);
  await expect(title.getByText("Deleting", { exact: true })).toBeVisible();
  await expect(title.getByText("Cold", { exact: true })).toHaveCount(0);
});
