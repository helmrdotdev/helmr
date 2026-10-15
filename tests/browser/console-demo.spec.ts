import { expect, test, type Page } from "@playwright/test";

const DEMO_SESSION_ID = "00000000-0000-7000-8000-000000000701";
const PROJECT_ID = "console-demo";
const DEMO_ENVIRONMENT_ID = "demo";

async function login(page: Page) {
  await page.goto("/dev/login");
  await expect(page).toHaveURL("/");
}

async function selectDemoEnvironment(page: Page) {
  await page.goto("/settings/environments");
  const demoRow = page.getByRole("row").filter({ hasText: "Demo" });
  await demoRow.getByRole("button", { name: "Actions for Demo", exact: true }).click();
  await page.getByRole("menuitem", { name: "Use", exact: true }).click();
  await expect(demoRow.getByText("Working", { exact: true })).toBeVisible();
}

test("console demo shows its waiting work on the Overview and retained history in current navigation", async ({ page }) => {
  await login(page);
  await selectDemoEnvironment(page);
  await page.goto("/");
  await expect(page.getByRole("link", { name: "000000000501", exact: true })).toHaveAttribute("href", "/deployments/00000000-0000-7000-8000-000000000501");
  await expect(page.getByText(/Promoted 3 days ago/)).toBeVisible();
  // The seed holds one Session for approval and has one recent failure in an
  // open Session; closed history and the older failure wait on no one.
  const items = page.getByRole("list", { name: "Waiting items" }).getByRole("listitem");
  await expect(items).toHaveCount(2);
  await expect(items.nth(0)).toHaveAttribute("aria-label", "Held: Waiting for approval before changing the demo billing plan");
  await expect(items.nth(0).getByRole("link", { name: "Review", exact: true })).toHaveAttribute("href", "/sessions/00000000-0000-7000-8000-000000000702");
  await expect(items.nth(0).getByRole("link", { name: "demo-triage", exact: true })).toBeVisible();
  await expect(items.nth(1)).toHaveAttribute("aria-label", "Failed: Turn #1 failed: tool_error");
  await expect(items.nth(1).getByRole("link", { name: "Inspect", exact: true })).toHaveAttribute("href", "/sessions/00000000-0000-7000-8000-000000000703#turn-00000000-0000-7000-8000-000000000713");
  await expect(page.getByRole("group", { name: "Filter by kind" }).getByRole("button")).toHaveText(["All 2", "Questions 0", "Held 1", "Failed 1"]);
  await expect(page.getByRole("navigation", { name: "Sections" }).getByRole("link", { name: /^Overview/ })).toHaveAccessibleName("Overview, 2 waiting");
  await expect(page.getByRole("region", { name: /^Set up/ })).toHaveCount(0);

  const navigation = page.getByRole("navigation", { name: "Sections" });
  await navigation.getByRole("link", { name: "Deployments", exact: true }).click();
  await page.getByRole("link", { name: /000000000501/ }).click();
  await expect(page.getByText("demo-agent", { exact: true })).toBeVisible();
  await navigation.getByRole("link", { name: "Agents", exact: true }).click();
  for (const agent of ["demo-agent", "demo-reporter", "demo-triage"]) {
    await expect(page.getByRole("link", { name: agent, exact: true })).toBeVisible();
  }
  await page.getByRole("tab", { name: "Triggers", exact: true }).click();
  await expect(page.getByRole("tabpanel", { name: "Triggers" })).toContainText("daily");
  await navigation.getByRole("link", { name: "Sessions", exact: true }).click();
  await expect(page).toHaveURL("/sessions");
  for (const id of ["000000000701", "000000000702", "000000000703", "000000000704"]) {
    await expect(page.getByRole("link", { name: id, exact: true })).toBeVisible();
  }
  await expect(page.getByText("Closed", { exact: true }).first()).toBeVisible();
  await navigation.getByRole("link", { name: "Computers", exact: true }).click();
  await expect(page.getByRole("link", { name: "000000000601", exact: true })).toBeVisible();
  await page.getByRole("tab", { name: "Templates", exact: true }).click();
  await expect(page.getByRole("tabpanel", { name: "Templates" })).toContainText("demo-computer");
});

test("an Agent's Slack tab explains that this installation has no Slack integration", async ({ page }) => {
  await login(page);
  await page.goto(`/agents/demo-agent/slack?project=${PROJECT_ID}&environment=${DEMO_ENVIRONMENT_ID}`);
  await expect(page.getByText("Slack is not set up for this Helmr installation.")).toBeVisible();
  await expect(page.getByText(/The operator enables it on the control plane/)).toBeVisible();
  await expect(page.getByText("service is unavailable")).toBeHidden();
  await expect(page.getByRole("button", { name: "Retry" })).toHaveCount(0);
  await page.getByText("Technical details").click();
  await expect(page.getByText(/^service_unavailable: /)).toBeVisible();
});

test("console demo retains failed Turn input and terminal Session history", async ({ page }) => {
  await login(page);
  await page.goto(`/sessions/${DEMO_SESSION_ID}?project=${PROJECT_ID}&environment=${DEMO_ENVIRONMENT_ID}`);
  await expect(page.getByText("Demo input", { exact: true }).first()).toBeVisible();
  await expect(page.getByText("Closed", { exact: true }).first()).toBeVisible();
  await expect(page.getByRole("region", { name: "Log" }).getByText("failed", { exact: true }).first()).toBeVisible();
  // A closed Session takes no more work, so the page offers no CLI hint.
  await expect(page.getByText(/from the CLI/)).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Cancel session", exact: true })).toHaveCount(0);
});
