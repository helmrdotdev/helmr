import { expect, test, type Page } from "@playwright/test";

const projectID = "00000000-0000-7000-8000-000000000301";
const environmentID = "00000000-0000-7000-8000-000000000403";
const id = "00000000-0000-7000-8000-000000000799";
const turnID = "00000000-0000-7000-8000-000000000798";
const base = `/api/projects/${projectID}/environments/${environmentID}`;
const path = `${base}/sessions/${id}`;
const url = `/sessions/${id}?project_id=${projectID}&environment_id=${environmentID}`;
const now = "2026-10-06T00:00:00Z";
const initialSession = {
  id,
  agent_id: turnID,
  deployment_id: turnID,
  computer_id: turnID,
  root_session_id: id,
  parent_session_id: null,
  status: "open",
  created_at: now,
  holds: [],
};
const initialTurn = {
  id: turnID,
  session_id: id,
  sequence: 1,
  status: "queued",
  input: [{ type: "text", text: "original input" }],
};

async function scope(page: Page, controls: string[] = []) {
  const environment = {
    id: environmentID,
    project_id: projectID,
    slug: "dev",
    name: "Development",
    color_hex: "#008000",
    is_default: true,
    created_at: now,
    updated_at: now,
  };
  const project = {
    id: projectID,
    slug: "test",
    name: "Test",
    default_region_id: "local",
    is_default: true,
    environments: [environment],
    created_at: now,
    updated_at: now,
  };
  await page.route("**/api/**", async (route) => {
    const endpoint = new URL(route.request().url()).pathname;
    let json: unknown;
    if (endpoint === "/api/me")
      json = {
        user_id: "local",
        display_name: "Local",
        org_id: "org",
        permissions: ["sessions.read", "sessions.send", "sessions.cancel", ...controls],
        admin: false,
        organization_required: false,
        project_required: false,
      };
    else if (endpoint === "/api/projects") json = { projects: [project] };
    else if (endpoint === `/api/projects/${projectID}`) json = project;
    else if (endpoint === base) json = environment;
    else if (endpoint.endsWith("/events")) json = { records: [], next_after: 0, has_more: false, retained_after: 0 };
    else if (endpoint.endsWith("/asks")) json = { asks: [] };
    else {
      await route.fulfill({
        status: 404,
        json: { error: { code: "not_found", message: endpoint } },
      });
      return;
    }
    await route.fulfill({ json });
  });
}

test("retained queue shows independent holds and cancellation preserves input", async ({
  page,
}) => {
  await scope(page);
  let cancelled = false;
  const mutations: unknown[] = [];
  await page.route(`**${path}**`, async (route) => {
    const request = route.request();
    const endpoint = new URL(request.url()).pathname;
    if (request.method() === "POST") {
      expect(endpoint).toBe(`${path}/cancel`);
      mutations.push(request.postDataJSON());
      cancelled = true;
      await route.fulfill({
        json: { id: turnID, session_id: id, status: "accepted" },
      });
    } else if (endpoint === path) {
      await route.fulfill({
        json: {
          ...initialSession,
          status: cancelled ? "cancelled" : "open",
          holds: [
            {
              id: "hold-a",
              session_id: id,
              scope: "local",
              reason: "Operator pause",
              created_at: now,
            },
            {
              id: "hold-b",
              session_id: id,
              scope: "subtree",
              reason: "Secret revoked",
              created_at: now,
            },
          ],
        },
      });
    } else if (endpoint === `${path}/turns`)
      await route.fulfill({
        json: {
          turns: [
            {
              ...initialTurn,
              status: cancelled ? "cancelled" : "queued",
              ...(cancelled ? { terminal_at: now } : {}),
            },
          ],
        },
      });
    else await route.fallback();
  });
  await page.goto(url);
  await expect(
    page.getByText("original input", { exact: false }),
  ).toBeVisible();
  await expect(page.getByText("Operator pause (local)")).toBeVisible();
  await expect(page.getByText("Secret revoked (subtree)")).toBeVisible();
  await page
    .getByRole("button", { name: "Cancel Session", exact: true })
    .click();
  await expect(
    page.getByText("Other Sessions sharing the Computer are not cancelled.", {
      exact: false,
    }),
  ).toBeVisible();
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "Cancel Session", exact: true })
    .click();
  await expect(
    page.getByText('"status": "accepted"', { exact: false }),
  ).toBeVisible();
  await expect(
    page.getByText("original input", { exact: false }),
  ).toBeVisible();
  await expect(page.locator("form")).toHaveCount(0);
  await expect(page.getByText("Send work through the CLI", { exact: false })).toBeVisible();
  expect(mutations).toHaveLength(1);
  expect(mutations[0]).toEqual({ idempotency_key: expect.any(String) });
});

