import { expect, test, type Page } from "@playwright/test";
import { addEvent, makeTurn, serveSession, sessionPath, sessionURL, sessionWith, settleTurn, turnID, type SessionData, expandTurns, notifySessionChanged } from "./fixtures/session-log";

const now = new Date().toISOString();
const log = (page: Page) => page.getByRole("region", { name: "Log" });
const inputLine = (page: Page, sequence: number) => log(page).locator(`#turn-${turnID(sequence)}`).getByRole("button", { name: /^(\S+ )?input\b/ });

// Answers a control POST once the page asks for it; everything else falls through
// to the in-memory Session.
async function onControl(page: Page, action: string, handle: (body: unknown) => { status?: number; json: unknown }) {
  await page.route(`**${sessionPath}/${action}`, async route => {
    if (route.request().method() !== "POST") return route.fallback();
    const reply = handle(route.request().postDataJSON());
    await route.fulfill({ status: reply.status ?? 200, json: reply.json });
  });
}

test("retained queue shows independent holds and cancellation preserves input", async ({ page }) => {
  const data = sessionWith({
    holds: [
      { id: "hold-a", session_id: "00000000-0000-7000-8000-000000000900", scope: "local", reason: "Operator pause", created_at: now },
      { id: "hold-b", session_id: "00000000-0000-7000-8000-000000000900", scope: "subtree", reason: "Secret revoked", created_at: now },
    ],
  }, [makeTurn(1)]);
  await serveSession(page, data);
  const mutations: unknown[] = [];
  await onControl(page, "cancel", body => {
    mutations.push(body);
    data.session["status"] = "cancelled";
    settleTurn(data, 1, { status: "cancelled", terminal_at: now });
    return { json: { id: turnID(1), session_id: data.session["id"], status: "accepted" } };
  });
  await page.goto(sessionURL);
  await expandTurns(page);
  await expect(inputLine(page, 1)).toContainText("original input");
  await expect(page.getByText("Held: Operator pause (this session).", { exact: false })).toBeVisible();
  await expect(page.getByText("Held: Secret revoked (this session and its children).", { exact: false })).toBeVisible();
  await page.getByRole("button", { name: "Cancel session", exact: true }).click();
  await expect(page.getByText("Other sessions sharing the computer are not cancelled.", { exact: false })).toBeVisible();
  await page.getByRole("dialog").getByRole("button", { name: "Cancel session", exact: true }).click();
  await expect(page.getByText("Cancel session requested.", { exact: false })).toBeVisible();
  await expect(log(page).locator(`#turn-${turnID(1)}`)).toContainText("cancelled");
  await expect(inputLine(page, 1)).toContainText("original input");
  await expect(page.locator("form")).toHaveCount(0);
  expect(mutations).toEqual([{ idempotency_key: expect.any(String) }]);
});

test("terminal Session observation refreshes retained Turn outcome", async ({ page }) => {
  await page.clock.install();
  const data = sessionWith({}, [makeTurn(1, { status: "running", started_at: now })]);
  await serveSession(page, data);
  await page.goto(sessionURL);
  await expandTurns(page);
  await expect(log(page).locator(`#turn-${turnID(1)}`)).toContainText("running");
  await expect(log(page).getByRole("button", { name: /^(\S+ )?result / })).toHaveCount(0);
  settleTurn(data, 1, { status: "completed", terminal_at: now, result: { saved: "complete" } });
  data.session["status"] = "closed";
  await notifySessionChanged(page);
  await expect(log(page).getByRole("button", { name: /^(\S+ )?result / })).toContainText('json {saved: "complete"}');
});

