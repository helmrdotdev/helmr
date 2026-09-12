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

test("the Tokens page shows its empty state and no Token actions", async ({ page }) => {
  await login(page);

  await page.goto("/tokens");
  await expect(page.getByRole("heading", { name: "Tokens" })).toBeVisible();
  await expect(page.getByText("No Tokens match this filter.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Complete" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Cancel", exact: true })).toHaveCount(0);
  await expect(page.locator("main")).not.toContainText("callback_url");
  await expect(page.locator("main")).not.toContainText("public_access_token");
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

const PROJECT_ID = "00000000-0000-7000-8000-000000000301";
const ENVIRONMENT_ID = "00000000-0000-7000-8000-000000000401";

async function createToken(page: Page, tag: string): Promise<string> {
  const response = await page.request.post(`/api/projects/${PROJECT_ID}/environments/${ENVIRONMENT_ID}/tokens`, {
    data: { timeout: "1h", tags: [tag], idempotency_key: crypto.randomUUID() },
  });
  expect(response.ok()).toBeTruthy();
  const token = (await response.json()) as { id: string };
  return token.id;
}

test("a pending Token can be completed from its detail page and cancelled from the list", async ({ page }) => {
  await login(page);
  const completeID = await createToken(page, "complete-me");
  const cancelID = await createToken(page, "cancel-me");

  await page.goto(`/tokens/${completeID}`);
  await expect(page.getByRole("heading", { name: "Token", exact: true })).toBeVisible();
  await expect(page.locator("main")).toContainText(completeID);
  await expect(page.locator("main")).not.toContainText("callback_url");
  await expect(page.locator("main")).not.toContainText("token-callbacks");
  await expect(page.locator("main")).not.toContainText("hlmr_pub_");
  await page.getByRole("button", { name: "Complete" }).click();
  const completeDialog = page.getByRole("dialog", { name: "Complete Token" });
  await completeDialog.getByLabel("Result (JSON)").fill('{"approved": true}');
  await completeDialog.getByRole("button", { name: "Complete" }).click();
  await expect(completeDialog).toHaveCount(0);
  await expect(page.getByRole("heading", { name: "Result" })).toBeVisible();
  await expect(page.locator("main")).toContainText('"approved": true');
  await expect(page.getByRole("button", { name: "Complete" })).toHaveCount(0);

  await page.goto("/tokens");
  const cancelRow = page.getByRole("row", { name: /cancel-me/ });
  await expect(cancelRow).toContainText("Pending");
  await cancelRow.getByRole("button", { name: "Cancel" }).click();
  await page.getByRole("dialog", { name: "Cancel Token" }).getByRole("button", { name: "Cancel Token" }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect(cancelRow).toContainText("Cancelled");
  await expect(cancelRow.getByRole("button", { name: "Cancel" })).toHaveCount(0);
  await expect(page.getByRole("row", { name: /complete-me/ })).toContainText("Completed");
  await page.getByRole("row", { name: /cancel-me/ }).getByRole("link").first().click();
  await expect(page).toHaveURL(`/tokens/${cancelID}`);
});
