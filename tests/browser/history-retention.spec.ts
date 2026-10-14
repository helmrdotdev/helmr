import { expect, test, type Page } from "@playwright/test";

const projectID = "console-demo";
const now = "2026-10-07T00:00:00Z";
async function scope(page: Page, options: { policy?: string; manage?: boolean; failRead?: boolean; failWrite?: boolean } = {}) {
  let policy = options.policy ?? "30d";
  let environment = { environment: "demo", project: projectID, name: "Production", color_hex: "#008000", is_default: true, created_at: now, updated_at: now };
  const project = () => ({ project: projectID, name: "Test", default_region_id: "local", is_default: true, environments: [environment], created_at: now, updated_at: now });
  const writes: { path: string; body: Record<string, unknown> }[] = [];
  await page.route("**/api/**", async route => {
    const path = new URL(route.request().url()).pathname;
    if (["POST", "PATCH"].includes(route.request().method())) {
      const body = route.request().postDataJSON(); writes.push({ path, body });
      if (path.endsWith("/settings")) {
        if (options.failWrite) return route.fulfill({ status: 500, json: { error: { code: "internal_error", message: "Could not save policy." } } });
        policy = body.history_retention;
        return route.fulfill({ json: { history_retention: policy } });
      }
      if (path === "/api/projects") return route.fulfill({ json: { ...project(), ...body } });
      environment = { ...environment, ...body }; return route.fulfill({ json: environment });
    }
    if (path === "/api/me") return route.fulfill({ json: { user_id: "local", display_name: "Local", org_id: "org", permissions: options.manage === false ? [] : ["projects.manage"], admin: false, organization_required: false, project_required: false } });
    if (path === "/api/projects") return route.fulfill({ json: { projects: [project()] } });
    if (path === `/api/projects/${projectID}`) return route.fulfill({ json: project() });
    if (path.endsWith("/settings")) {
      if (options.failRead) return route.fulfill({ status: 500, json: { error: { code: "internal_error", message: "Read failed." } } });
      return route.fulfill({ json: { history_retention: policy } });
    }
    if (path === "/api/regions") return route.fulfill({ json: { regions: [{ id: "local", display_name: "Local" }] } });
    return route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } });
  });
  return { writes };
}

const mode = (page: Page) => page.getByRole("combobox", { name: "History retention", exact: true });
const unit = (page: Page) => page.getByRole("combobox", { name: "Unit", exact: true });
const choose = async (page: Page, combobox: ReturnType<typeof mode>, option: string) => {
  await combobox.click();
  await page.getByRole("option", { name: option, exact: true }).click();
};
const period = (page: Page) => page.getByLabel("Retention period", { exact: true });
const save = (page: Page) => page.getByRole("button", { name: "Save history retention", exact: true });

test("Project creation uses the default without asking for a policy", async ({ page }) => {
  const { writes } = await scope(page);
  await page.goto("/projects/new");
  await page.getByLabel("Name", { exact: true }).fill("New project");
  await expect(mode(page)).toHaveCount(0);
  await page.getByRole("button", { name: "Create", exact: true }).click();
  await expect.poll(() => writes.length).toBe(1);
  expect(writes[0]!.body).not.toHaveProperty("history_retention");
});

for (const kind of ["project", "environment"] as const) {
  test(`scope switcher creates a ${kind} without retention fields`, async ({ page }) => {
    const { writes } = await scope(page);
    await page.goto("/settings/environments");
    await page.getByRole("button", { name: "Project Test / Env Production" }).click();
    await page.getByRole("dialog", { name: "Choose project" }).getByRole("button", { name: `New ${kind}`, exact: true }).click();
    const dialog = page.getByRole("dialog");
    await expect(mode(page)).toHaveCount(0);
    await dialog.getByLabel("Name", { exact: true }).fill("Preview");
    await dialog.getByRole("button", { name: "Create", exact: true }).click();
    await expect.poll(() => writes.length).toBe(1);
    expect(writes[0]!.body).not.toHaveProperty("history_retention");
  });
}

test("Environment metadata editing has no retention policy", async ({ page }) => {
  const { writes } = await scope(page);
  await page.goto("/settings/environments");
  await expect(page.getByRole("columnheader", { name: "History retention" })).toHaveCount(0);
  await page.getByRole("button", { name: "Edit", exact: true }).click();
  await expect(mode(page)).toHaveCount(0);
  await page.getByLabel("Name", { exact: true }).fill("Renamed");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => writes.length).toBe(1);
  expect(writes[0]!.body).toEqual({ name: "Renamed", color_hex: "#008000" });
});

