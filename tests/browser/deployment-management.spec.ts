import { expect, test, type Page } from "@playwright/test";

const projectID = "00000000-0000-7000-8000-000000000301";
const environmentID = "00000000-0000-7000-8000-000000000401";
const deploymentID = "00000000-0000-7000-8000-000000000501";
const base = `/api/projects/${projectID}/environments/${environmentID}`;
const now = "2026-10-09T00:00:00Z";

async function catalog(page: Page, permissions = ["agents.read", "deployments.read", "deployments.write"], current = deploymentID) {
  const environment = { id: environmentID, project_id: projectID, slug: "dev", name: "Development", color_hex: "#008000", is_default: true, created_at: now, updated_at: now };
  const project = { id: projectID, slug: "test", name: "Test", default_region_id: "local", is_default: true, environments: [environment], created_at: now, updated_at: now };
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
    else if (path === `${base}/agents`) json = { deployment_id: deploymentID, agents: [{ agent: "assistant", template: "browser", agent_id: "00000000-0000-7000-8000-000000000601" }] };
    else if (path.endsWith("/turns")) json = { turns: [] };
    else if (path.endsWith("/events")) json = { records: [], next_after: 0, has_more: false, retained_after: 0 };
    else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } }); return; }
    await route.fulfill({ json });
  });
}


for (const view of ["list", "detail"] as const) {
  test(`Deployment ${view} preserves promotion with confirmation and scope`, async ({ page }) => {
    await catalog(page, undefined, "another-deployment");
    const requests: string[] = [];
    await page.route(`**${base}/deployments/${deploymentID}/promote`, async route => {
      requests.push(route.request().method());
      await route.fulfill({ json: { id: deploymentID, version: "test-version", created_at: now, bundle_digest: `sha256:${"b".repeat(64)}` } });
      await page.route(`**${base}/deployments/current`, route => route.fulfill({ json: { id: deploymentID, version: "test-version", created_at: now, bundle_digest: `sha256:${"b".repeat(64)}` } }));
    });
    await page.goto(view === "list" ? "/deployments" : `/deployments/${deploymentID}`);
    if (view === "list") await page.getByRole("button", { name: "Actions for test-version" }).click();
    await page.getByRole("button", { name: "Promote", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Promote Deployment" });
    await expect(dialog).toContainText("Existing Sessions keep their pinned Deployment");
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
    await expect(page.getByText("New independent Sessions use only the current Deployment", { exact: false })).toBeVisible();
    await expect(page.getByRole("button", { name: "Promote", exact: true })).toHaveCount(0);
    await expect(page.locator("form")).toHaveCount(0);
  });
}

test("Computer exposes independently paged Sessions and Commands with pinned Template", async ({ page }) => {
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
      await route.fulfill({ json: { id: computer, template: "browser", deployment_id: deploymentID, status: "available", residency: "resident", secrets: [], created_at: now, updated_at: now, last_activity_at: now } });
    }
  });
  await page.goto(`/computers/${computer}`);
  await expect(page.getByRole("heading", { name: "Sessions", exact: true })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Commands", exact: true })).toBeVisible();
  await expect(page.getByText("unreconciled", { exact: true })).toBeVisible();
  await expect(page.getByText("running", { exact: true })).toBeVisible();
  const templateLink = page.getByRole("link", { name: "browser", exact: true });
  await expect(templateLink).toHaveAttribute("href", new RegExp(`/templates/browser\\?.*deployment_id=${deploymentID}`));
  await page.getByRole("button", { name: "Load more sessions", exact: true }).click();
  await expect(page.getByRole("button", { name: "Load more sessions", exact: true })).toHaveCount(0);
  expect(requestPaths.some(path => path.endsWith("/sessions?cursor=sessions-page-2"))).toBe(true);
  expect(requestPaths.some(path => path.includes("/commands?cursor="))).toBe(false);
  expect(requestPaths.some(path => path.includes("/members"))).toBe(false);
});
