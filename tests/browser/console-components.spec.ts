import { expect, test, type Page } from "@playwright/test";

// Keyboard and focus behavior of the shared Console parts (popovers, menus,
// selects, dialogs and tabs) against a synthetic API fixture.

const projectID = "console-demo";
const environmentID = "production";
const deploymentID = "00000000-0000-7000-8000-000000000501";
const secretID = "SYNTHETIC_TOKEN";
const base = `/api/projects/${projectID}/environments/${environmentID}`;
const now = "2026-10-14T00:00:00Z";
const cities = ["Amsterdam", "Berlin", "Chicago", "Dublin", "Frankfurt", "London", "Madrid", "Oslo", "Paris", "Seoul", "Sydney", "Tokyo"];

async function fixture(page: Page) {
  const environment = { environment: environmentID, project: projectID, name: "Production", color_hex: "#008000", is_default: true, created_at: now, updated_at: now };
  const project = { project: projectID, name: "Test", default_region_id: "amsterdam", is_default: true, environments: [environment], created_at: now, updated_at: now };
  const deployment = { id: deploymentID, version: "test-version", bundle_digest: `sha256:${"b".repeat(64)}`, created_at: now };
  await page.route("**/api/**", async route => {
    const path = new URL(route.request().url()).pathname;
    let json: unknown;
    if (path === "/api/me") json = { user_id: "local", display_name: "Local", org_id: "org", role: "owner", permissions: ["projects.manage", "secrets.read", "secrets.write", "secrets.revoke", "deployments.read"], admin: false, organization_required: false, project_required: false };
    else if (path === "/api/projects") json = { projects: [project] };
    else if (path === `/api/projects/${projectID}`) json = project;
    else if (path === `/api/projects/${projectID}/settings`) json = { history_retention: "30d" };
    else if (path === base) json = environment;
    else if (path === "/api/regions") json = { regions: cities.map(city => ({ id: city.toLowerCase(), display_name: city })) };
    else if (path === `${base}/secrets`) json = { secrets: [{ secret: secretID, status: "active", created_at: now }] };
    else if (path === `${base}/deployments/${deploymentID}`) json = deployment;
    else if (path === `${base}/deployments/current`) json = deployment;
    else if (path === `${base}/agents`) json = { deployment_id: deploymentID, agents: [{ agent: "assistant", template: "browser" }] };
    else if (path === `${base}/templates`) json = { deployment_id: deploymentID, templates: [{ template: "browser" }] };
    else if (path.endsWith("/events")) json = { events: [] };
    else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } }); return; }
    await route.fulfill({ json });
  });
}

const scope = `project=${projectID}&environment=${environmentID}`;

async function openNewProject(page: Page) {
  await page.goto(`/settings/secrets?${scope}`);
  await page.getByRole("button", { name: "Project Test / Env Production" }).click();
  await page.getByRole("dialog", { name: "Choose project" }).getByRole("button", { name: "New project", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "New project" });
  await expect(dialog).toBeVisible();
  return dialog;
}

test("Escape closes a Select inside a dialog before the dialog, and the dialog returns focus to its opener", async ({ page }) => {
  await fixture(page);
  const dialog = await openNewProject(page);
  const region = dialog.getByRole("combobox", { name: "Region", exact: true });
  await region.click();
  await expect(page.getByRole("listbox", { name: "Region" })).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("listbox")).toHaveCount(0);
  await expect(dialog).toBeVisible();
  await expect(region).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Project Test / Env Production" })).toBeFocused();
});

test("a dialog keeps Tab inside it, and Tab out of a Select returns to its trigger", async ({ page }) => {
  await fixture(page);
  const dialog = await openNewProject(page);
  const name = dialog.getByLabel("Name", { exact: true });
  const close = dialog.getByRole("button", { name: "Close modal", exact: true });
  const cancel = dialog.getByRole("button", { name: "Cancel", exact: true });
  await expect(name).toBeFocused();
  await page.keyboard.press("Shift+Tab");
  await expect(close).toBeFocused();
  await page.keyboard.press("Shift+Tab");
  await expect(cancel).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(close).toBeFocused();

  const region = dialog.getByRole("combobox", { name: "Region", exact: true });
  await region.click();
  await expect(page.getByRole("listbox", { name: "Region" })).toBeVisible();
  await page.keyboard.press("Tab");
  await expect(page.getByRole("listbox")).toHaveCount(0);
  await expect(region).toBeFocused();
  await expect(dialog).toBeVisible();
});

test("a long Select filters its options and picks with the keyboard", async ({ page }) => {
  await fixture(page);
  const dialog = await openNewProject(page);
  const region = dialog.getByRole("combobox", { name: "Region", exact: true });
  await expect(region).toHaveText("Amsterdam");
  await region.click();
  const filter = page.getByRole("searchbox", { name: "Filter options" });
  await expect(filter).toBeFocused();
  await expect(page.getByRole("option")).toHaveCount(cities.length);
  await filter.fill("lon");
  await expect(page.getByRole("option")).toHaveText(["London"]);
  await page.keyboard.press("Enter");
  await expect(page.getByRole("listbox")).toHaveCount(0);
  await expect(region).toHaveText("London");
  await expect(region).toBeFocused();

  await region.press("ArrowDown");
  await expect(filter).toBeFocused();
  await page.keyboard.press("ArrowDown");
  await expect(page.getByRole("option", { name: "Amsterdam", exact: true })).toBeFocused();
  await page.keyboard.press("ArrowDown");
  await expect(page.getByRole("option", { name: "Berlin", exact: true })).toBeFocused();
  await page.keyboard.press("End");
  await expect(page.getByRole("option", { name: "Tokyo", exact: true })).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(region).toHaveText("Tokyo");
});