test("Project settings shorten with confirmation, persist, and disable expiry", async ({ page }) => {
  const { writes } = await scope(page);
  await page.goto("/settings/projects");
  await expect(period(page)).toHaveValue("30");
  await expect(page.getByText("Applies to all remaining Session history in every Environment of this Project.")).toBeVisible();
  await expect(save(page)).toBeDisabled();
  await period(page).fill("7");
  page.once("dialog", dialog => dialog.dismiss());
  await save(page).click();
  expect(writes).toHaveLength(0);
  page.once("dialog", async dialog => { expect(dialog.message()).toContain("permanently deleted"); await dialog.accept(); });
  await save(page).click();
  await expect.poll(() => writes.length).toBe(1);
  expect(writes[0]).toEqual({ path: `/api/projects/${projectID}/settings`, body: { history_retention: "7d" } });
  await expect(page.getByRole("status").filter({ hasText: "History retention" })).toHaveText("History retention saved.");
  await page.reload();
  await expect(period(page)).toHaveValue("7");
  await choose(page, mode(page), "Never expire");
  await save(page).click();
  await expect.poll(() => writes.length).toBe(2);
  expect(writes[1]!.body).toEqual({ history_retention: "never" });
  await page.reload();
  await expect(mode(page)).toHaveText("Never expire");
  await expect(period(page)).toHaveCount(0);
});

test("custom seconds round-trip exactly and hours normalize", async ({ page }) => {
  const { writes } = await scope(page, { policy: "3601s" });
  await page.goto("/settings/projects");
  await expect(period(page)).toHaveValue("3601");
  await expect(unit(page)).toHaveText("Seconds");
  await expect(save(page)).toBeDisabled();
  await period(page).fill("48");
  await choose(page, unit(page), "Hours");
  await save(page).click();
  await expect.poll(() => writes.length).toBe(1);
  expect(writes[0]!.body).toEqual({ history_retention: "2d" });
});

test("invalid durations are rejected before a request", async ({ page }) => {
  const { writes } = await scope(page);
  await page.goto("/settings/projects");
  await period(page).fill("0");
  await save(page).click();
  expect(writes).toHaveLength(0);
  await period(page).fill("1.5");
  await save(page).click();
  expect(writes).toHaveLength(0);
});

test("viewers can read but cannot edit Project retention", async ({ page }) => {
  await scope(page, { manage: false });
  await page.goto("/settings/projects");
  await expect(period(page)).toHaveValue("30");
  await expect(mode(page)).toBeDisabled();
  await expect(period(page)).toBeDisabled();
  await expect(save(page)).toHaveCount(0);
});

test("failed settings read does not present an editable default", async ({ page }) => {
  await scope(page, { failRead: true });
  await page.goto("/settings/projects");
  await expect(page.getByRole("alert")).toContainText("Could not load history retention.");
  await expect(mode(page)).toHaveCount(0);
});

test("failed save preserves the draft and shows the error", async ({ page }) => {
  await scope(page, { failWrite: true });
  await page.goto("/settings/projects");
  await choose(page, mode(page), "Never expire");
  await save(page).click();
  await expect(page.getByRole("alert")).toHaveText("Could not save policy.");
  await expect(mode(page)).toHaveText("Never expire");
  await expect(save(page)).toBeEnabled();
});

test("real Project defaults to 30d and settings persist across all Environments", async ({ page }) => {
  await page.goto("/dev/login");
  const project = `retention-${crypto.randomUUID()}`;
  await page.goto("/projects/new");
  await page.getByLabel("Name", { exact: true }).fill(project);
  const creation = page.waitForResponse(response => response.url().endsWith("/api/projects") && response.request().method() === "POST");
  await page.getByRole("button", { name: "Create", exact: true }).click();
  const response = await creation;
  expect(response.ok(), await response.text()).toBeTruthy();
  const created = await response.json();
  expect(created.environments).toHaveLength(2);
  for (const environment of created.environments) expect(environment).not.toHaveProperty("history_retention_mode");
  await page.goto(`/settings/projects?project=${project}&environment=production`);
  await expect(period(page)).toHaveValue("30");
  await period(page).fill("7");
  page.once("dialog", dialog => dialog.accept());
  await save(page).click();
  await expect(page.getByRole("status").filter({ hasText: "History retention" })).toHaveText("History retention saved.");
  await page.goto(`/settings/projects?project=${project}&environment=staging`);
  await expect(period(page)).toHaveValue("7");
  const saved = await page.request.get(`/api/projects/${project}/settings`);
  expect(saved.ok()).toBeTruthy();
  expect(await saved.json()).toEqual({ history_retention: "7d" });
  await choose(page, mode(page), "Never expire");
  await save(page).click();
  await expect(page.getByRole("status").filter({ hasText: "History retention" })).toHaveText("History retention saved.");
  await page.reload();
  await expect(mode(page)).toHaveText("Never expire");
  const env = await page.request.post(`/api/projects/${project}/environments`, { data: { environment: "preview", name: "Preview", color_hex: "#123456" } });
  expect(env.ok()).toBeTruthy();
  expect(await env.json()).not.toHaveProperty("history_retention");
  const after = await page.request.get(`/api/projects/${project}/settings`);
  expect(await after.json()).toEqual({ history_retention: "never" });
});
