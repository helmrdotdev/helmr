import { execFileSync } from "node:child_process";
import { expect, test, type Page } from "@playwright/test";
import { createAgentCatalog } from "./fixtures/agent-catalog";

// Runtime phase rows are fixtures. External HTTP writes and Console observation
// use the running Control Plane and PostgreSQL, without mocked API responses.
async function conversation(page: Page, kind: "text" | "choice" = "text") {
  await page.goto("/dev/login");
  const catalog = await createAgentCatalog(page);
  const response = await page.request.post(`${catalog.base}/agents/browser-agent/start`, {
    data: { input: [{ type: "text", text: "retained" }], idempotency_key: crypto.randomUUID() },
  });
  expect(response.ok(), await response.text()).toBeTruthy();
  const { session_id: sessionID, turn_id: turnID } = await response.json() as { session_id: string; turn_id: string };
  const askID = `00000000-0000-7000-8000-${crypto.randomUUID().slice(-12)}`;
  const seed = (mode: "text" | "choice" | "finalizing" | "output" | "disable-member" | "restore-member" | "expose-secret", referenceID = askID) => {
    execFileSync("bash", ["tests/browser/fixtures/session-conversation.sh", catalog.environmentID, sessionID, turnID, referenceID, mode], { cwd: process.cwd() });
  };
  seed(kind);
  const turnBase = `${catalog.base}/sessions/${sessionID}/turns/${turnID}`;
  const question = await page.request.get(`${turnBase}/asks/${askID}`);
  expect(question.ok(), await question.text()).toBeTruthy();
  return { ...catalog, sessionID, turnID, askID, seed, turnBase,
    askBase: `${turnBase}/asks/${askID}`,
    askURL: `/sessions/${sessionID}/turns/${turnID}/asks/${askID}?${catalog.scope}`,
    sessionURL: `/sessions/${sessionID}?${catalog.scope}`,
  };
}

test("real API shows ordered progress while saving keeps response and result private", async ({ page }, testInfo) => {
  const f = await conversation(page);
  await page.goto(f.askURL);
  await expect(page.getByLabel("Answer through CLI")).toBeVisible();
  const response = await page.request.post(`${f.askBase}/respond`, { data: { answer: "", response_id: crypto.randomUUID() } });
  expect(response.ok(), await response.text()).toBeTruthy();
  await page.reload();
  await expect(page.getByLabel("Recorded answer")).toHaveText('""');
  const recorded = await (await page.request.get(f.askBase)).json();
  const me = await (await page.request.get("/api/me")).json();
  expect(recorded.status).toBe("responded");
  expect(recorded.answer).toBe("");
  expect(recorded.responded_by_user_id).toBe(me.user_id);
  f.seed("output");
  f.seed("finalizing");
  await page.goto(f.sessionURL);
  await expect(page.getByText("First visible progress", { exact: true })).toBeVisible();
  await expect(page.getByText("Second visible progress", { exact: true })).toBeVisible();
  const progress = page.locator('article[aria-label^="Progress "]');
  expect(await progress.allTextContents()).toEqual([
    expect.stringContaining("First visible progress"), expect.stringContaining("Second visible progress"),
  ]);
  await expect(page.getByText("The handler has returned. Helmr is saving the required disk state before completion.", { exact: false })).toBeVisible();
  await expect(page.getByText("RESPONSE_REQUIRES_OWN_SAVE")).toHaveCount(0);
  await expect(page.getByText("PRIVATE_RETURN")).toHaveCount(0);
  const before = await (await page.request.get(`${f.base}/sessions/${f.sessionID}/turns`)).json();
  expect(before.turns[0].status).toBe("finalizing");
  expect(JSON.stringify(before)).not.toContain("RESPONSE_REQUIRES_OWN_SAVE");
  expect(JSON.stringify(before)).not.toContain("PRIVATE_RETURN");
  const enqueue = await page.request.post(`${f.base}/sessions/${f.sessionID}/enqueue`, { data: { input: [{ type: "text", text: "kept while saving" }], idempotency_key: crypto.randomUUID() } });
  expect(enqueue.ok(), await enqueue.text()).toBeTruthy();
  await page.reload();
  await expect(page.getByText("kept while saving", { exact: false })).toBeVisible();
  await page.reload();
  await expect(page.getByText("Second visible progress", { exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: "Question — responded" })).toBeVisible();
  const after = await (await page.request.get(`${f.base}/sessions/${f.sessionID}/turns`)).json();
  expect(after.turns.map((turn: { status: string; input: unknown }) => [turn.status, turn.input])).toEqual([
    ["finalizing", [{ type: "text", text: "retained" }]], ["queued", [{ type: "text", text: "kept while saving" }]],
  ]);
  await page.screenshot({ path: testInfo.outputPath("real-api-saving-progress.png"), fullPage: true });
});

test("Console records one externally retried choice with its exact values", async ({ page }) => {
  const f = await conversation(page, "choice");
  await page.goto(f.askURL);
  await expect(page.getByLabel("Answer control")).toContainText("First");
  const answer = { selected: [{ id: "a", value: { v: 1 } }, { id: "b", value: null }], text: "extra" };
  const data = { answer, response_id: crypto.randomUUID() };
  for (let attempt = 0; attempt < 2; attempt++) {
    const response = await page.request.post(`${f.askBase}/respond`, { data });
    expect(response.ok(), await response.text()).toBeTruthy();
  }
  await page.reload();
  await expect(page.getByLabel("Recorded answer")).toHaveText(JSON.stringify(answer, null, 2));
  await expect(page.getByLabel("Answer through CLI")).toHaveCount(0);
  const events = await (await page.request.get(`${f.base}/sessions/${f.sessionID}/events?after=0&limit=100`)).json();
  expect(events.records.filter((event: { kind: string }) => event.kind === "ask.responded")).toHaveLength(1);
});

