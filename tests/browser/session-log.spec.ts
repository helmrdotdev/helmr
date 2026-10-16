import { expect, test, type Page } from "@playwright/test";
import { addEvent, appendOutput, askID, longSession, makeTurn, serveSession, sessionURL, sessionWith, settleTurn, turnID, notifySessionChanged } from "./fixtures/session-log";

const shots = process.env.HELMR_SESSION_LOG_SHOTS;
// Turns start closed; live specs open the running Turn (#41) to watch it.
const openTurn = (page: Page, sequence: number) => page.locator(`#turn-${turnID(sequence)} [data-turn-header] button[aria-expanded]`).click();

test("a Session loads its whole history with every Turn on the timeline", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1100 });
  const data = longSession();
  const requests = await serveSession(page, data);
  await page.goto(sessionURL);
  const log = page.getByRole("region", { name: "Log" });
  await expect(log.getByText(/^42 turns · \d+ events/)).toBeVisible();
  await expect(page.getByRole("list", { name: "Turn timeline" }).getByRole("listitem")).toHaveCount(42);
  // One events page and one Turns page; no request per Turn.
  expect(requests.events).toEqual(["?after=0&limit=1000"]);
  expect(requests.turns).toEqual(["?limit=100"]);
  // The running Turn waits on a question: shown above the log and in it.
  await expect(page.getByText("Turn #41 is waiting on a question", { exact: false })).toBeVisible();
  // Turns start closed, one header line each.
  const headers = log.locator("[data-turn-header] button");
  expect(await headers.count()).toBeGreaterThan(5);
  await expect(log.locator('[data-turn-header] button[aria-expanded="true"]')).toHaveCount(0);
  // Opening a Turn shows its events; the failed line carries the Turn record's message.
  const waiting = log.locator(`#turn-${turnID(41)}`);
  await waiting.locator("[data-turn-header] button[aria-expanded]").click();
  await expect(log.locator(`[data-ask="${askID}"]`)).toContainText("Merge PR #8812? · choice: Merge now / Wait for review");
  const failed = log.locator(`#turn-${turnID(35)}`);
  await failed.locator("[data-turn-header] button[aria-expanded]").click();
  await expect(failed).toContainText("handler_error · Export job exceeded the 12m deadline (shard 3)");
  if (shots) await page.screenshot({ path: `${shots}/main.png`, fullPage: true });

  // A JSON output opens as a folded tree.
  await failed.getByRole("button", { name: /json \{shard: 3/ }).first().click();
  const tree = failed.getByRole("figure").first();
  await expect(tree).toContainText("durations");
  await tree.getByRole("button", { name: "Expand durations" }).click();
  await expect(tree).toContainText("42: 1446");
  if (shots) await failed.screenshot({ path: `${shots}/payload.png` });
});

test("change notifications refresh events even while the Session record is unchanged", async ({ page }) => {
  await page.clock.install();
  const data = longSession();
  const requests = await serveSession(page, data);
  await page.goto(sessionURL);
  const log = page.getByRole("region", { name: "Log" });
  await expect(log.getByText(/^42 turns · \d+ events/)).toBeVisible();
  await openTurn(page, 41);
  const before = requests.events.length;
  appendOutput(data, ["Rebased on main.", "Waiting for review."]);
  await notifySessionChanged(page);
  await expect(log.getByText('"Waiting for review."')).toBeVisible();
  expect(requests.events.slice(before)).toEqual([`?after=${data.events.length - 2}&limit=1000`]);
  data.session["status"] = "closed";
  await notifySessionChanged(page);
  await expect(page.getByText("Closed", { exact: true })).toBeVisible();
  const settled = requests.events.length;
  await page.clock.runFor(30_000);
  expect(requests.events.length).toBe(settled);
});

test("expired content keeps Turn status and times", async ({ page }) => {
  const data = longSession({ expired: true });
  await serveSession(page, data);
  await page.goto(sessionURL);
  await expect(page.getByRole("region", { name: "Log" }).getByRole("strong").filter({ hasText: "Content expired" })).toBeVisible();
  await expect(page.getByText("handler_error").first()).toBeVisible();
});