test("a short Select moves with arrow keys, Home, End and typeahead", async ({ page }) => {
  await fixture(page);
  await page.goto(`/settings/projects?${scope}`);
  const unit = page.getByRole("combobox", { name: "Unit", exact: true });
  await expect(unit).toHaveText("Days");
  await unit.focus();
  await page.keyboard.press("ArrowDown");
  await expect(page.getByRole("option", { name: "Days", exact: true })).toBeFocused();
  await page.keyboard.press("ArrowDown");
  await expect(page.getByRole("option", { name: "Hours", exact: true })).toBeFocused();
  await page.keyboard.press("End");
  await expect(page.getByRole("option", { name: "Seconds", exact: true })).toBeFocused();
  await page.keyboard.press("Home");
  await expect(page.getByRole("option", { name: "Days", exact: true })).toBeFocused();
  await page.keyboard.press("m");
  await expect(page.getByRole("option", { name: "Minutes", exact: true })).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("listbox")).toHaveCount(0);
  await expect(unit).toHaveText("Minutes");
  await expect(unit).toBeFocused();

  await unit.press("s");
  await expect(page.getByRole("option", { name: "Seconds", exact: true })).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(unit).toHaveText("Seconds");
});

test("a row menu moves with arrow keys, Home and End, and Tab or Escape close it onto its trigger", async ({ page }) => {
  await fixture(page);
  await page.goto(`/settings/secrets?${scope}`);
  const trigger = page.getByRole("button", { name: `Actions for ${secretID}` });
  const menu = page.getByRole("group", { name: `Actions for ${secretID}` });
  const update = menu.getByRole("button", { name: "Update", exact: true });
  const revoke = menu.getByRole("button", { name: "Revoke", exact: true });

  await trigger.click();
  await expect(update).toBeFocused();
  await page.keyboard.press("ArrowDown");
  await expect(revoke).toBeFocused();
  await page.keyboard.press("ArrowDown");
  await expect(update).toBeFocused();
  await page.keyboard.press("ArrowUp");
  await expect(revoke).toBeFocused();
  await page.keyboard.press("Home");
  await expect(update).toBeFocused();
  await page.keyboard.press("End");
  await expect(revoke).toBeFocused();

  await page.keyboard.press("Tab");
  await expect(menu).toHaveCount(0);
  await expect(trigger).toBeFocused();
  await expect(trigger).toHaveAttribute("aria-expanded", "false");

  await trigger.click();
  await expect(update).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(menu).toHaveCount(0);
  await expect(trigger).toBeFocused();
});

test("closing a dialog opened from a row menu returns focus to the menu trigger", async ({ page }) => {
  await fixture(page);
  await page.goto(`/settings/secrets?${scope}`);
  const trigger = page.getByRole("button", { name: `Actions for ${secretID}` });
  await trigger.click();
  await page.getByRole("button", { name: "Update", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Update secret" });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByLabel("Value", { exact: true })).toBeFocused();
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(trigger).toBeFocused();

  await trigger.click();
  await page.getByRole("button", { name: "Update", exact: true }).click();
  await expect(dialog).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(trigger).toBeFocused();
});

test("page tabs move with arrow keys, Home and End and show the selected panel", async ({ page }) => {
  await fixture(page);
  await page.goto(`/deployments/${deploymentID}?${scope}`);
  const tabs = page.getByRole("tablist", { name: "Deployment sections" });
  const agents = tabs.getByRole("tab", { name: "Agents", exact: true });
  const templates = tabs.getByRole("tab", { name: "Templates", exact: true });
  const events = tabs.getByRole("tab", { name: "Events", exact: true });
  await expect(agents).toHaveAttribute("aria-selected", "true");
  await expect(page.getByRole("tabpanel", { name: "Agents" })).toContainText("assistant");

  await agents.focus();
  await page.keyboard.press("ArrowRight");
  await expect(templates).toBeFocused();
  await expect(templates).toHaveAttribute("aria-selected", "true");
  await expect(agents).toHaveAttribute("aria-selected", "false");
  await expect(page.getByRole("tabpanel", { name: "Templates" })).toContainText("browser");
  await page.keyboard.press("End");
  await expect(events).toBeFocused();
  await expect(events).toHaveAttribute("aria-selected", "true");
  await page.keyboard.press("ArrowRight");
  await expect(agents).toBeFocused();
  await page.keyboard.press("ArrowLeft");
  await expect(events).toBeFocused();
  await page.keyboard.press("Home");
  await expect(agents).toBeFocused();
  await expect(agents).toHaveAttribute("aria-selected", "true");
  await expect(tabs.getByRole("tab", { selected: true })).toHaveCount(1);
  await expect(page.getByRole("tabpanel")).toHaveCount(1);
});
