import { expect, test, type Page } from "@playwright/test";

const NAVIGATION = ["Overview", "Agents", "Computers", "Deployments"];

// Each section route and the header tab that is current on it.
const SECTION_ROUTES: [string, string][] = [
  ["/", "Overview"],
  ["/agents", "Agents"],
  ["/sessions", "Agents"],
  ["/triggers", "Agents"],
  ["/agents/support", "Agents"],
  ["/agents/support/slack", "Agents"],
  ["/sessions/00000000-0000-7000-8000-000000000001", "Agents"],
  ["/computers", "Computers"],
  ["/templates", "Computers"],
  ["/computers/00000000-0000-7000-8000-000000000002", "Computers"],
  ["/deployments", "Deployments"],
  ["/deployments/00000000-0000-7000-8000-000000000003", "Deployments"],
];

async function login(page: Page) {
  await page.goto("/dev/login");
  await expect(page).toHaveURL("/");
}

test("local developer can enter the console and sees the four sections", async ({ page }) => {
  await login(page);

  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
  const navigation = page.getByRole("navigation", { name: "Sections" });
  await expect(navigation.getByRole("link")).toHaveText(NAVIGATION);
  await expect(page.getByRole("link", { name: "Settings" })).toHaveAttribute("href", "/settings/project");
});

test("an environment before its first Deployment shows the CLI steps for this scope on the Overview", async ({ page }) => {
  await login(page);

  const setup = page.getByRole("region", { name: "Set up Production" });
  await expect(setup.getByRole("heading", { name: "Set up Production" })).toBeVisible();
  const steps = setup.getByRole("list", { name: "First Deployment" });
  await expect(steps.getByRole("listitem")).toHaveCount(3);
  const origin = new URL(page.url()).origin;
  await expect(setup.getByLabel("Install and sign in")).toContainText(`helmr login ${origin}`);
  await expect(setup.getByLabel("Create and deploy")).toHaveText("helmr init --dir ./my-agent\ncd ./my-agent\nbun install\nhelmr deploy . --project console-demo --env production");
  await expect(setup.getByLabel("Start a Session")).toHaveText("helmr session create hello --project console-demo --env production");
  await expect(page.getByText("No current Deployment")).toBeVisible();
  await expect(page.getByRole("region", { name: "Waiting on you" })).toHaveCount(0);
  await expect(page.getByRole("navigation", { name: "Sections" }).getByRole("link", { name: "Overview", exact: true })).toBeVisible();
});

test("the header marks exactly the section that owns each route", async ({ page }) => {
  await login(page);
  const navigation = page.getByRole("navigation", { name: "Sections" });
  for (const [path, section] of SECTION_ROUTES) {
    await page.goto(path);
    await expect(navigation.locator("[aria-current=page]"), path).toHaveText([section]);
  }
});

test("every navigation item resolves to its page", async ({ page }) => {
  await login(page);
  const navigation = page.getByRole("navigation", { name: "Sections" });

  await navigation.getByRole("link", { name: "Deployments", exact: true }).click();
  await expect(page).toHaveURL("/deployments");
  await expect(page.getByRole("heading", { name: "Deployments" })).toBeVisible();
  await expect(page.getByText("No Deployments yet.")).toBeVisible();

  await navigation.getByRole("link", { name: "Agents", exact: true }).click();
  await expect(page).toHaveURL("/agents");
  await expect(page.getByRole("heading", { name: "Agents", exact: true })).toBeVisible();
  await expect(page.getByRole("tab", { name: "Agents", exact: true })).toHaveAttribute("aria-selected", "true");

  await navigation.getByRole("link", { name: "Computers", exact: true }).click();
  await expect(page).toHaveURL("/computers");
  await expect(page.getByRole("heading", { name: "Computers" })).toBeVisible();
  await expect(page.getByText("No Computers yet.")).toBeVisible();

  await navigation.getByRole("link", { name: "Overview", exact: true }).click();
  await expect(page).toHaveURL("/");
});