test("terminal Session observation refreshes retained Turn outcome", async ({
  page,
}) => {
  await scope(page);
  await page.clock.install();
  let terminal = false;
  let reads = 0;
  await page.route(`**${path}**`, async (route) => {
    const endpoint = new URL(route.request().url()).pathname;
    if (endpoint === path)
      await route.fulfill({
        json: { ...initialSession, status: terminal ? "closed" : "open" },
      });
    else if (endpoint === `${path}/turns`) {
      reads++;
      await route.fulfill({
        json: {
          turns: [
            {
              ...initialTurn,
              status: terminal ? "completed" : "finalizing",
              ...(terminal
                ? {
                    terminal_at: now,
                    completion_save_id: turnID,
                    result: { saved: "complete" },
                  }
                : {}),
            },
          ],
        },
      });
    } else await route.fallback();
  });
  await page.goto(url);
  await expect(
    page.getByText("The handler has returned.", { exact: false }),
  ).toBeVisible();
  await expect(
    page.getByText('"saved": "complete"', { exact: false }),
  ).toHaveCount(0);
  terminal = true;
  await page.clock.runFor(5100);
  await expect(
    page.getByText('"saved": "complete"', { exact: false }),
  ).toBeVisible();
  expect(reads).toBeGreaterThan(1);
});

test("ordered progress remains visible while final response waits for completion", async ({ page }) => {
  await scope(page);
  await page.clock.install();
  let completed = false;
  await page.route(`**${path}**`, async route => {
    const endpoint = new URL(route.request().url()).pathname;
    if (endpoint === path) await route.fulfill({ json: { ...initialSession, status: completed ? "closed" : "open" } });
    else if (endpoint === `${path}/turns`) await route.fulfill({ json: { turns: [{ ...initialTurn, status: completed ? "completed" : "finalizing", ...(completed ? { response: [{ type: "text", text: "Finished response" }], result: null } : {}) }] } });
    else if (endpoint === `${path}/events`) await route.fulfill({ json: { records: [{ session_id: id, turn_id: turnID, sequence: 1, kind: "turn.output", data: [{ type: "text", text: "Published progress" }, { type: "json", value: { n: null } }], created_at: now }], next_after: 1, has_more: false, retained_after: 0 } });
    else if (endpoint === `${path}/turns/${turnID}/asks`) await route.fulfill({ json: { asks: [] } });
    else await route.fallback();
  });
  await page.goto(url);
  await expect(page.getByText("Published progress", { exact: true })).toBeVisible();
  await expect(page.getByText("Finished response", { exact: true })).toHaveCount(0);
  completed = true;
  await page.clock.fastForward(5_001);
  await expect(page.getByText("Finished response", { exact: true })).toBeVisible();
  await expect(page.getByText("Published progress", { exact: true })).toBeVisible();
});