test("phone layout keeps lines readable", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await serveSession(page, longSession());
  await page.goto(sessionURL);
  await expect(page.getByRole("list", { name: "Turn timeline" }).getByRole("listitem")).toHaveCount(42);
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  expect(overflow).toBeLessThanOrEqual(0);
  if (shots) await page.screenshot({ path: `${shots}/phone.png`, fullPage: true });
});

test("a Turn whose events continue after the next Turn was queued keeps its record", async ({ page }) => {
  const data = sessionWith({}, [makeTurn(1, { status: "running", started_at: new Date().toISOString(), input: [{ type: "text", text: "long first task" }] })]);
  data.turns.push(makeTurn(2, { input: [{ type: "text", text: "follow-up" }] }));
  addEvent(data, 2, "turn.queued", {});
  for (let index = 0; index < 300; index++) addEvent(data, 1, "turn.output", [{ type: "text", text: `step ${index}` }]);
  settleTurn(data, 1, { status: "completed", terminal_at: new Date().toISOString(), result: { done: true } });
  await serveSession(page, data);
  await page.goto(sessionURL);
  const first = page.locator(`#turn-${turnID(1)}`);
  await expect(first.locator("[data-turn-header]")).toContainText("#1");
  await expect(first.locator("[data-turn-header]")).toContainText("completed");
  await expect(first.locator("[data-turn-header]")).toContainText("long first task");
});

test("following drains bursts over 1000 events and retries a failed read on the next notification", async ({ page }) => {
  await page.clock.install();
  const data = longSession();
  const requests = await serveSession(page, data);
  await page.goto(sessionURL);
  const log = page.getByRole("region", { name: "Log" });
  await expect(log.getByText(/^42 turns · \d+ events/)).toBeVisible();
  await openTurn(page, 41);
  appendOutput(data, Array.from({ length: 1500 }, (_, index) => `burst ${index}`));
  await notifySessionChanged(page);
  await expect(log.getByText('"burst 1499"')).toHaveCount(1);
  data.failEvents = 1;
  appendOutput(data, ["after failure"]);
  await notifySessionChanged(page);
  await expect(log.getByRole("alert")).toBeVisible();
  await notifySessionChanged(page);
  await expect(log.getByText('"after failure"')).toBeVisible();
  // Settled Turn records already held are not read again.
  const turnReads = requests.turns.length;
  appendOutput(data, ["one more"]);
  await notifySessionChanged(page);
  await expect(log.getByText('"one more"')).toBeVisible();
  expect(requests.turns.slice(turnReads).every(query => query.includes("cursor=40"))).toBe(true);
});