test("the Agents section switches between Agents, Sessions and Triggers by click and keyboard", async ({ page }) => {
  await login(page);
  await page.goto("/agents");
  const tabs = page.getByRole("tablist", { name: "Agents" });
  await expect(tabs.getByRole("tab")).toHaveText(["Agents", "Sessions", "Triggers"]);

  await tabs.getByRole("tab", { name: "Sessions", exact: true }).click();
  await expect(page).toHaveURL("/sessions");
  await expect(tabs.getByRole("tab", { name: "Sessions", exact: true })).toHaveAttribute("aria-selected", "true");
  await expect(page.getByRole("tabpanel", { name: "Sessions" })).toContainText("No Sessions yet.");

  await tabs.getByRole("tab", { name: "Sessions", exact: true }).focus();
  await page.keyboard.press("ArrowRight");
  await expect(page).toHaveURL("/triggers");
  const triggers = tabs.getByRole("tab", { name: "Triggers", exact: true });
  await expect(triggers).toBeFocused();
  await expect(triggers).toHaveAttribute("aria-selected", "true");
  await expect(triggers).toHaveAttribute("tabindex", "0");
  await expect(page.getByRole("tabpanel", { name: "Triggers" })).toContainText("No Trigger activations in this environment.");

  await page.keyboard.press("Home");
  await expect(page).toHaveURL("/agents");
  await expect(tabs.getByRole("tab", { name: "Agents", exact: true })).toBeFocused();
  await page.keyboard.press("ArrowLeft");
  await expect(page).toHaveURL("/triggers");
  await page.keyboard.press("End");
  await expect(page).toHaveURL("/triggers");
  await expect(triggers).toBeFocused();
});

test("the Computers section switches between Computers and Templates", async ({ page }) => {
  await login(page);
  await page.goto("/computers");
  const tabs = page.getByRole("tablist", { name: "Computers" });
  await expect(tabs.getByRole("tab")).toHaveText(["Computers", "Templates"]);
  await expect(page.getByRole("tabpanel", { name: "Computers" })).toContainText("No Computers yet.");
  await tabs.getByRole("tab", { name: "Templates", exact: true }).click();
  await expect(page).toHaveURL("/templates");
  await expect(page.getByRole("tabpanel", { name: "Templates" })).toContainText("No current Deployment.");
  await page.keyboard.press("ArrowLeft");
  await expect(page).toHaveURL("/computers");
  await expect(tabs.getByRole("tab", { name: "Computers", exact: true })).toBeFocused();
});

