import { expect, test, type BrowserContext, type Page } from "@playwright/test";

const project = "scope-tabs";
const sessionID = "00000000-0000-7000-8000-000000000901";
const now = "2026-10-12T00:00:00Z";
const environment = (projectID: string, id: string, name: string, isDefault: boolean) =>
  ({ environment: id, project: projectID, name, color_hex: "#1f7a4d", is_default: isDefault, created_at: now, updated_at: now });
const projects = [
  // The organization default, so a fallback to it is distinguishable from a fallback to the tab's own scope.
  { project: "other", name: "Other", is_default: true, environments: [environment("other", "production", "Other Production", true)], created_at: now, updated_at: now },
  { project, name: "Scope Tabs", is_default: false, environments: [environment(project, "production", "Production", true), environment(project, "staging", "Staging", false)], created_at: now, updated_at: now },
];

// Records every Environment-scoped request as `<project>/<environment><path>`.
async function mockConsole(context: BrowserContext) {
  const scoped: string[] = [];
  await context.route("**/api/**", async route => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    const match = path.match(/^\/api\/projects\/([^/]+)\/environments\/([^/]+)(\/.*)?$/);
    const detail = projects.find(candidate => path === `/api/projects/${candidate.project}`);
    let json: unknown;
    if (match) scoped.push(`${match[1]}/${match[2]}${match[3] ?? ""}`);
    if (path === "/api/me") json = { user_id: "user", display_name: "User", org_id: "org", role: "owner", permissions: ["sessions.read"], organization_required: false, project_required: false };
    else if (path === "/api/projects") json = { projects };
    else if (detail) json = detail;
    else if (match?.[3] === "/sessions") json = { sessions: [] };
    else if (match?.[3] === "/agents/support/status" && match[2] === "production") json = { agent: "support", current_deployment_id: null, defined_in_current: false };
    else if (match?.[3] === "/triggers") json = { triggers: [] };
    else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } }); return; }
    await route.fulfill({ json });
  });
  return scoped;
}

function environmentTrigger(page: Page) {
  return page.locator("header").getByRole("button", { name: /^Environment:/ });
}

function projectTrigger(page: Page) {
  return page.locator("header").getByRole("button", { name: /^Project:/ });
}

function notice(page: Page) {
  return page.getByRole("status");
}

async function chooseEnvironment(page: Page, name: string) {
  await environmentTrigger(page).click();
  await page.getByRole("dialog", { name: "Choose environment" }).getByRole("option", { name }).click();
}

async function openLink(page: Page, search: string, expected: string) {
  await page.goto(`/sessions?${search}`);
  await expect(page).toHaveURL("/sessions");
  await expect(environmentTrigger(page)).toContainText(expected);
}

test("each tab keeps its own Environment across reloads", async ({ context }) => {
  await mockConsole(context);
  const first = await context.newPage();
  await openLink(first, `project=${project}&environment=production`, "Production");

  const second = await context.newPage();
  await second.goto("/sessions");
  await expect(environmentTrigger(second)).toContainText("Production");
  await chooseEnvironment(second, "Staging");
  await expect(environmentTrigger(second)).toContainText("Staging");
  await expect(second).toHaveURL("/sessions");

  await first.reload();
  await second.reload();
  await expect(environmentTrigger(first)).toContainText("Production");
  await expect(environmentTrigger(second)).toContainText("Staging");
  await expect(notice(first)).toHaveText("");
  await expect(notice(second)).toHaveText("");
});

test("a new tab starts in the scope of the tab last in front", async ({ context }) => {
  const scoped = await mockConsole(context);
  const first = await context.newPage();
  await openLink(first, `project=${project}&environment=production`, "Production");
  const second = await context.newPage();
  await second.goto("/sessions");
  await chooseEnvironment(second, "Staging");
  await expect(environmentTrigger(second)).toContainText("Staging");

  // Headless Chromium keeps every page visible and focused and fires no event on
  // bringToFront, so deliver the focus event a real tab switch would.
  await first.bringToFront();
  await first.evaluate(() => window.dispatchEvent(new Event("focus")));
  const opened = await context.newPage();
  await opened.goto(`/sessions/${sessionID}`);
  await expect(environmentTrigger(opened)).toContainText("Production");
  expect(scoped).toContain(`${project}/production/sessions/${sessionID}`);
  expect(scoped).not.toContain(`${project}/staging/sessions/${sessionID}`);
});

