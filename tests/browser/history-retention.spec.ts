import { expect, test, type Page } from "@playwright/test";
const projectID = "00000000-0000-7000-8000-000000000301";
const environmentID = "00000000-0000-7000-8000-000000000403";
const now = "2026-10-07T00:00:00Z";
async function scope(page: Page) {
  let environment = { id: environmentID, project_id: projectID, slug: "production", name: "Production", color_hex: "#008000", is_default: true, created_at: now, updated_at: now, history_retention_mode: "until_environment_deletion" };
  const project = () => ({ id: projectID, slug: "test", name: "Test", default_region_id: "local", is_default: true, environments: [environment], created_at: now, updated_at: now });
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
test("Project creation requires an explicit retention choice", async ({ page }) => {
  const { writes } = await scope(page);
  await page.goto("/projects/new");
  await page.getByLabel("Name", { exact: true }).fill("New project");
  await expect(page.getByLabel("History retention policy")).toHaveValue("");
  await page.getByRole("button", { name: "Create", exact: true }).click();
  expect(writes).toHaveLength(0);
  await page.getByLabel("History retention policy").selectOption("duration");
  await page.getByLabel("History retention seconds").fill("3600");
  await page.getByRole("button", { name: "Create", exact: true }).click();
  await expect.poll(() => writes.length).toBe(1);
  expect(writes[0]).toMatchObject({ history_retention_mode: "duration", history_retention_seconds: 3600 });
});
test("Environment creation and editing display and submit the selected policy", async ({ page }) => {
  const { writes } = await scope(page);
  await page.goto("/settings/environments");
  await expect(page.getByRole("cell", { name: "Until Environment deletion", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Edit", exact: true }).click();
  await expect(page.getByLabel("History retention policy")).toHaveValue("until_environment_deletion");
  await page.getByLabel("History retention policy").selectOption("duration");
  await page.getByLabel("History retention seconds").fill("7200");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => writes.length).toBe(1);
  expect(writes[0]).toMatchObject({ history_retention_mode: "duration", history_retention_seconds: 7200 });
  await expect(page.getByRole("cell", { name: "7200 seconds after termination and obligation release" })).toBeVisible();
  await page.getByRole("button", { name: "New environment", exact: true }).click();
  await expect(page.getByLabel("History retention policy")).toHaveValue("");
  await page.getByLabel("Name", { exact: true }).fill("Preview");
  await page.getByLabel("History retention policy").selectOption("until_environment_deletion");
  await page.getByRole("button", { name: "Create", exact: true }).click();
  await expect.poll(() => writes.length).toBe(2);
  expect(writes[1]).toMatchObject({ history_retention_mode: "until_environment_deletion" });
  expect(writes[1]).not.toHaveProperty("history_retention_seconds");
});

test("an ordinary edit preserves a policy changed while its modal was open", async ({ page }) => {
  const { writes, setPolicy } = await scope(page);
  await page.goto("/settings/environments");
  await page.getByRole("button", { name: "Edit", exact: true }).click();
  await expect(page.getByLabel("History retention policy")).toHaveValue("until_environment_deletion");
  setPolicy(900);
  await page.getByLabel("Name", { exact: true }).fill("Renamed");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => writes.length).toBe(1);
  expect(writes[0]).not.toHaveProperty("history_retention_mode");
  expect(writes[0]).not.toHaveProperty("history_retention_seconds");
  await expect(page.getByRole("cell", { name: "900 seconds after termination and obligation release" })).toBeVisible();
});
