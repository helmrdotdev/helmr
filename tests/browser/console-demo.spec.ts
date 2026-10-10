import { expect, test, type Page } from "@playwright/test";

const DEMO_SESSION_ID = "00000000-0000-7000-8000-000000000701";
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

test("console demo shows retained history in current navigation", async ({ page }) => {
  await login(page);
  await selectDemoEnvironment(page);
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Current Deployment" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Recent Sessions" })).toBeVisible();
  await expect(page.getByRole("link", { name: "000000000701", exact: true })).toBeVisible();
  const navigation = page.getByRole("navigation", { name: "Sections" });
  await navigation.getByRole("link", { name: "Deployments", exact: true }).click();
  await page.getByRole("link", { name: /000000000501/ }).click();
  await expect(page.getByText("demo-agent", { exact: true })).toBeVisible();
  await navigation.getByRole("link", { name: "Sessions", exact: true }).click();
  await expect(page.getByRole("link", { name: "000000000701", exact: true })).toBeVisible();
  await expect(page.getByText("Closed", { exact: true }).first()).toBeVisible();
  await navigation.getByRole("link", { name: "Computers", exact: true }).click();
  await expect(page.getByRole("link", { name: "000000000601", exact: true })).toBeVisible();
});

test("console demo retains failed Turn input and terminal Session history", async ({ page }) => {
  await login(page);
  await page.goto(`/sessions/${DEMO_SESSION_ID}?project_id=${PROJECT_ID}&environment_id=${DEMO_ENVIRONMENT_ID}`);
  await expect(page.getByText("Demo input", { exact: true })).toBeVisible();
  await expect(page.getByText("Closed", { exact: true }).first()).toBeVisible();
  await expect(page.getByText("Failed", { exact: true }).first()).toBeVisible();
  await expect(page.getByText("Send work through the CLI", { exact: false })).toBeVisible();
  await expect(page.getByRole("button", { name: "Cancel Session", exact: true })).toHaveCount(0);
});