test("a share link switches the tab without touching its old scope, leaves a clean address and announces the change", async ({ page }) => {
  const scoped = await mockConsole(page.context());
  await openLink(page, `project=${project}&environment=production`, "Production");
  await expect(notice(page)).toHaveText("");

  scoped.length = 0;
  await page.goto(`/sessions?ref=share&project=${project}&environment=staging#history`);
  await expect(page).toHaveURL("/sessions?ref=share#history");
  await expect(notice(page)).toHaveText("Switched to Scope Tabs / Staging from the link.");
  await expect(environmentTrigger(page)).toContainText("Staging");
  expect(scoped).toContain(`${project}/staging/sessions`);
  expect(scoped.filter(path => path.startsWith(`${project}/production`))).toEqual([]);
  await notice(page).getByRole("button", { name: "Dismiss notice" }).click();
  await expect(notice(page)).toHaveText("");

  await openLink(page, `project=${project}&environment=staging`, "Staging");
  await expect(notice(page)).toHaveText("");
});

test("a link to an unavailable Project returns the tab to its own scope and says so", async ({ page }) => {
  const scoped = await mockConsole(page.context());
  await openLink(page, `project=${project}&environment=staging`, "Staging");
  await openLink(page, "project=missing&environment=production", "Staging");
  await expect(projectTrigger(page)).toContainText("Scope Tabs");
  await expect(notice(page)).toHaveText("The link's Project or Environment is not available; showing Scope Tabs / Staging.");
  // The organization default Project is never shown on the way back.
  expect(scoped.filter(path => path.startsWith("other/"))).toEqual([]);
});

test("a link to an unavailable Environment returns the tab to its own scope and says so", async ({ page }) => {
  const scoped = await mockConsole(page.context());
  await openLink(page, "project=other&environment=production", "Other Production");
  await openLink(page, `project=${project}&environment=missing`, "Other Production");
  await expect(notice(page)).toHaveText("The link's Project or Environment is not available; showing Other / Other Production.");
  // Only the linked Environment itself was tried; the linked Project's default never loads.
  expect(scoped.filter(path => path.startsWith(`${project}/`) && !path.startsWith(`${project}/missing`))).toEqual([]);
});

test("a first link to an unavailable Environment shows the Project's default Environment and says so", async ({ page }) => {
  await mockConsole(page.context());
  await openLink(page, `project=${project}&environment=missing`, "Production");
  await expect(notice(page)).toHaveText("The link's Project or Environment is not available; showing Scope Tabs / Production.");
});

test("a partial link keeps the tab's scope and says so", async ({ page }) => {
  await mockConsole(page.context());
  await openLink(page, `project=${project}&environment=staging`, "Staging");
  await openLink(page, "project=other", "Staging");
  await expect(notice(page)).toHaveText("The link's Project or Environment is not available; showing Scope Tabs / Staging.");
});

test("Copy link shares the page in this tab's scope and an Agent page survives an Environment switch", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await mockConsole(context);
  await page.goto(`/agents/support/triggers?project=${project}&environment=production`);
  await expect(page).toHaveURL("/agents/support/triggers");
  await expect(page.getByRole("tab", { name: "Triggers", exact: true })).toHaveAttribute("aria-selected", "true");
  await page.getByRole("button", { name: "Copy link" }).click();
  await expect(page.getByRole("button", { name: "Copied" })).toBeVisible();
  await expect(page.getByText("Link copied")).toBeAttached();
  const origin = new URL(page.url()).origin;
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(`${origin}/agents/support/triggers?project=${project}&environment=production`);

  await chooseEnvironment(page, "Staging");
  await expect(page).toHaveURL("/agents/support/triggers");
  await expect(page.getByText("This environment has no Agent with this name.")).toBeVisible();
  await chooseEnvironment(page, "Production");
  await expect(page.getByRole("tab", { name: "Triggers", exact: true })).toHaveAttribute("aria-selected", "true");
});

test("Copy link reports a clipboard failure", async ({ page }) => {
  await mockConsole(page.context());
  await page.addInitScript(() => {
    Object.defineProperty(navigator, "clipboard", { value: { writeText: () => Promise.reject(new Error("denied")) } });
  });
  await page.goto(`/agents/support?project=${project}&environment=production`);
  await page.getByRole("button", { name: "Copy link" }).click();
  await expect(page.getByRole("button", { name: "Could not copy" })).toBeVisible();
  await expect(page.getByText("Could not copy link")).toBeAttached();
});

