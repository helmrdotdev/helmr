import { expect, test, type Page } from "@playwright/test";

const project = "00000000-0000-7000-8000-000000000301";
const environment = "00000000-0000-7000-8000-000000000403";
const endpoint = `/api/projects/${project}/environments/${environment}`;

async function openCreation(page: Page) {
  await page.goto("/dev/login");
  await expect(page).toHaveURL("/");
  await page.goto("/settings/environments");
  await page.getByRole("row").filter({ hasText: "Demo" }).getByRole("button", { name: "Use" }).click();
  // Synthetic material in the actual seeded development API, never a real credential.
  const name = `browser-${crypto.randomUUID()}`;
  const response = await page.request.post(`${endpoint}/secrets`, {
    data: { name, value: "synthetic-browser-only", idempotency_key: crypto.randomUUID() },
  });
  expect(response.ok()).toBeTruthy();
  await page.goto("/workspaces");
  await page.getByRole("button", { name: "Create Workspace", exact: true }).click();
  return { dialog: page.getByRole("dialog"), name };
}

async function choose(page: Page, index: number, mode: string) {
  await page.getByRole("button", { name: "Placement", exact: true }).nth(index).click();
  await page.getByRole("option", { name: mode, exact: true }).click();
}

test("existing modal creates mixed bindings through real API and preserves typing", async ({ page }) => {
  const { dialog } = await openCreation(page);
  await dialog.getByRole("button", { name: "Add placement" }).click();
  const target = dialog.getByRole("textbox", { name: "Env var name" });
  await target.pressSequentially("GH_TOKEN");
  await expect(target).toBeFocused();
  await expect(target).toHaveValue("GH_TOKEN");
  const origins = dialog.getByRole("textbox", { name: "Allowed HTTPS origins" });
  await origins.pressSequentially("https://API.GITHUB.COM:443/");
  await expect(origins).toBeFocused();
  await expect(origins).toHaveValue("https://API.GITHUB.COM:443/");
  await dialog.getByRole("button", { name: "Add placement" }).click();
  await choose(page, 1, "Raw env");
  await dialog.getByRole("textbox", { name: "Env var name" }).nth(1).pressSequentially("PGPASSWORD");
  await dialog.getByRole("button", { name: "Add placement" }).click();
  await choose(page, 2, "Raw file");
  await dialog.getByRole("textbox", { name: "File path" }).pressSequentially("/run/secrets/client.key");
  // Remove a neighboring row and keep editing the remaining file row.
  await dialog.getByRole("button", { name: "Add placement" }).click();
  await dialog.getByRole("button", { name: "Remove placement" }).nth(3).click();
  await expect(dialog.getByRole("textbox", { name: "File path" })).toHaveValue("/run/secrets/client.key");
  const created = page.waitForResponse(r => r.url().includes("/sandboxes/demo-sandbox/workspaces") && r.request().method() === "POST");
  await dialog.getByRole("button", { name: "Create", exact: true }).click();
  const response = await created;
  expect(response.status()).toBe(201);
  const workspace = await response.json();
  expect(workspace.secrets).toHaveLength(3);
  expect(workspace.secrets.find((s: { env?: { mode: string } }) => s.env?.mode === "protected").env.allowed_origins).toEqual(["https://api.github.com"]);
  await expect(dialog).toHaveCount(0);
  await page.goto(`/workspaces/${workspace.id}?project_id=${project}&environment_id=${environment}`);
  await expect(page.getByText("Protected env", { exact: true })).toBeVisible();
  await expect(page.getByText("Raw env", { exact: true })).toBeVisible();
  await expect(page.getByText("Raw file", { exact: true })).toBeVisible();
  await expect(page.getByText("https://api.github.com", { exact: true })).toBeVisible();
});

test("invalid targets are rejected and uncertain responses reuse exact request identity", async ({ page }) => {
  const { dialog } = await openCreation(page);
  await dialog.getByRole("button", { name: "Add placement" }).click();
  await dialog.getByRole("textbox", { name: "Env var name" }).fill("TOKEN");
  await dialog.getByRole("textbox", { name: "Allowed HTTPS origins" }).fill("https://example.com/path");
  await dialog.getByRole("button", { name: "Create", exact: true }).click();
  await expect(dialog.getByRole("alert")).toContainText("exact HTTPS origins");
  await dialog.getByRole("textbox", { name: "Allowed HTTPS origins" }).fill("https://example.com");
  await dialog.getByRole("button", { name: "Add placement" }).click();
  await choose(page, 1, "Raw env");
  await dialog.getByRole("textbox", { name: "Env var name" }).nth(1).fill("TOKEN");
  const rejected = page.waitForResponse(r => r.url().includes("/sandboxes/demo-sandbox/workspaces") && r.request().method() === "POST");
  await dialog.getByRole("button", { name: "Create", exact: true }).click();
  const rejection = await rejected;
  expect(rejection.status()).toBe(400);
  await expect(dialog.getByRole("alert")).toHaveText((await rejection.json()).error.message);
  await dialog.getByRole("button", { name: "Remove placement" }).nth(1).click();
  const bodies: Record<string, unknown>[] = [];
  let workspaceID = "";
  await page.route(`**${endpoint}/sandboxes/demo-sandbox/workspaces`, async route => {
    bodies.push(route.request().postDataJSON());
    const response = await route.fetch();
    expect(response.ok()).toBeTruthy();
    const body = await response.json();
    if (!workspaceID) workspaceID = body.id;
    expect(body.id).toBe(workspaceID);
    if (bodies.length === 1) await route.abort("failed");
    else await route.fulfill({ response });
  });
  await dialog.getByRole("button", { name: "Create", exact: true }).click();
  await expect(dialog.getByRole("alert")).toHaveText("Workspace creation could not be confirmed. Retry without changing inputs to continue with the same request.");
  await dialog.getByRole("button", { name: "Create", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(bodies).toHaveLength(2);
  expect(bodies[0]).toEqual(bodies[1]);
});

test("editing effective input after an uncertain response uses a new key", async ({ page }) => {
  const { dialog } = await openCreation(page);
  await dialog.getByRole("button", { name: "Add placement" }).click();
  await choose(page, 0, "Raw env");
  await dialog.getByRole("textbox", { name: "Env var name" }).fill("FIRST_TOKEN");
  const requests: Record<string, unknown>[] = [];
  await page.route(`**${endpoint}/sandboxes/demo-sandbox/workspaces`, async route => {
    requests.push(route.request().postDataJSON());
    await route.abort("failed");
  });
  await dialog.getByRole("button", { name: "Create", exact: true }).click();
  await expect(dialog.getByRole("alert")).toHaveText("Workspace creation could not be confirmed. Retry without changing inputs to continue with the same request.");
  await dialog.getByRole("textbox", { name: "Env var name" }).fill("SECOND_TOKEN");
  await dialog.getByRole("button", { name: "Create", exact: true }).click();
  await expect.poll(() => requests.length).toBe(2);
  expect(requests[0]?.idempotency_key).not.toBe(requests[1]?.idempotency_key);
});
