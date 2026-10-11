import { expect, test, type Page } from "@playwright/test";

import { createAgentCatalog } from "./fixtures/agent-catalog";

async function confirm(page: Page, label: string) {
  await page.getByRole("button", { name: label, exact: true }).click();
  const dialog = page.getByRole("dialog", { name: label, exact: true });
  await dialog.getByRole("button", { name: label, exact: true }).click();
  await expect(dialog).toHaveCount(0);
}

test("Console inspects externally admitted work and applies interrupt and cancel", async ({ page }, testInfo) => {
  await page.goto("/dev/login");
  const { scope, base } = await createAgentCatalog(page);
  const response = await page.request.post(`${base}/agents/browser-agent/start`, {
    data: { input: [{ type: "text", text: "retained" }], idempotency_key: crypto.randomUUID() },
  });
  expect(response.ok(), await response.text()).toBeTruthy();
  const receipt = await response.json() as { session_id: string; turn_id: string };
  const enqueued = await page.request.post(`${base}/sessions/${receipt.session_id}/enqueue`, {
    data: { input: [{ type: "text", text: "queued" }], idempotency_key: crypto.randomUUID() },
  });
  expect(enqueued.ok(), await enqueued.text()).toBeTruthy();
  await page.goto(`/sessions/${receipt.session_id}?${scope}`);
  await expect(page.getByText("retained", { exact: false })).toBeVisible();
  await expect(page.locator("pre").filter({ hasText: /^queued$/ })).toBeVisible();
  await expect(page.locator("form")).toHaveCount(0);
  await confirm(page, "Interrupt Session");
  const held = await (await page.request.get(`${base}/sessions/${receipt.session_id}`)).json();
  expect(held.holds).toHaveLength(1);
  await expect(page.getByText("Use the CLI to close this Session or release a hold.", { exact: false })).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath("held-session.png"), fullPage: true });
  await page.goto(`/sessions?${scope}`);
  await expect(page.getByRole("row").filter({ hasText: receipt.session_id.slice(-12) })).toContainText("1 active");
  await page.goto(`/sessions/${receipt.session_id}?${scope}`);
  await confirm(page, "Cancel Session");
  await expect(page.getByRole("button", { name: "Cancel Session", exact: true })).toHaveCount(0);
  const turns = await (await page.request.get(`${base}/sessions/${receipt.session_id}/turns`)).json();
  expect(turns.turns.map((turn: { status: string }) => turn.status)).toEqual(["cancelled", "cancelled"]);
  expect(turns.turns.map((turn: { input: unknown }) => turn.input)).toEqual([[{ type: "text", text: "retained" }], [{ type: "text", text: "queued" }]]);
  await expect(page.getByText("retained", { exact: false })).toBeVisible();
  await expect(page.locator("pre").filter({ hasText: /^queued$/ })).toBeVisible();
});