test("Session closure refreshes final progress and questions before another polling tick", async ({ page }) => {
  await scope(page);
  await page.clock.install();
  let closed = false;
  const askID = "00000000-0000-7000-8000-000000000797";
  await page.route(`**${path}**`, async route => {
    const endpoint = new URL(route.request().url()).pathname;
    if (endpoint === `${path}/cancel`) { closed = true; await route.fulfill({ json: { id: turnID, status: "accepted" } }); }
    else if (endpoint === path) await route.fulfill({ json: { ...initialSession, status: closed ? "cancelled" : "open" } });
    else if (endpoint === `${path}/turns`) await route.fulfill({ json: { turns: [{ ...initialTurn, status: closed ? "cancelled" : "running" }] } });
    else if (endpoint === `${path}/events`) await route.fulfill({ json: { records: closed ? [{ session_id: id, turn_id: turnID, sequence: 1, kind: "turn.output", data: [{ type: "text", text: "Last published output" }], created_at: now }] : [], next_after: closed ? 1 : 0, has_more: false, retained_after: 0 } });
    else if (endpoint === `${path}/turns/${turnID}/asks`) await route.fulfill({ json: { asks: [{ id: askID, session_id: id, turn_id: turnID, status: closed ? "cancelled" : "pending", created_at: now, ...(closed ? { cancelled_at: now } : {}), prompt: [{ type: "text", text: "Question" }], answer_control: { type: "text" } }] } });
    else await route.fallback();
  });
  await page.goto(url);
  await expect(page.getByRole("link", { name: "Question — pending" })).toBeVisible();
  await page.getByRole("button", { name: "Cancel Session", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Cancel Session", exact: true }).click();
  await expect(page.getByText("Last published output", { exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: "Question — cancelled" })).toBeVisible();
});

test("expired Turn content remains distinct from empty input and a null result", async ({ page }) => {
  await scope(page);
  await page.route(`**${path}**`, async route => {
    const endpoint = new URL(route.request().url()).pathname;
    if (endpoint === path) await route.fulfill({ json: { ...initialSession, status: "closed" } });
    else if (endpoint === `${path}/turns`) await route.fulfill({ json: { turns: [
      { id: turnID, session_id: id, sequence: 1, status: "completed", terminal_at: now, completion_save_id: turnID, payload_expired_at: now },
      { id: "00000000-0000-7000-8000-000000000797", session_id: id, sequence: 2, status: "completed", terminal_at: now, completion_save_id: turnID, input: [], result: null },
    ] } });
    else await route.fallback();
  });
  await page.goto(url);
  const expired = page.getByRole("listitem").filter({ hasText: "Turn #1" });
  await expect(expired.getByText("Turn content expired.")).toBeVisible();
  await expect(expired.getByText("Completed", { exact: true })).toBeVisible();
  await expect(expired.getByText("Input", { exact: true })).toHaveCount(0);
  const retained = page.getByRole("listitem").filter({ hasText: "Turn #2" });
  await expect(retained.getByText("Input", { exact: true })).toBeVisible();
  await expect(retained.locator("pre").filter({ hasText: /^null$/ })).toHaveCount(1);
});

for (const action of ["interrupt", "cancel"] as const) {
  test(`${action} retries preserve identity`, async ({ page }) => {
    await scope(page, [`sessions.${action}`]);
    const label = { interrupt: "Interrupt Session", cancel: "Cancel Session" }[action];
    const attempts: unknown[] = [];
    let accepted = false;
    await page.route(`**${path}**`, async route => {
      const request = route.request();
      const endpoint = new URL(request.url()).pathname;
      if (request.method() === "POST") {
        expect(endpoint).toBe(`${path}/${action}`);
        attempts.push(request.postDataJSON());
        if (attempts.length === 1) await route.fulfill({ status: 503, json: { error: { code: "unavailable", message: "Reply unavailable" } } });
        else {
          accepted = true;
          await route.fulfill({ json: { id: "control-receipt", session_id: id, status: "accepted" } });
        }
      } else if (endpoint === path) await route.fulfill({ json: {
        ...initialSession,
        status: accepted && action === "cancel" ? "cancelled" : "open",
        holds: [{ id: "owned-hold", session_id: id, scope: "subtree", reason: "Operator pause", created_at: now }],
      } });
      else if (endpoint === `${path}/turns`) await route.fulfill({ json: { turns: [initialTurn] } });
      else await route.fallback();
    });
    await page.goto(url);
    await page.getByRole("button", { name: label, exact: true }).click();
    const dialog = page.getByRole("dialog", { name: label });
    await dialog.getByRole("button", { name: label, exact: true }).click();
    await expect(dialog.getByRole("alert")).toContainText("Reply unavailable");
    await dialog.getByRole("button", { name: "Keep", exact: true }).click();
    await page.getByRole("button", { name: label, exact: true }).click();
    await dialog.getByRole("button", { name: label, exact: true }).click();
    await expect(dialog).toHaveCount(0);
    expect(attempts).toHaveLength(2);
    expect(attempts[1]).toEqual(attempts[0]);
    expect(attempts[0]).toEqual({ idempotency_key: expect.any(String) });
  });
}

test("inherited hold links its owner and cannot be released on a child", async ({ page }) => {
  await scope(page, ["sessions.resume"]);
  const owner = "00000000-0000-7000-8000-000000000796";
  await page.route(`**${path}**`, async route => {
    const endpoint = new URL(route.request().url()).pathname;
    if (endpoint === path) await route.fulfill({ json: { ...initialSession, holds: [{ id: "parent-hold", session_id: owner, scope: "subtree", reason: "Parent paused", created_at: now }] } });
    else if (endpoint === `${path}/turns`) await route.fulfill({ json: { turns: [initialTurn] } });
    else await route.fallback();
  });
  await page.goto(url);
  await expect(page.getByText("Parent paused (subtree)")).toBeVisible();
  await expect(page.locator("form")).toHaveCount(0);
  await expect(page.getByRole("link", { name: "000000000796", exact: true })).toHaveAttribute("href", `/sessions/${owner}?project_id=${projectID}&environment_id=${environmentID}`);
});