test("the Environment picker checks the current Environment and moves, searches and chooses by keyboard", async ({ page }) => {
  await mockConsole(page.context());
  await openLink(page, `project=${project}&environment=production`, "Production");
  await environmentTrigger(page).click();
  const picker = page.getByRole("dialog", { name: "Choose environment" });
  const search = picker.getByRole("combobox", { name: "Find an environment" });
  await expect(search).toBeFocused();
  const production = picker.getByRole("option", { name: /^Production/ });
  const staging = picker.getByRole("option", { name: /^Staging/ });
  await expect(picker.getByRole("option")).toHaveCount(2);
  await expect(production).toHaveAttribute("aria-checked", "true");
  await expect(staging).not.toHaveAttribute("aria-checked", "true");
  await expect(production).toHaveAttribute("aria-selected", "true");
  await page.keyboard.press("ArrowDown");
  await expect(staging).toHaveAttribute("aria-selected", "true");
  await expect(search).toHaveAttribute("aria-activedescendant", (await staging.getAttribute("id"))!);
  await page.keyboard.press("Home");
  await expect(production).toHaveAttribute("aria-selected", "true");
  await page.keyboard.press("End");
  await expect(staging).toHaveAttribute("aria-selected", "true");
  await page.keyboard.press("Enter");
  await expect(picker).toHaveCount(0);
  await expect(environmentTrigger(page)).toContainText("Staging");
  await expect(environmentTrigger(page)).toBeFocused();
  await expect(page).toHaveURL("/sessions");

  await page.keyboard.press("Enter");
  await expect(search).toBeFocused();
  await page.keyboard.type("prod");
  await expect(picker.getByRole("option")).toHaveCount(1);
  // With text in the box, Home moves the text cursor rather than the list.
  await page.keyboard.press("Home");
  expect(await search.evaluate((input: HTMLInputElement) => input.selectionStart)).toBe(0);
  await expect(picker.getByRole("option")).toHaveAttribute("aria-selected", "true");
  await page.keyboard.press("Enter");
  await expect(environmentTrigger(page)).toContainText("Production");
});

test("the Project picker switches Project by search and its Manage links lead to Settings", async ({ page }) => {
  await mockConsole(page.context());
  await openLink(page, `project=${project}&environment=staging`, "Staging");
  await projectTrigger(page).click();
  const picker = page.getByRole("dialog", { name: "Choose project" });
  await expect(picker.getByRole("combobox", { name: "Find a project" })).toBeFocused();
  await expect(picker.getByRole("option", { name: "Scope Tabs" })).toHaveAttribute("aria-checked", "true");
  await expect(picker.getByRole("link", { name: "Manage", exact: true })).toHaveAttribute("href", "/settings/project");
  await page.keyboard.type("oth");
  await expect(picker.getByRole("option")).toHaveText([/^Other/]);
  await page.keyboard.press("Enter");
  await expect(projectTrigger(page)).toContainText("Other");
  await expect(environmentTrigger(page)).toContainText("Other Production");

  await environmentTrigger(page).click();
  const environments = page.getByRole("dialog", { name: "Choose environment" });
  await expect(environments).toContainText("Environment · Other");
  await environments.getByRole("link", { name: "Manage", exact: true }).click();
  await expect(page).toHaveURL("/settings/environments");
  await expect(environments).toHaveCount(0);
});

test("the Project picker loads the remaining pages so its search covers every Project", async ({ page }) => {
  await mockConsole(page.context());
  const cursors: (string | null)[] = [];
  await page.context().route((url) => url.pathname === "/api/projects", async (route) => {
    const cursor = new URL(route.request().url()).searchParams.get("cursor");
    cursors.push(cursor);
    await route.fulfill({ json: cursor === "page-2" ? { projects: [projects[1]] } : { projects: [projects[0]], next_cursor: "page-2" } });
  });
  await openLink(page, "project=other&environment=production", "Other Production");
  await projectTrigger(page).click();
  const picker = page.getByRole("dialog", { name: "Choose project" });
  await expect(picker.getByRole("option")).toHaveText([/^Other/, /^Scope Tabs/]);
  await expect(picker.getByRole("combobox", { name: "Find a project" })).toBeVisible();
  await expect(picker.getByRole("button", { name: "Load more projects" })).toHaveCount(0);
  expect(cursors).toEqual([null, "page-2"]);
  await page.keyboard.type("scope");
  await page.keyboard.press("Enter");
  await expect(projectTrigger(page)).toContainText("Scope Tabs");
});
