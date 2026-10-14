import { expect, test, type Page } from "@playwright/test";

// The command palette: opening by shortcut and button, keyboard choice across
// Environments, pages, Agents and Sessions, IME safety and focus return.

const project = "palette";
const sessionID = "00000000-0000-7000-8000-000000000931";
const now = "2026-10-14T00:00:00Z";
const environment = (projectID: string, id: string, name: string, isDefault: boolean) =>
  ({ environment: id, project: projectID, name, color_hex: "#1f7a4d", is_default: isDefault, created_at: now, updated_at: now });
const projects = [
  { project, name: "Palette", is_default: true, environments: [environment(project, "production", "Production", true), environment(project, "staging", "Staging", false)], created_at: now, updated_at: now },
  { project: "other", name: "Other", is_default: false, environments: [environment("other", "sandbox", "Sandbox", true)], created_at: now, updated_at: now },
];

async function mockConsole(page: Page) {
  await page.context().route("**/api/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    const match = path.match(/^\/api\/projects\/([^/]+)\/environments\/([^/]+)(\/.*)?$/);
    const detail = projects.find((candidate) => path === `/api/projects/${candidate.project}`);
    let json: unknown;
    if (path === "/api/me") json = { user_id: "user", display_name: "User", org_id: "org", role: "developer", permissions: ["sessions.read"], organization_required: false, project_required: false };
    else if (path === "/api/projects") json = { projects };
    else if (detail) json = detail;
    else if (match?.[3] === "/sessions") json = { sessions: [{ id: sessionID, agent: "support", status: "open", holds: [], created_at: now }] };
    else if (match?.[3] === "/agents") json = { agents: [{ agent: "support", current_deployment_id: null, defined_in_current: false }] };
    else if (match?.[3] === "/attention") json = { items: [], counts: { question: 0, hold: 0, failed_turn: 0 } };
    else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } }); return; }
    await route.fulfill({ json });
  });
}

const palette = (page: Page) => page.getByRole("dialog", { name: "Command palette" });
const search = (page: Page) => palette(page).getByRole("combobox", { name: "Search" });
const environmentTrigger = (page: Page) => page.locator("header").getByRole("button", { name: /^Environment:/ });
const projectTrigger = (page: Page) => page.locator("header").getByRole("button", { name: /^Project:/ });

async function open(page: Page, path = "/sessions") {
  await mockConsole(page);
  await page.goto(`${path}?project=${project}&environment=production`);
  await expect(environmentTrigger(page)).toContainText("Production");
}

// A key press as an input method delivers it while composing text.
async function composingKey(page: Page, target: "window" | "search", init: KeyboardEventInit) {
  if (target === "window") {
    await page.evaluate((init) => document.body.dispatchEvent(new KeyboardEvent("keydown", { ...init, isComposing: true, bubbles: true, cancelable: true })), init);
  } else {
    await search(page).evaluate((input, init) => input.dispatchEvent(new KeyboardEvent("keydown", { ...init, isComposing: true, bubbles: true, cancelable: true })), init);
  }
}

test("the shortcut and the Search button open the palette, Escape closes it and focus returns", async ({ page }) => {
  await open(page);
  const settings = page.getByRole("link", { name: "Settings", exact: true });
  await settings.focus();
  await page.keyboard.press("ControlOrMeta+k");
  await expect(palette(page)).toBeVisible();
  await expect(search(page)).toBeFocused();
  await expect(palette(page).getByRole("group", { name: "Switch environment" })).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(palette(page)).toHaveCount(0);
  await expect(settings).toBeFocused();

  const button = page.getByRole("button", { name: "Search", exact: true });
  await button.click();
  await expect(search(page)).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(button).toBeFocused();
});

test("typing in an input never opens the palette without the modifier, and composing text never does", async ({ page }) => {
  await open(page, "/agents");
  const filter = page.getByLabel("Filter agents", { exact: true });
  await filter.press("k");
  await expect(filter).toHaveValue("k");
  await expect(palette(page)).toHaveCount(0);
  await composingKey(page, "window", { key: "k", ctrlKey: true, metaKey: true });
  await expect(palette(page)).toHaveCount(0);
  await filter.press("ControlOrMeta+k");
  await expect(palette(page)).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(filter).toBeFocused();
});

test("the palette switches Environment and Project by keyboard", async ({ page }) => {
  await open(page);
  await page.keyboard.press("ControlOrMeta+k");
  await expect(search(page)).toBeFocused();
  await page.keyboard.type("stag");
  const options = palette(page).getByRole("option");
  await expect(options.first()).toHaveText(/^Staging/);
  await expect(options.first()).toHaveAttribute("aria-selected", "true");
  // An input method's Enter confirms its text, never the selection.
  await composingKey(page, "search", { key: "Enter" });
  await expect(palette(page)).toBeVisible();
  await page.keyboard.press("Enter");
  await expect(palette(page)).toHaveCount(0);
  await expect(environmentTrigger(page)).toContainText("Staging");
  await expect(page).toHaveURL("/sessions");

  await page.keyboard.press("ControlOrMeta+k");
  await expect(search(page)).toBeFocused();
  await page.keyboard.type("other");
  await expect(palette(page).getByRole("option", { name: /^Other/ })).toContainText("last used environment");
  await page.keyboard.press("Enter");
  await expect(projectTrigger(page)).toContainText("Other");
  await expect(environmentTrigger(page)).toContainText("Sandbox");
});

test("the palette goes to pages, Agents and Sessions", async ({ page }) => {
  await open(page);
  await page.keyboard.press("ControlOrMeta+k");
  await expect(search(page)).toBeFocused();
  await page.keyboard.type("deployments");
  await expect(palette(page).getByRole("group", { name: "Go to" }).getByRole("option")).toHaveText(["Deployments"]);
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL("/deployments");

  await page.keyboard.press("ControlOrMeta+k");
  await expect(search(page)).toBeFocused();
  await page.keyboard.type("support");
  const agents = palette(page).getByRole("group", { name: "Agents" });
  await expect(agents.getByRole("option")).toHaveText(["support"]);
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("ArrowUp");
  await expect(agents.getByRole("option")).toHaveAttribute("aria-selected", "true");
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL("/agents/support");

  await page.keyboard.press("ControlOrMeta+k");
  await expect(search(page)).toBeFocused();
  await page.keyboard.type("000000000931");
  const sessions = palette(page).getByRole("group", { name: "Sessions" });
  await expect(sessions.getByRole("option")).toHaveText(["000000000931 · support"]);
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(`/sessions/${sessionID}`);
});

test("the shortcut does nothing while another modal dialog is open", async ({ page }) => {
  await open(page);
  await projectTrigger(page).click();
  await page.getByRole("dialog", { name: "Choose project" }).getByRole("button", { name: "New project", exact: true }).click();
  const create = page.getByRole("dialog", { name: "New project" });
  await expect(create).toBeVisible();
  await page.keyboard.press("ControlOrMeta+k");
  await expect(palette(page)).toHaveCount(0);
  await expect(create).toBeVisible();
});