test("real API rejects a loaded question after organization scope loss and accepts after restoration", async ({ page }) => {
  const f = await conversation(page);
  await page.goto(f.askURL);
  const data = { answer: "retained draft", response_id: crypto.randomUUID() };
  const before = await (await page.request.get("/api/me")).json();
  expect(before.org_id).toBeTruthy();
  try {
    f.seed("disable-member");
    const revoked = await (await page.request.get("/api/me")).json();
    expect(revoked.org_id).toBeFalsy();
    expect(revoked.user_id).toBe(before.user_id);
    const reply = await page.request.post(`${f.askBase}/respond`, { data });
    // Revoked membership removes the request's organization/environment scope.
    expect(reply.status()).toBe(400);
    await expect(page.getByLabel("Recorded answer")).toHaveCount(0);
    expect((await page.request.get(f.askBase)).status()).toBe(400);
  } finally {
    f.seed("restore-member");
  }
  const pending = await (await page.request.get(f.askBase)).json();
  expect(pending.status).toBe("pending");
  const reply = await page.request.post(`${f.askBase}/respond`, { data });
  expect(reply.ok(), await reply.text()).toBeTruthy();
  await page.reload();
  await expect(page.getByLabel("Recorded answer")).toHaveText('"retained draft"');
});

test("real Secret revocation preserves queued input for inspection and explicit cancellation", async ({ page }, testInfo) => {
  const f = await conversation(page);
  const created = await page.request.post(`${f.base}/secrets`, {
    data: { name: "BROWSER_REVOCATION", value: "synthetic-browser-value", idempotency_key: crypto.randomUUID() },
  });
  expect(created.ok(), await created.text()).toBeTruthy();
  const secret = await created.json() as { id: string };
  // Exposure is a runtime fixture; revocation and its reconciliation use the real owners.
  f.seed("expose-secret", secret.id);
  await page.goto(f.sessionURL);
  const enqueue = await page.request.post(`${f.base}/sessions/${f.sessionID}/enqueue`, { data: { input: [{ type: "text", text: "retained after revocation" }], idempotency_key: crypto.randomUUID() } });
  expect(enqueue.ok(), await enqueue.text()).toBeTruthy();
  await page.reload();
  await expect(page.getByText("retained after revocation", { exact: false })).toBeVisible();
  await page.goto(`/settings/secrets?${f.scope}`);
  await page.getByRole("button", { name: "Actions for BROWSER_REVOCATION" }).click();
  const confirmation = page.waitForEvent("dialog");
  const click = page.getByRole("button", { name: "Revoke", exact: true }).click();
  const dialog = await confirmation;
  expect(dialog.message()).toContain("Pending inputs are retained but cannot run on affected Computers.");
  await dialog.accept();
  await click;
  await expect(page.getByText("Revoked", { exact: true })).toBeVisible();
  const turns = async () => (await (await page.request.get(`${f.base}/sessions/${f.sessionID}/turns`)).json()).turns;
  await expect.poll(async () => (await turns()).map((turn: { status: string }) => turn.status)).toEqual(["interrupted", "queued"]);
  await page.goto(f.sessionURL);
  await expect(page.getByText("Computer exposed to a revoked Secret", { exact: false })).toBeVisible();
  await expect(page.getByText("retained after revocation", { exact: false })).toBeVisible();
  expect((await (await page.request.get(f.askBase)).json()).status).toBe("cancelled");
  await page.reload();
  await expect(page.getByText("Use the CLI to close this Session or release a hold.", { exact: false })).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath("real-api-secret-blocked-queue.png"), fullPage: true });
  const held = await (await page.request.get(`${f.base}/sessions/${f.sessionID}`)).json();
  const release = await page.request.post(`${f.base}/sessions/${f.sessionID}/resume`, { data: { hold_id: held.holds[0].id, idempotency_key: crypto.randomUUID() } });
  expect(release.ok(), await release.text()).toBeTruthy();
  const released = await (await page.request.get(`${f.base}/sessions/${f.sessionID}`)).json();
  expect(released.status).toBe("open");
  expect(released.holds).toEqual([]);
  const rejected = await page.request.post(`${f.base}/sessions/${f.sessionID}/enqueue`, {
    data: { input: [{ type: "text", text: "must not bypass revoked Computer" }], idempotency_key: crypto.randomUUID() },
  });
  expect(rejected.status()).toBe(409);
  expect((await rejected.json()).error.code).toBe("admission_unavailable");
  expect((await turns()).map((turn: { status: string; input: unknown }) => [turn.status, turn.input])).toEqual([
    ["interrupted", [{ type: "text", text: "retained" }]], ["queued", [{ type: "text", text: "retained after revocation" }]],
  ]);
  await page.getByRole("button", { name: "Cancel Session", exact: true }).click();
  const cancel = page.getByRole("dialog", { name: "Cancel Session", exact: true });
  await cancel.getByRole("button", { name: "Cancel Session", exact: true }).click();
  await expect(cancel).toHaveCount(0);
  await expect.poll(async () => (await turns()).map((turn: { status: string; input: unknown }) => [turn.status, turn.input])).toEqual([
    ["interrupted", [{ type: "text", text: "retained" }]], ["cancelled", [{ type: "text", text: "retained after revocation" }]],
  ]);
  await page.reload();
  await expect(page.getByText("retained after revocation", { exact: false })).toBeVisible();
});
