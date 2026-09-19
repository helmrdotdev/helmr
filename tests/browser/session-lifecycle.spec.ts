import { expect, test, type Page } from "@playwright/test";

const project = "00000000-0000-7000-8000-000000000301";
const environment = "00000000-0000-7000-8000-000000000403";
const id = "00000000-0000-7000-8000-000000000799";
const turnID = "00000000-0000-7000-8000-000000000798";
const base = `/api/projects/${project}/environments/${environment}/sessions/${id}`;
const url = `/sessions/${id}?project_id=${project}&environment_id=${environment}`;
const session = { id, actor_id: "lifecycle-fixture", deployment_id: "deployment", status: "open", created_at: "2026-09-20T00:00:00Z", updated_at: "2026-09-20T00:00:00Z", active_turn_id: turnID, current_run_id: null, dispatch: { state: "ready" } };
const turn = { id: turnID, session_id: id, status: "running", accepts_messages: true, interrupt_requested: false };
const event = (sequence: number, kind: string) => ({ id: `event-${sequence}`, session_id: id, turn_id: turnID, sequence, kind, data: {}, created_at: "2026-09-20T00:00:00Z", provenance: null });
async function login(page: Page) {
  await page.goto("/dev/login");
  await expect(page).toHaveURL("/");
}

test("terminal Session observation refreshes stale Turn and event projections", async ({ page }) => {
  await login(page);
  await page.clock.install();
  let eventReads = 0, turnReads = 0, terminal = false;
  await page.route(`**${base}**`, async route => {
    const path = new URL(route.request().url()).pathname;
    if (path === base) {
      if (eventReads >= 2 && turnReads >= 2) terminal = true;
      await route.fulfill({ json: { ...session, status: terminal ? "closed" : "open", active_turn_id: terminal ? null : turnID } });
    } else if (path.endsWith("/events")) {
      eventReads++;
      await route.fulfill({ json: { records: [event(1, "output"), ...(terminal ? [event(2, "turn.completed")] : [])], next_after: terminal ? 2 : 1, has_more: false, retained_after: 0 } });
    } else {
      turnReads++;
      await route.fulfill({ json: { ...turn, status: terminal ? "completed" : "running", accepts_messages: !terminal } });
    }
  });
  await page.goto(url);
  await expect(page.getByText("Turn status: running")).toBeVisible();
  await expect(page.getByText("output", { exact: true })).toBeVisible();
  await page.clock.runFor(5100);
  await expect.poll(() => eventReads).toBeGreaterThanOrEqual(2);
  await expect.poll(() => turnReads).toBeGreaterThanOrEqual(2);
  await page.clock.runFor(5100);
  await expect(page.getByText("Turn status: completed")).toBeVisible();
  await expect(page.getByText("turn.completed", { exact: true })).toBeVisible();
});

test("interruption receipt is not convergence and resume stays bound to confirmed hold", async ({ page }) => {
  await login(page);
  await page.clock.install();
  let held = false;
  let hold = "hold-one";
  const mutations: { path: string; body: Record<string, unknown> }[] = [];
  await page.route(`**${base}**`, async route => {
    const request = route.request(), path = new URL(request.url()).pathname;
    if (request.method() === "POST") {
      mutations.push({ path, body: request.postDataJSON() });
      if (path.endsWith("/interrupt")) {
        held = true;
        await route.fulfill({ json: { id: "stop-receipt", status: "stopping", turn_id: turnID, hold_id: hold } });
      } else {
        await route.fulfill({ status: 409, json: { error: { code: "conflict", message: "hold_mismatch" } } });
      }
    } else if (path === base) {
      await route.fulfill({ json: { ...session, active_turn_id: held ? null : turnID, dispatch: held ? { state: "held", hold_id: hold, reason: "interrupted" } : session.dispatch } });
    } else if (path.endsWith("/events")) {
      await route.fulfill({ json: { records: [], next_after: 0, has_more: false, retained_after: 0 } });
    } else {
      await route.fulfill({ json: { ...turn, status: held ? "interrupted" : "running", interrupt_requested: held } });
    }
  });
  await page.goto(url);
  await page.getByRole("button", { name: "Interrupt Turn", exact: true }).click();
  await page.getByRole("button", { name: "Interrupt", exact: true }).click();
  await expect(page.getByText('"status": "stopping"', { exact: false })).toBeVisible();
  await expect(page.getByText("Turn status: interrupted")).toBeVisible();
  await expect(page.getByText("Interruption requested; waiting for execution to stop.")).toHaveCount(0);
  await page.getByRole("button", { name: "Resume queued work", exact: true }).click();
  hold = "hold-two";
  await page.clock.runFor(5100);
  await expect(page.getByText("hold-two", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Resume", exact: true }).click();
  await expect(page.getByText("The pause has changed. Review the current Session before resuming.")).toBeVisible();
  await page.getByRole("button", { name: "Resume", exact: true }).click();
  expect(mutations[0]?.path).toBe(`${base}/turns/${turnID}/interrupt`);
  expect(mutations[1]?.body.hold_id).toBe("hold-one");
  await expect.poll(() => mutations.length).toBe(3);
  expect(mutations[2]?.body).toEqual(mutations[1]?.body);
});
