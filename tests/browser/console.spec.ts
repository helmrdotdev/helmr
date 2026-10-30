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

test("an environment before its first Deployment shows setup on the Overview", async ({ page }) => {
  await login(page);

  const setup = page.getByRole("region", { name: "Set up" });
  await expect(setup.getByRole("list", { name: "Set up" }).getByRole("listitem")).toHaveCount(4);
  await expect(setup.getByRole("radio", { name: "Claude Code" })).toBeChecked();
  await expect(page.getByRole("region", { name: /^Needs attention/ })).toHaveCount(0);
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
  await expect(page.getByText("No computers yet.")).toBeVisible();

  await navigation.getByRole("link", { name: "Overview", exact: true }).click();
  await expect(page).toHaveURL("/");
});

test("the Agents section switches between Agents, Sessions and Triggers by click and keyboard", async ({ page }) => {
  await login(page);
  await page.goto("/agents");
  const tabs = page.getByRole("tablist", { name: "Agents" });
  await expect(tabs.getByRole("tab")).toHaveText(["Agents", "Sessions", "Triggers"]);

  await tabs.getByRole("tab", { name: "Agents", exact: true }).focus();
  await page.keyboard.press("ArrowRight");
  await expect(page).toHaveURL("/sessions");
  await expect(tabs.getByRole("tab", { name: "Sessions", exact: true })).toBeFocused();
  await page.keyboard.press("ArrowRight");
  await expect(page).toHaveURL("/triggers");
  const triggers = tabs.getByRole("tab", { name: "Triggers", exact: true });
  await expect(triggers).toBeFocused();
  await expect(triggers).toHaveAttribute("aria-selected", "true");
  await expect(triggers).toHaveAttribute("tabindex", "0");
  await expect(page.getByRole("tabpanel", { name: "Triggers" })).toContainText("No trigger activations in this environment.");

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
  await expect(page.getByRole("tabpanel", { name: "Computers" })).toContainText("No computers yet.");
  await tabs.getByRole("tab", { name: "Templates", exact: true }).click();
  await expect(page).toHaveURL("/templates");
  await expect(page.getByRole("tabpanel", { name: "Templates" })).toContainText("No current deployment.");
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
  // The bar keeps both scope pickers at phone width, so the drawer has no scope
  // block and no title repeating the product name.
  await expect(drawer.getByRole("heading")).toHaveCount(0);
  await expect(drawer.getByRole("button", { name: /^Change / })).toHaveCount(0);
  await expect(drawer.getByRole("button", { name: "Sign out", exact: true })).toBeVisible();
  await expect(drawer.getByRole("link", { name: "Profile", exact: true })).toHaveAttribute("href", "/settings/account");
  await expect(drawer.getByRole("link", { name: "Switch organization", exact: true })).toHaveAttribute("href", "/organizations");
  await drawer.getByRole("link", { name: "Settings", exact: true }).click();
  await expect(page).toHaveURL("/settings/project");

  for (const [segment, picker, search] of [[/^Project:/, "Choose project", "Find a project"], [/^Environment:/, "Choose environment", "Find an environment"]] as const) {
    const trigger = page.locator("header").getByRole("button", { name: segment });
    await expect(trigger).toBeVisible();
    await trigger.click();
    const dialog = page.getByRole("dialog", { name: picker });
    await expect(dialog).toBeVisible();
    await expect(dialog.getByRole("combobox", { name: search })).toBeFocused();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
  }

  await page.getByRole("button", { name: "Open menu" }).click();
  const menu = page.getByRole("dialog", { name: "Menu" });
  await expect(menu.getByRole("navigation", { name: "Sections" }).getByRole("link", { name: NAVIGATION[0], exact: true })).toBeFocused();
  for (const name of NAVIGATION.slice(1)) {
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
  await page.goto("/triggers/");
  await expect(page.getByRole("tab", { name: "Triggers", exact: true })).toHaveAttribute("aria-selected", "true");
  await page.goto("/agents/support/no-such-tab");
  await expect(page.getByRole("heading", { name: "Page not found", exact: true })).toBeVisible();
  await expect(page.getByRole("tablist", { name: "Agent" })).toHaveCount(0);
});

test("the Sessions page filters by status and shows the empty state", async ({ page }) => {
  await login(page);
  await page.goto("/sessions");
  await expect(page.getByRole("tab", { name: "Sessions", exact: true })).toHaveAttribute("aria-selected", "true");
  await expect(page.getByRole("navigation", { name: "Sections" }).locator("[aria-current=page]")).toHaveText(["Agents"]);
  await expect(page.getByText("No sessions yet.")).toBeVisible();

  const statusSelect = page.getByRole("combobox", { name: "Filter by status" });
  const cancelledRequest = page.waitForRequest((request) =>
    request.url().includes("/sessions?") && new URL(request.url()).searchParams.get("status") === "cancelled",
  );
  await statusSelect.click();
  await page.getByRole("option", { name: "Cancelled" }).click();
  await cancelledRequest;
  await expect(page.getByText("No sessions match this filter.")).toBeVisible();
});

test("an environment can be renamed and recolored from Settings and the header dot follows", async ({ page }) => {
  await login(page);

  await page.goto("/settings/environments");
  await expect(page.getByRole("heading", { name: "Environments" })).toBeVisible();
  const stagingRow = page.getByRole("row", { name: /Staging/ });
  await expect(stagingRow).toBeVisible();
  await stagingRow.getByRole("button", { name: "Actions for Staging" }).click();
  await page.getByRole("menuitem", { name: "Edit", exact: true }).click();

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
  await page.getByRole("menuitem", { name: "Use", exact: true }).click();
  const header = page.locator("header");
  await expect(header).toContainText("Staging Renamed");
  await expect(header.locator("span[style*='background-color']").first()).toHaveCSS("background-color", "rgb(34, 197, 94)");

  await page.reload();
  await expect(page.getByRole("row", { name: /Staging Renamed/ })).toBeVisible();
});
