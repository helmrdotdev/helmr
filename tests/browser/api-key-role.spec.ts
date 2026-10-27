import { expect, test } from "@playwright/test";
import { createAgentCatalog } from "./fixtures/agent-catalog";

test("a Session-only API key role selects Agent targets and enforces its access", async ({ page, request }) => {
  await page.goto("/dev/login");
  const { scope, base, environmentID } = await createAgentCatalog(page, ["api_keys.read", "api_keys.create"]);
  const roleName = `Session reader ${crypto.randomUUID()}`;
  const createdRole = await page.request.post("/api/roles", { data: { name: roleName, permissions: ["sessions.read"] } });
  expect(createdRole.ok(), await createdRole.text()).toBeTruthy();
  const role = await createdRole.json();
  const started = await page.request.post(`${base}/sessions`, { data: { agent: "browser-agent", idempotency_key: crypto.randomUUID() } });
  expect(started.ok(), await started.text()).toBeTruthy();
  const session = await started.json();
  const other = await createAgentCatalog(page);
  const otherStarted = await page.request.post(`${other.base}/sessions`, { data: { agent: "browser-agent", idempotency_key: crypto.randomUUID() } });
  expect(otherStarted.ok(), await otherStarted.text()).toBeTruthy();
  const otherSession = await otherStarted.json();
  await page.goto(`/settings/api-keys?${scope}`);
  await expect(page.getByText(`No API keys in ${environmentID} yet.`, { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: /^Generate API key/ })).toHaveCount(1);
  await page.getByRole("button", { name: "Generate API key", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Generate API key", exact: true });
  await dialog.getByLabel("Name", { exact: true }).fill("Session reader");
  await dialog.getByRole("combobox", { name: "Role", exact: true }).click();
  await page.getByRole("option", { name: roleName, exact: true }).click();
  await dialog.getByRole("button", { name: "Generate key", exact: true }).click();
  await expect(dialog).toContainText("Choose a role and its targets");
  await dialog.getByRole("checkbox", { name: "browser-agent", exact: true }).check();
  await expect(dialog).toContainText("Sessions: browser-agent");
  const issuedResponse = page.waitForResponse(response => response.url().endsWith(`${base}/api-keys`) && response.request().method() === "POST");
  await dialog.getByRole("button", { name: "Generate key", exact: true }).click();
  const issued = await issuedResponse;
  expect(issued.ok(), await issued.text()).toBeTruthy();
  const key = await issued.json();
  await expect(page.getByRole("dialog", { name: "API key generated" })).toBeVisible();
  const grants = await (await page.request.get("/api/role-grants?limit=100")).json();
  const grant = grants.grants.find((item: { api_key_id?: string }) => item.api_key_id === key.id);
  expect(grant).toMatchObject({ role_id: role.id, agents_mode: "selected", agent_keys: ["browser-agent"] });
  // The standalone request context has no login cookies: these requests act as
  // the issued key, including the cross-environment and mutation denials.
  const headers = { Authorization: `Bearer ${key.raw_key}` };
  const history = await request.get(`/v1/sessions/${session.session_id}`, { headers });
  expect(history.ok(), await history.text()).toBeTruthy();
  expect((await history.json()).id).toBe(session.session_id);
  expect((await request.post("/v1/sessions", { headers, data: { agent: "browser-agent", idempotency_key: crypto.randomUUID() } })).status()).toBe(403);
  expect((await request.get(`/v1/sessions/${otherSession.session_id}`, { headers })).status()).toBe(404);
});