test("ordered progress remains visible while final response waits for completion", async ({ page }) => {
  await page.clock.install();
  const data = sessionWith({}, [makeTurn(1, { status: "running", started_at: now })]);
  addEvent(data, 1, "turn.output", [{ type: "text", text: "Published progress" }, { type: "json", value: { n: null } }]);
  await serveSession(page, data);
  await page.goto(sessionURL);
  await expandTurns(page);
  await expect(log(page).getByText('"Published progress" · json {n: null}')).toBeVisible();
  await expect(log(page).getByRole("button", { name: /^(\S+ )?response / })).toHaveCount(0);
  settleTurn(data, 1, { status: "completed", terminal_at: now, response: [{ type: "text", text: "Finished response" }], result: null });
  data.session["status"] = "closed";
  await notifySessionChanged(page);
  await expect(log(page).getByRole("button", { name: /^(\S+ )?response / })).toContainText('"Finished response"');
  await expect(log(page).getByText('"Published progress" · json {n: null}')).toBeVisible();
});

test("Session closure refreshes final progress and the waiting question", async ({ page }) => {
  const askID = "00000000-0000-7000-8000-000000000797";
  const data = sessionWith({}, [makeTurn(1, { status: "running", started_at: now })]);
  addEvent(data, 1, "ask.created", { ask_id: askID, question: { prompt: [{ type: "text", text: "Ship it?" }], type: "text" } });
  data.asks.push({ id: askID, session_id: data.session["id"], turn_id: turnID(1), status: "pending", created_at: now, question: { prompt: [{ type: "text", text: "Ship it?" }], type: "text" } });
  await serveSession(page, data);
  await onControl(page, "cancel", () => {
    data.session["status"] = "cancelled";
    addEvent(data, 1, "turn.output", [{ type: "text", text: "Last published output" }]);
    addEvent(data, 1, "ask.cancelled", { ask_id: askID });
    data.asks[0]!["status"] = "cancelled";
    settleTurn(data, 1, { status: "cancelled", terminal_at: now });
    return { json: { id: turnID(1), status: "accepted" } };
  });
  await page.goto(sessionURL);
  await expandTurns(page);
  await expect(page.getByText("Turn #1 is waiting on a question", { exact: false })).toBeVisible();
  await page.getByRole("button", { name: "Cancel session", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Cancel session", exact: true }).click();
  await expect(log(page).getByText('"Last published output"')).toBeVisible();
  await expect(log(page).getByText("question cancelled")).toBeVisible();
  await expect(page.getByText("Turn #1 is waiting on a question", { exact: false })).toHaveCount(0);
});

test("expired Turn content remains distinct from empty input and a null result", async ({ page }) => {
  const data = sessionWith({ status: "closed" }, [
    makeTurn(1, { status: "completed", terminal_at: now, payload_expired_at: now }),
    makeTurn(2, { status: "completed", terminal_at: now, input: [], result: null }),
  ]);
  delete data.turns[0]!.input;
  await serveSession(page, data);
  await page.goto(sessionURL);
  await expandTurns(page);
  const expired = log(page).locator(`#turn-${turnID(1)}`);
  await expect(expired.locator("[data-log-line]").filter({ hasText: /^[\d:.]*input/ })).toContainText("content expired");
  await expect(log(page).locator(`#turn-${turnID(1)}`).getByRole("button", { name: /^(\S+ )?result / })).toHaveCount(0);
  await expect(inputLine(page, 2)).not.toContainText("expired");
  await expect(log(page).locator(`#turn-${turnID(2)}`).getByRole("button", { name: /^(\S+ )?result / })).toContainText("json null");
});

for (const action of ["interrupt", "cancel"] as const) {
  test(`${action} retries preserve identity`, async ({ page }) => {
    const label = { interrupt: "Interrupt session", cancel: "Cancel session" }[action];
    const data = sessionWith({ holds: [{ id: "owned-hold", session_id: "00000000-0000-7000-8000-000000000900", scope: "subtree", reason: "Operator pause", created_at: now }] }, [makeTurn(1)]);
    await serveSession(page, data);
    const attempts: unknown[] = [];
    await onControl(page, action, body => {
      attempts.push(body);
      if (attempts.length === 1) return { status: 503, json: { error: { code: "unavailable", message: "Reply unavailable" } } };
      if (action === "cancel") data.session["status"] = "cancelled";
      return { json: { id: "control-receipt", session_id: data.session["id"], status: "accepted" } };
    });
    await page.goto(sessionURL);
    await expandTurns(page);
    await page.getByRole("button", { name: label, exact: true }).click();
    const dialog = page.getByRole("dialog", { name: label });
    await dialog.getByRole("button", { name: label, exact: true }).click();
    await expect(dialog.getByRole("alert")).toContainText("Reply unavailable");
    await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
    await page.getByRole("button", { name: label, exact: true }).click();
    await dialog.getByRole("button", { name: label, exact: true }).click();
    await expect(dialog).toHaveCount(0);
    expect(attempts).toHaveLength(2);
    expect(attempts[1]).toEqual(attempts[0]);
    expect(attempts[0]).toEqual({ idempotency_key: expect.any(String) });
  });
}

test("inherited hold links its owner and cannot be released on a child", async ({ page }) => {
  const owner = "00000000-0000-7000-8000-000000000796";
  await serveSession(page, sessionWith({ holds: [{ id: "parent-hold", session_id: owner, scope: "subtree", reason: "Parent paused", created_at: now }] }, [makeTurn(1)]));
  await page.goto(sessionURL);
  await expandTurns(page);
  await expect(page.getByText("Held: Parent paused (this session and its children).", { exact: false })).toBeVisible();
  await expect(page.locator("form")).toHaveCount(0);
  await expect(page.getByRole("link", { name: "000000000796", exact: true })).toHaveAttribute("href", `/sessions/${owner}`);
});

for (const status of ["open", "closing", "closed", "cancelled"] as const) {
  const active = status === "open" || status === "closing";
  test(`Interrupt session and the Cancel session danger zone are ${active ? "offered" : "not offered"} for a ${status} Session`, async ({ page }) => {
    // Settled Turns only: whether work is running does not decide the action.
    await serveSession(page, sessionWith({ status }, [makeTurn(1, { status: "completed", terminal_at: now, result: null })]), ["sessions.read", "sessions.interrupt", "sessions.cancel"]);
    await page.goto(sessionURL);
    await expandTurns(page);
    await expect(inputLine(page, 1)).toContainText("original input");
    await expect(page.getByRole("button", { name: "Interrupt session", exact: true })).toHaveCount(active ? 1 : 0);
    await expect(page.getByRole("region", { name: "Cancel session" }).getByRole("button", { name: "Cancel session", exact: true })).toHaveCount(active ? 1 : 0);
  });
}

test("Cancel session sits in a danger zone after the log and confirms before cancelling", async ({ page }) => {
  const data = sessionWith({}, [makeTurn(1)]);
  await serveSession(page, data);
  const mutations: string[] = [];
  await onControl(page, "cancel", () => {
    mutations.push("cancel");
    data.session["status"] = "cancelled";
    settleTurn(data, 1, { status: "cancelled", terminal_at: now });
    return { json: { id: turnID(1), status: "accepted" } };
  });
  await page.goto(sessionURL);
  await expandTurns(page);
  const zone = page.getByRole("region", { name: "Cancel session" });
  await expect(zone).toBeVisible();
  // The page header keeps Copy link and Interrupt; the destructive action is not there.
  await expect(page.getByRole("button", { name: "Cancel session", exact: true })).toHaveCount(1);
  const logBox = (await log(page).boundingBox())!;
  const zoneBox = (await zone.boundingBox())!;
  expect(zoneBox.y).toBeGreaterThan(logBox.y + logBox.height);
  await zone.getByRole("button", { name: "Cancel session", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Cancel session" });
  await expect(dialog).toBeVisible();
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(mutations).toHaveLength(0);
  await zone.getByRole("button", { name: "Cancel session", exact: true }).click();
  await dialog.getByRole("button", { name: "Cancel session", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(mutations).toEqual(["cancel"]);
  await expect(page.getByRole("region", { name: "Cancel session" })).toHaveCount(0);
});

test("a Session without events says so instead of an empty log", async ({ page }) => {
  await serveSession(page, sessionWith({}, []));
  await page.goto(sessionURL);
  await expandTurns(page);
  await expect(log(page).getByText("No events yet. Events appear when the first turn is queued.")).toBeVisible();
});

test("retained structured inputs render without treating JSON values as text parts", async ({ page }) => {
  const inputs: unknown[] = [{ topic: "structured object", count: 2 }, "structured string", 7, false, null, [1, { nested: true }]];
  const data: SessionData = sessionWith({}, [...inputs.map((input, index) => makeTurn(index + 1, { input })), makeTurn(inputs.length + 1)]);
  await serveSession(page, data);
  const errors: string[] = [];
  page.on("pageerror", error => errors.push(error.message));
  await page.goto(sessionURL);
  await expandTurns(page);
  for (let index = 0; index < inputs.length; index++) {
    const line = inputLine(page, index + 1);
    await line.click();
    // The input line opens the Turn record's input; Raw shows it exactly.
    const view = log(page).getByRole("figure", { name: `Turn #${index + 1} input` });
    await view.getByRole("button", { name: "Raw", exact: true }).click();
    expect(JSON.parse(await view.locator("pre").innerText()).input).toEqual(inputs[index]);
  }
  await inputLine(page, inputs.length + 1).click();
  await expect(log(page).getByRole("figure", { name: `Turn #${inputs.length + 1} input` })).toContainText("original input");
  expect(errors).toEqual([]);
});

test("recovery notice reports this conversation and Resume releases only its recovery hold", async ({ page }) => {
  const recoveryHold = "00000000-0000-7000-8000-000000000711";
  const owner = "00000000-0000-7000-8000-000000000900";
  const operatorHold = { id: "operator-hold", session_id: owner, scope: "local", kind: "control", reason: "Operator pause", created_at: now };
  const data = sessionWith({ holds: [operatorHold, {
    id: recoveryHold, session_id: owner, scope: "local", kind: "recovery_gap", reason: "Recovery confirmation required", created_at: now,
    recovery_gap: { holdId: recoveryHold, computerId: turnID(1), leaseEpoch: 1, recoverySaveId: null, restoredAt: null, affectedTurnIds: [turnID(1)] },
  }] }, [makeTurn(1, { status: "completed", terminal_at: now, response: [{ type: "text", text: "Previously delivered answer" }], result: null })]);
  await serveSession(page, data, ["sessions.read", "sessions.resume"]);
  const mutations: unknown[] = [];
  await onControl(page, "resume", body => {
    mutations.push(body);
    data.session["holds"] = [operatorHold];
    return { json: { kind: "resume", hold_id: recoveryHold } };
  });
  await page.goto(sessionURL);
  await expandTurns(page);
  await expect(page.getByText("This Computer was restored to the initial work environment.", { exact: false })).toBeVisible();
  await expect(page.getByText("native conversation history for 1 completed requests", { exact: false })).toBeVisible();
  await expect(log(page).getByRole("button", { name: /^(\S+ )?response / })).toContainText("Previously delivered answer");
  await page.getByRole("button", { name: "Resume", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByText("Completed requests will not run again.", { exact: false })).toBeVisible();
  await dialog.getByRole("button", { name: "Resume", exact: true }).click();
  await expect(page.getByText("This Computer was restored", { exact: false })).toHaveCount(0);
  await expect(page.getByText("Operator pause", { exact: false })).toBeVisible();
  expect(mutations).toEqual([{ hold_id: recoveryHold, idempotency_key: expect.any(String) }]);
});

test("a failed first Session read shows Retry while automatic recovery remains active", async ({ page }) => {
  await page.clock.install();
  const data = sessionWith({}, [makeTurn(1, { status: "completed", terminal_at: now, result: null })]);
  await serveSession(page, data);
  let reads = 0;
  await page.route(`**${sessionPath}`, async route => {
    reads++;
    if (reads === 1) return route.fulfill({ status: 503, json: { error: { code: "unavailable", message: "Temporarily unavailable" } } });
    return route.fallback();
  });
  await page.goto(sessionURL);
  const failure = page.getByRole("alert").filter({ hasText: "Could not load this session." });
  await expect(failure).toBeVisible();
  await expect(failure.getByRole("button", { name: "Retry", exact: true })).toBeVisible();
  await page.clock.runFor(5_100);
  await expect(page.getByText("CLI or API", { exact: true })).toBeVisible();
  await expect(failure).toHaveCount(0);
  expect(reads).toBe(2);
});

test("a failed Session snapshot retries without another change notification", async ({ page }) => {
  await page.clock.install();
  const data = sessionWith({}, [makeTurn(1, { status: "completed", terminal_at: now, result: null })]);
  await serveSession(page, data, ["sessions.read", "sessions.resume"]);
  await page.goto(sessionURL);
  await expect(page.getByText("CLI or API", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Resume", exact: true })).toHaveCount(0);
  const hold = "00000000-0000-7000-8000-000000000712";
  data.session["holds"] = [{
    id: hold, session_id: data.session["id"], scope: "local", kind: "recovery_gap", reason: "Recovery confirmation required", created_at: now,
    recovery_gap: { holdId: hold, computerId: turnID(1), leaseEpoch: 1, recoverySaveId: null, restoredAt: null, affectedTurnIds: [turnID(1)] },
  }];
  let reads = 0;
  await page.route(`**${sessionPath}`, async route => {
    reads++;
    if (reads <= 4) return route.fulfill({ status: 503, json: { error: { code: "unavailable", message: "Temporarily unavailable" } } });
    return route.fallback();
  });
  // The stream stays connected and delivers only this one hint.
  await notifySessionChanged(page);
  await expect.poll(() => reads).toBe(1);
  await expect(page.getByRole("alert").filter({ hasText: "Could not load this session." })).toBeVisible();
  for (let index = 0; index < 4; index++) {
    await page.clock.runFor(5_100);
    await expect.poll(() => reads).toBe(index + 2);
  }
  await expect(page.getByText("native conversation history for 1 completed requests", { exact: false })).toBeVisible();
  await expect(page.getByRole("button", { name: "Resume", exact: true })).toBeVisible();
  await expect(page.getByRole("alert").filter({ hasText: "Could not load this session." })).toHaveCount(0);
  await page.clock.runFor(60_000);
  expect(reads).toBe(5);
});

test("current storage hold stays authoritative across paginated pressure history", async ({ page }) => {
  const stopped = "The request was stopped because storage stayed full";
  const data = sessionWith({ holds: [{
    id: "pressure-hold", session_id: "00000000-0000-7000-8000-000000000900", scope: "local", kind: "storage_pressure",
    reason: `${stopped}. Saving is still retrying; new requests will wait until space is available.`, created_at: now,
  }] }, [makeTurn(1, { status: "interrupted", terminal_at: now })]);
  addEvent(data, 1, "session.storage_pressure", { holdId: "pressure-hold", state: "paused" });
  for (let index = 0; index < 1000; index++) addEvent(data, 1, "turn.output", [{ type: "text", text: `progress ${index}` }]);
  addEvent(data, 1, "session.storage_pressure", { holdId: "pressure-hold", state: "stopped" });
  const requests = await serveSession(page, data);
  await page.goto(sessionURL);
  await expandTurns(page);
  await expect(page.getByRole("status").filter({ hasText: `${stopped}. Saving is still retrying;` })).toBeVisible();
  await expect(log(page).getByText("Work was paused while storage was full.", { exact: true })).toBeVisible();
  await expect(log(page).getByText(`${stopped}. Local changes are retained while this environment remains available; recovery may omit changes not yet saved.`, { exact: true })).toBeVisible();
  expect(requests.events).toContain("?after=1000&limit=1000");
  data.session["holds"] = [];
  addEvent(data, 1, "session.storage_pressure", { holdId: "pressure-hold", state: "resolved" });
  await notifySessionChanged(page);
  await expect(page.getByRole("status").filter({ hasText: `${stopped}. Saving is still retrying;` })).toHaveCount(0);
  await expect(log(page).getByText("Storage recovered and queued work was allowed to continue.", { exact: false })).toBeVisible();
});
