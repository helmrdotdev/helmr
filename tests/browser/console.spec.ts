import { expect, test, type Page } from "@playwright/test";

const NAVIGATION = ["Overview", "Sessions", "Computers", "Deployments"];

async function login(page: Page) {
  await page.goto("/dev/login");
  await expect(page).toHaveURL("/");
}

test("local developer can enter the console and sees the four sections", async ({ page }) => {
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

  await expect(page.getByRole("heading", { name: "Set up your first Agent" })).toBeVisible();
  await expect(page.getByText("helmr init --dir ./my-agent")).toBeVisible();
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

  await navigation.getByRole("link", { name: "Computers", exact: true }).click();
  await expect(page).toHaveURL("/computers");
  await expect(page.getByRole("heading", { name: "Computers" })).toBeVisible();
  await expect(page.getByText("No Computers yet.")).toBeVisible();

  await navigation.getByRole("link", { name: "Overview", exact: true }).click();
  await expect(page).toHaveURL("/");
});

test("the Sessions page filters by status and shows the empty state", async ({ page }) => {
  await login(page);
  await page.goto("/sessions");
  await expect(page.getByRole("heading", { name: "Sessions" })).toBeVisible();
  await expect(page.getByText("No Sessions yet.")).toBeVisible();

  const statusSelect = page.getByRole("button", { name: "Filter sessions" });
  const cancelledRequest = page.waitForRequest((request) =>
    request.url().includes("/sessions?") && new URL(request.url()).searchParams.get("status") === "cancelled",
  );
  await statusSelect.click();
  await page.getByRole("option", { name: "Cancelled" }).click();
  await cancelledRequest;
  await expect(page.getByText("No Sessions match this filter.")).toBeVisible();
});

test("an environment can be renamed and recolored from Settings and the header dot follows", async ({ page }) => {
  await login(page);

  await page.goto("/settings/environments");
  await expect(page.getByRole("heading", { name: "Environments" })).toBeVisible();
  const stagingRow = page.getByRole("row", { name: /Staging/ });
  await expect(stagingRow).toBeVisible();
  await stagingRow.getByRole("button", { name: "Edit" }).click();

  const dialog = page.getByRole("dialog", { name: "Edit Staging" });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByLabel("Slug (not editable)")).toBeDisabled();
  await dialog.getByLabel("Name").fill("Staging Renamed");
  await dialog.getByRole("button", { name: "Use #22C55E" }).click();
  await dialog.getByRole("button", { name: "Save" }).click();
  await expect(dialog).toHaveCount(0);

  const renamedRow = page.getByRole("row", { name: /Staging Renamed/ });
  await expect(renamedRow).toBeVisible();
  await expect(renamedRow.getByRole("cell", { name: "staging", exact: true })).toBeVisible();
  await expect(renamedRow.locator("span[style]").first()).toHaveCSS("background-color", "rgb(34, 197, 94)");

  await renamedRow.getByRole("button", { name: "Use" }).click();
  const header = page.locator("header");
  await expect(header).toContainText("Staging Renamed");
  await expect(header.locator("span[style*='background-color']").first()).toHaveCSS("background-color", "rgb(34, 197, 94)");

  await page.reload();
  await expect(page.getByRole("row", { name: /Staging Renamed/ })).toBeVisible();
});