test("an open payload stays open when new events arrive", async ({ page }) => {
  await page.clock.install();
  const data = longSession();
  await serveSession(page, data);
  await page.goto(sessionURL);
  await openTurn(page, 41);
  const failed = page.locator(`#turn-${turnID(35)}`);
  await failed.locator("[data-turn-header] button[aria-expanded]").click();
  await failed.getByRole("button", { name: /json \{shard: 3/ }).first().click();
  await expect(failed.getByRole("figure").first()).toContainText("durations");
  appendOutput(data, ["later output"]);
  await notifySessionChanged(page);
  await expect(page.getByText('"later output"')).toBeVisible();
  await expect(failed.getByRole("figure").first()).toContainText("durations");
});

test("a sync that fails as the Session closes still settles", async ({ page }) => {
  await page.clock.install();
  const data = longSession();
  await serveSession(page, data);
  await page.goto(sessionURL);
  const log = page.getByRole("region", { name: "Log" });
  await expect(log.getByText(/^42 turns · \d+ events/)).toBeVisible();
  await openTurn(page, 41);
  appendOutput(data, ["final words"]);
  data.session["status"] = "closed";
  data.failEvents = 1;
  await notifySessionChanged(page);
  await expect(log.getByRole("alert")).toBeVisible();
  await page.clock.runFor(2_100);
  await expect(log.getByText('"final words"')).toBeVisible();
  await expect(log.getByRole("alert")).toHaveCount(0);
});

test("a pending question after more than a page of answered ones is found", async ({ page }) => {
  const data = longSession();
  const pending = data.asks[0]!;
  data.asks = [...Array.from({ length: 120 }, (_, index) => ({ ...pending, id: `00000000-0000-7000-8000-${String(50_000 + index).padStart(12, "0")}`, status: "answered", answer: { selected: [{ id: "wait", value: "wait" }] }, answered_at: pending["created_at"], answered_by_user_id: "00000000-0000-7000-8000-000000000101" })), pending];
  await serveSession(page, data);
  await page.goto(sessionURL);
  await expect(page.getByText("Turn #41 is waiting on a question", { exact: false })).toBeVisible();
});

test("a failed first open on a live Session is retried on a change notification", async ({ page }) => {
  await page.clock.install();
  const data = longSession();
  data.failEvents = 1;
  await serveSession(page, data);
  await page.goto(sessionURL);
  const log = page.getByRole("region", { name: "Log" });
  await expect(log.getByRole("alert")).toBeVisible();
  // The next notification retries the failed initial read before its backoff.
  await notifySessionChanged(page);
  await expect(log.getByText(/^42 turns · \d+ events/)).toBeVisible();
  await expect(log.getByRole("alert")).toHaveCount(0);
});

test("views and search narrow the log", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 });
  const data = longSession();
  await serveSession(page, data);
  await page.goto(sessionURL);
  const log = page.getByRole("region", { name: "Log" });
  await expect(log.getByText(/^42 turns · \d+ events/)).toBeVisible();
  const show = log.getByRole("group", { name: "Show" });
  await show.getByRole("button", { name: /^Problems/ }).click();
  await expect(log.getByText("Slack update for", { exact: false })).toBeVisible();
  await expect(log.getByText('"Running 214 tests in 6 shards."')).toHaveCount(0);
  // Output shows what the agent produced or asked, with each Turn's input.
  await show.getByRole("button", { name: "Output", exact: true }).click();
  await expect(log.getByText("Slack update for", { exact: false })).toHaveCount(0);
  await expect(log.locator("[data-ask]").first()).toBeVisible();
  await expect(log.getByRole("button", { name: /^(\S+ )?input\b/ }).first()).toBeVisible();
  await show.getByRole("button", { name: "All", exact: true }).click();
  await log.getByRole("searchbox", { name: "Search events" }).fill("shard 3 is slower");
  await expect(log.getByText(/of \d+ shown/).first()).toBeVisible();
  if (shots) await page.screenshot({ path: `${shots}/search.png` });
});

test("an event link opens its Turn and highlights the line", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  const data = longSession();
  await serveSession(page, data);
  await page.goto(`${sessionURL}#event-40`);
  const line = page.locator("#event-40");
  await expect(line).toBeVisible();
  if (shots) await page.screenshot({ path: `${shots}/link.png` });
});

test("an early event link keeps the waiting question notice", async ({ page }) => {
  await serveSession(page, longSession());
  await page.goto(`${sessionURL}#event-1`);
  await expect(page.locator("#event-1")).toBeVisible();
  await expect(page.getByText("Turn #41 is waiting on a question", { exact: false })).toBeVisible();
});

test("a link past the newest event points at the newest one", async ({ page }) => {
  const data = longSession();
  await serveSession(page, data);
  await page.goto(`${sessionURL}#event-999999`);
  await expect(page.locator(`#event-${data.events.length}`)).toBeVisible();
  await page.goto(`${sessionURL}#turn-00000000-0000-7000-8000-00000000dead`);
  await expect(page.getByRole("region", { name: "Log" }).getByText(/^42 turns · \d+ events/)).toBeVisible();
});

test("a failed first load retries", async ({ page }) => {
  await page.clock.install();
  const data = longSession();
  data.failEvents = 1;
  const requests = await serveSession(page, data);
  await page.goto(sessionURL);
  const log = page.getByRole("region", { name: "Log" });
  await expect(log.getByRole("alert")).toBeVisible();
  await page.clock.runFor(2_100);
  await expect(log.getByText(/^42 turns · \d+ events/)).toBeVisible();
  expect(requests.events.every(query => query === "?after=0&limit=1000")).toBe(true);
});
