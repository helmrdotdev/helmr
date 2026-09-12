import { expect, test, type Page } from "@playwright/test";

const NAVIGATION = ["Overview", "Runs", "Sessions", "Tokens", "Workspaces", "Deployments"];

async function login(page: Page) {
  await page.goto("/dev/login");
  await expect(page).toHaveURL("/");
}

test("local developer can enter the console and sees the six sections", async ({ page }) => {
  await login(page);

  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
  const navigation = page.getByRole("navigation", { name: "Sections" });
  for (const name of NAVIGATION) {
    await expect(navigation.getByRole("link", { name, exact: true })).toBeVisible();
  }
  await expect(navigation.getByRole("link", { name: "Tasks", exact: true })).toHaveCount(0);
  await expect(navigation.getByRole("link", { name: "Schedules", exact: true })).toHaveCount(0);
  await expect(navigation.getByRole("link", { name: "Dashboard", exact: true })).toHaveCount(0);
  await expect(page.getByRole("link", { name: "Settings" })).toHaveAttribute("href", "/settings/projects");
});

test("an environment without a deployment shows the CLI onboarding on the Overview", async ({ page }) => {
  await login(page);

  await expect(page.getByRole("heading", { name: "Set up your first task" })).toBeVisible();
  await expect(page.getByText("helmr deploy ./my-helmr-tasks")).toBeVisible();
  await expect(page.getByRole("heading", { name: "Needs you" })).toHaveCount(0);
});

test("every navigation item resolves to its page", async ({ page }) => {
  await login(page);
  const navigation = page.getByRole("navigation", { name: "Sections" });

  await navigation.getByRole("link", { name: "Deployments", exact: true }).click();
  await expect(page).toHaveURL("/deployments");
  await expect(page.getByRole("heading", { name: "Deployments" })).toBeVisible();
  await expect(page.getByText("No Deployments yet.")).toBeVisible();

  await navigation.getByRole("link", { name: "Sessions", exact: true }).click();
  await expect(page).toHaveURL("/sessions");
  await expect(page.getByRole("heading", { name: "Sessions" })).toBeVisible();
  await expect(page.getByText("No Sessions yet.")).toBeVisible();

  await navigation.getByRole("link", { name: "Tokens", exact: true }).click();
  await expect(page).toHaveURL("/tokens");
  await expect(page.getByRole("heading", { name: "Tokens" })).toBeVisible();
  await expect(page.getByText("No Tokens match this filter.")).toBeVisible();

  await navigation.getByRole("link", { name: "Workspaces", exact: true }).click();
  await expect(page).toHaveURL("/workspaces");
  await expect(page.getByRole("heading", { name: "Workspaces" })).toBeVisible();
  await expect(page.getByText("No Workspaces yet.")).toBeVisible();

  await navigation.getByRole("link", { name: "Runs", exact: true }).click();
  await expect(page).toHaveURL("/runs");
  await expect(page.getByRole("heading", { name: "Runs" })).toBeVisible();

  await navigation.getByRole("link", { name: "Overview", exact: true }).click();
  await expect(page).toHaveURL("/");
});

test("the Runs ledger filters by kind through the server", async ({ page }) => {
  await login(page);
  await page.goto("/runs");
  await expect(page.getByRole("heading", { name: "Runs" })).toBeVisible();
  await expect(page.getByText("No runs match this filter.")).toBeVisible();

  const kindSelect = page.getByRole("button", { name: "Filter runs by kind" });
  await expect(kindSelect).toHaveText(/All kinds/);
  const actorRequest = page.waitForRequest((request) =>
    request.url().includes("/runs?") && new URL(request.url()).searchParams.get("kind") === "actor",
  );
  await kindSelect.click();
  await page.getByRole("option", { name: "Actor" }).click();
  await actorRequest;
  await expect(kindSelect).toHaveText(/Actor/);
  await expect(page.getByText("No runs match this filter.")).toBeVisible();
});

test("the Sessions page filters by status and shows the empty state", async ({ page }) => {
  await login(page);
  await page.goto("/sessions");
  await expect(page.getByRole("heading", { name: "Sessions" })).toBeVisible();
  await expect(page.getByText("No Sessions yet.")).toBeVisible();

  const statusSelect = page.getByRole("button", { name: "Filter sessions" });
  const failedRequest = page.waitForRequest((request) =>
    request.url().includes("/sessions?") && new URL(request.url()).searchParams.get("status") === "failed",
  );
  await statusSelect.click();
  await page.getByRole("option", { name: "Failed" }).click();
  await failedRequest;
  await expect(page.getByText("No Sessions match this filter.")).toBeVisible();
});
