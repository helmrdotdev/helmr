import { expect, test, type Page } from "@playwright/test";
const projectID = "console-demo";
const environmentID = "demo";
const now = "2026-10-07T00:00:00Z";
async function scope(page: Page) {
  let environment = { environment: environmentID, project: projectID, name: "Production", color_hex: "#008000", is_default: true, created_at: now, updated_at: now, history_retention_mode: "until_environment_deletion" };
  const project = () => ({ project: projectID, name: "Test", default_region_id: "local", is_default: true, environments: [environment], created_at: now, updated_at: now });
  const writes: Record<string, unknown>[] = [];
  await page.route("**/api/**", async route => {
    const endpoint = new URL(route.request().url()).pathname;
    if (["POST", "PATCH"].includes(route.request().method())) {
      const body = route.request().postDataJSON(); writes.push(body);
      if (endpoint === "/api/projects") await route.fulfill({ json: { ...project(), ...body } });
      else { environment = { ...environment, ...body }; await route.fulfill({ json: environment }); }
    } else if (endpoint === "/api/me") await route.fulfill({ json: { user_id: "local", display_name: "Local", org_id: "org", permissions: ["projects.manage"], admin: false, organization_required: false, project_required: false } });
    else if (endpoint === "/api/projects") await route.fulfill({ json: { projects: [project()] } });
    else if (endpoint === `/api/projects/${projectID}`) await route.fulfill({ json: project() });
    else if (endpoint === "/api/regions") await route.fulfill({ json: { regions: [{ id: "local", display_name: "Local" }] } });
    else if (endpoint.endsWith("/tokens")) await route.fulfill({ json: { tokens: [] } });
    else await route.fulfill({ status: 404, json: { error: { code: "not_found", message: endpoint } } });
  });
  return { writes, setPolicy: (seconds: number) => { environment = { ...environment, history_retention_mode: "duration", ...{ history_retention_seconds: seconds } }; } };
}
test("Project creation does not ask for retention", async ({ page }) => {
  const { writes } = await scope(page);
  await page.goto("/projects/new");
  await page.getByLabel("Name", { exact: true }).fill("New project");
  await expect(page.getByText("Session history retention", { exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "Create", exact: true }).click();
  await expect.poll(() => writes.length).toBe(1);
  expect(writes[0]).not.toHaveProperty("history_retention_mode");
  expect(writes[0]).not.toHaveProperty("history_retention_seconds");
});

test("Environment settings edit retention and creation uses the default", async ({ page }) => {
  const { writes } = await scope(page);
  await page.goto("/settings/environments");
  await expect(page.getByRole("cell", { name: "No expiry", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Edit", exact: true }).click();
  await expect(page.getByRole("combobox", { name: "History retention", exact: true })).toHaveValue("until_environment_deletion");
  await expect(page.getByText("Changes apply only to new Sessions.", { exact: false })).toBeVisible();
  await page.getByRole("combobox", { name: "History retention", exact: true }).selectOption("duration");
  await page.getByLabel("Retention period", { exact: true }).fill("0");
  expect(await page.getByLabel("Retention period", { exact: true }).evaluate((input: HTMLInputElement) => input.validity.rangeUnderflow)).toBe(true);
  await page.getByRole("button", { name: "Save", exact: true }).click();
  expect(writes).toHaveLength(0);
  await page.getByLabel("Retention period", { exact: true }).fill("30");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => writes.length).toBe(1);
  expect(writes[0]).toMatchObject({ history_retention_mode: "duration", history_retention_seconds: 2592000 });
  await expect(page.getByRole("cell", { name: "30 days", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Edit", exact: true }).click();
  await expect(page.getByLabel("Retention period", { exact: true })).toHaveValue("30");
  await expect(page.getByRole("combobox", { name: "Unit", exact: true })).toHaveValue("86400");
  await page.getByRole("combobox", { name: "History retention", exact: true }).selectOption("until_environment_deletion");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => writes.length).toBe(2);
  expect(writes[1]).toMatchObject({ history_retention_mode: "until_environment_deletion" });
  expect(writes[1]).not.toHaveProperty("history_retention_seconds");
  await expect(page.getByRole("cell", { name: "No expiry", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "New environment", exact: true }).click();
  await expect(page.getByText("Session history retention", { exact: true })).toHaveCount(0);
  await page.getByLabel("Name", { exact: true }).fill("Preview");
  await page.getByRole("button", { name: "Create", exact: true }).click();
  await expect.poll(() => writes.length).toBe(3);
  expect(writes[2]).not.toHaveProperty("history_retention_mode");
  expect(writes[2]).not.toHaveProperty("history_retention_seconds");
});

for (const kind of ["project", "environment"] as const) {
  test(`scope switcher creates a ${kind} without retention fields`, async ({ page }) => {
    const { writes } = await scope(page);
    await page.goto("/settings/environments");
    await page.getByRole("button", { name: "Project Test / Env Production" }).click();
    await page.getByRole("dialog", { name: "Choose project" }).getByRole("button", { name: `New ${kind}`, exact: true }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText("Session history retention", { exact: true })).toHaveCount(0);
    await dialog.getByLabel("Name", { exact: true }).fill("Preview");
    await dialog.getByRole("button", { name: "Create", exact: true }).click();
    await expect.poll(() => writes.length).toBe(1);
    expect(writes[0]).not.toHaveProperty("history_retention_mode");
    expect(writes[0]).not.toHaveProperty("history_retention_seconds");
  });
}

test("an ordinary edit preserves a policy changed while its modal was open", async ({ page }) => {
  const { writes, setPolicy } = await scope(page);
  await page.goto("/settings/environments");
  await page.getByRole("button", { name: "Edit", exact: true }).click();
  await expect(page.getByRole("combobox", { name: "History retention", exact: true })).toHaveValue("until_environment_deletion");
  setPolicy(900);
  await page.getByLabel("Name", { exact: true }).fill("Renamed");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => writes.length).toBe(1);
  expect(writes[0]).not.toHaveProperty("history_retention_mode");
  expect(writes[0]).not.toHaveProperty("history_retention_seconds");
  await expect(page.getByRole("cell", { name: "15 minutes" })).toBeVisible();
});


test("real Project and Environment creation persist no expiry and settings update it", async ({ page }) => {
  await page.goto("/dev/login");
  const projectID = `retention-${crypto.randomUUID()}`;
  await page.goto("/projects/new");
  await page.getByLabel("Name", { exact: true }).fill(projectID);
  await expect(page.getByText("Session history retention", { exact: true })).toHaveCount(0);
  const creation = page.waitForResponse(response => response.url().endsWith("/api/projects") && response.request().method() === "POST");
  await page.getByRole("button", { name: "Create", exact: true }).click();
  const response = await creation;
  expect(response.ok(), await response.text()).toBeTruthy();
  const project = await response.json();
  expect(project.environments).toHaveLength(2);
  for (const environment of project.environments) {
    expect(environment.history_retention_mode).toBe("until_environment_deletion");
    expect(environment).not.toHaveProperty("history_retention_seconds");
  }
  await page.goto(`/settings/environments?project=${projectID}&environment=production`);
  await page.getByRole("button", { name: "New environment", exact: true }).click();
  await page.getByRole("dialog").getByLabel("Name", { exact: true }).fill("Preview");
  await page.getByRole("dialog").getByRole("button", { name: "Create", exact: true }).click();
  const row = page.getByRole("row").filter({ has: page.getByRole("button", { name: "Copy preview", exact: true }) });
  await expect(row.getByRole("cell", { name: "No expiry", exact: true })).toBeVisible();
  await row.getByRole("button", { name: "Edit", exact: true }).click();
  await page.getByRole("combobox", { name: "History retention", exact: true }).selectOption("duration");
  await page.getByLabel("Retention period", { exact: true }).fill("7");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(row.getByRole("cell", { name: "7 days", exact: true })).toBeVisible();
  const saved = await page.request.get(`/api/projects/${projectID}`);
  expect(saved.ok()).toBeTruthy();
  const environments = (await saved.json()).environments;
  expect(environments.find((env: { environment: string }) => env.environment === "preview")).toMatchObject({ history_retention_mode: "duration", history_retention_seconds: 604800 });
  expect(environments.find((env: { environment: string }) => env.environment === "production").history_retention_mode).toBe("until_environment_deletion");
});


test("duration settings preserve custom seconds and allow hours", async ({ page }) => {
  const { writes, setPolicy } = await scope(page);
  setPolicy(3601);
  await page.goto("/settings/environments");
  await page.getByRole("button", { name: "Edit", exact: true }).click();
  await expect(page.getByLabel("Retention period", { exact: true })).toHaveValue("3601");
  await expect(page.getByRole("combobox", { name: "Unit", exact: true })).toHaveValue("1");
  await page.getByLabel("Retention period", { exact: true }).fill("2");
  await page.getByRole("combobox", { name: "Unit", exact: true }).selectOption("3600");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => writes.length).toBe(1);
  expect(writes[0]).toMatchObject({ history_retention_mode: "duration", history_retention_seconds: 7200 });
  await expect(page.getByRole("cell", { name: "2 hours", exact: true })).toBeVisible();
});