test("on a phone the menu button opens a drawer that reaches every section, Settings and the account", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await login(page);
  await expect(page.getByRole("navigation", { name: "Sections" })).toHaveCount(0);
  const destinations: [string, string][] = [["Agents", "/agents"], ["Computers", "/computers"], ["Deployments", "/deployments"], ["Overview", "/"]];
  for (const [name, path] of destinations) {
    await page.getByRole("button", { name: "Open menu" }).click();
    const drawer = page.getByRole("dialog", { name: "Menu" });
    const sections = drawer.getByRole("navigation", { name: "Sections" });
    await expect(sections.getByRole("link")).toHaveText(NAVIGATION);
    for (const link of await sections.getByRole("link").all()) {
      expect((await link.boundingBox())!.height).toBeGreaterThanOrEqual(44);
    }
    await sections.getByRole("link", { name, exact: true }).click();
    await expect(page).toHaveURL(path);
    await expect(drawer).toHaveCount(0);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  }

  await page.getByRole("button", { name: "Open menu" }).click();
  const drawer = page.getByRole("dialog", { name: "Menu" });
  await expect(drawer).toContainText("Project");
  await expect(drawer).toContainText("Environment");
  await expect(drawer.getByRole("button", { name: "Sign out", exact: true })).toBeVisible();
  await expect(drawer.getByRole("link", { name: "Account settings", exact: true })).toHaveAttribute("href", "/settings/account");
  await drawer.getByRole("link", { name: "Settings", exact: true }).click();
  await expect(page).toHaveURL("/settings/project");

  for (const [button, picker, search] of [["Change project", "Choose project", "Find a project"], ["Change environment", "Choose environment", "Find an environment"]] as const) {
    await page.getByRole("button", { name: "Open menu" }).click();
    await page.getByRole("dialog", { name: "Menu" }).getByRole("button", { name: button, exact: true }).click();
    await expect(page.getByRole("dialog", { name: "Menu" })).toHaveCount(0);
    const dialog = page.getByRole("dialog", { name: picker });
    await expect(dialog).toBeVisible();
    await expect(dialog.getByRole("combobox", { name: search })).toBeFocused();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
  }

  await page.getByRole("button", { name: "Open menu" }).click();
  const menu = page.getByRole("dialog", { name: "Menu" });
  await expect(menu.getByRole("button", { name: "Change project", exact: true })).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(menu.getByRole("button", { name: "Change environment", exact: true })).toBeFocused();
  for (const name of NAVIGATION) {
    await page.keyboard.press("Tab");
    await expect(menu.getByRole("navigation", { name: "Sections" }).getByRole("link", { name, exact: true })).toBeFocused();
  }
  await page.keyboard.press("Tab");
  await expect(menu.getByRole("link", { name: "Settings", exact: true })).toBeFocused();
  await page.keyboard.press("Shift+Tab");
  await expect(menu.getByRole("link", { name: "Deployments", exact: true })).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(menu).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Open menu" })).toBeFocused();
});

test("a sub-page path with a trailing slash selects its tab, and an unknown Agent sub-page is not found", async ({ page }) => {
  await login(page);
  await page.goto("/sessions/");
  await expect(page.getByRole("tab", { name: "Sessions", exact: true })).toHaveAttribute("aria-selected", "true");
  await page.goto("/agents/support/no-such-tab");
  await expect(page.getByRole("heading", { name: "Page not found", exact: true })).toBeVisible();
  await expect(page.getByRole("tablist", { name: "Agent" })).toHaveCount(0);
});

test("the Sessions page filters by status and shows the empty state", async ({ page }) => {
  await login(page);
  await page.goto("/sessions");
  await expect(page.getByRole("tab", { name: "Sessions", exact: true })).toHaveAttribute("aria-selected", "true");
  await expect(page.getByText("No Sessions yet.")).toBeVisible();

  const statusSelect = page.getByRole("combobox", { name: "Filter sessions" });
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
  await stagingRow.getByRole("button", { name: "Actions for Staging" }).click();
  await page.getByRole("button", { name: "Edit", exact: true }).click();

  const dialog = page.getByRole("dialog", { name: "Edit Staging" });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByLabel("Environment ID (not editable)")).toBeDisabled();
  await dialog.getByLabel("Name").fill("Staging Renamed");
  await dialog.getByRole("button", { name: "Use #22C55E" }).click();
  await dialog.getByRole("button", { name: "Save" }).click();
  await expect(dialog).toHaveCount(0);

  const renamedRow = page.getByRole("row", { name: /Staging Renamed/ });
  await expect(renamedRow).toBeVisible();
  await expect(renamedRow.getByTitle("staging", { exact: true })).toHaveText("staging");
  await expect(renamedRow.locator("span[style]").first()).toHaveCSS("background-color", "rgb(34, 197, 94)");

  await renamedRow.getByRole("button", { name: "Actions for Staging Renamed" }).click();
  await page.getByRole("button", { name: "Use", exact: true }).click();
  const header = page.locator("header");
  await expect(header).toContainText("Staging Renamed");
  await expect(header.locator("span[style*='background-color']").first()).toHaveCSS("background-color", "rgb(34, 197, 94)");

  await page.reload();
  await expect(page.getByRole("row", { name: /Staging Renamed/ })).toBeVisible();
});
