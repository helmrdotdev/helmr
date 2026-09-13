import { expect, test, type Page } from "@playwright/test";

const DEMO_SESSION_OPEN_ID = "00000000-0000-7000-8000-000000000701";
const PROJECT_ID = "00000000-0000-7000-8000-000000000301";
const DEMO_ENVIRONMENT_ID = "00000000-0000-7000-8000-000000000403";

async function login(page: Page) {
  await page.goto("/dev/login");
  await expect(page).toHaveURL("/");
}

async function selectDemoEnvironment(page: Page) {
  await page.goto("/settings/environments");
  const demoRow = page.getByRole("row").filter({ hasText: "Demo" });
  await demoRow.getByRole("button", { name: "Use" }).click();
  await expect(demoRow.getByText("Current", { exact: true })).toBeVisible();
}

test("console demo seed populates overview and navigation pages", async ({ page }) => {
  await login(page);
  await selectDemoEnvironment(page);
  await page.goto("/");

  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Set up your first task" })).toHaveCount(0);
  await expect(page.getByRole("heading", { name: "Needs you" })).toBeVisible();
  await expect(page.getByText("demo-approval")).toBeVisible();

  const navigation = page.getByRole("navigation", { name: "Sections" });
  await navigation.getByRole("link", { name: "Deployments", exact: true }).click();
  await expect(page).toHaveURL("/deployments");
  await expect(page.getByText("demo-v1")).toBeVisible();
  await expect(page.getByText("No Deployments yet.")).toHaveCount(0);

  await navigation.getByRole("link", { name: "Sessions", exact: true }).click();
  await expect(page).toHaveURL("/sessions");
  await expect(page.getByRole("link", { name: "demo-actor" }).first()).toBeVisible();
  await expect(page.getByText("No Sessions yet.")).toHaveCount(0);

  await navigation.getByRole("link", { name: "Tokens", exact: true }).click();
  await expect(page).toHaveURL("/tokens");
  await expect(page.getByText("demo-approval")).toBeVisible();
  await expect(page.getByText("No Tokens match this filter.")).toHaveCount(0);

  await navigation.getByRole("link", { name: "Workspaces", exact: true }).click();
  await expect(page).toHaveURL("/workspaces");
  await expect(page.locator("code", { hasText: "demo-actor" })).toBeVisible();
  await expect(page.getByText("No Workspaces yet.")).toHaveCount(0);

  await navigation.getByRole("link", { name: "Runs", exact: true }).click();
  await expect(page).toHaveURL("/runs");
  await expect(page.getByRole("link", { name: "demo-task" }).first()).toBeVisible();
  await expect(page.getByText("No runs match this filter.")).toHaveCount(0);
});

test("console demo session detail shows conversation and execution history", async ({ page }) => {
  await login(page);
  await selectDemoEnvironment(page);
  await page.goto(
    `/sessions/${DEMO_SESSION_OPEN_ID}?project_id=${PROJECT_ID}&environment_id=${DEMO_ENVIRONMENT_ID}`,
  );

  await expect(page.getByRole("heading", { name: "demo-actor" })).toBeVisible();
  await expect(page.getByText("Synthetic demo input")).toBeVisible();
  await expect(page.getByText("Follow-up demo input")).toBeVisible();
  await expect(page.getByText("Synthetic demo output")).toBeVisible();
  await expect(page.getByRole("heading", { name: "Execution history" })).toBeVisible();
  await expect(page.getByRole("link", { name: DEMO_SESSION_OPEN_ID })).toHaveCount(0);
  await expect(page.getByRole("table").getByRole("link", { name: /000000000713/ })).toBeVisible();
});
