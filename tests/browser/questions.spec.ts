import { isAuthenticationPath, authenticationResponse } from "./fixtures/authentication";
import { expect, test, type Page } from "@playwright/test";
const projectID = "console-demo";
const environmentID = "demo";
const sessionID = "00000000-0000-7000-8000-000000000799";
const turnID = "00000000-0000-7000-8000-000000000798";
const askID = "00000000-0000-7000-8000-000000000797";
const userID = "00000000-0000-7000-8000-000000000796";
const base = `/api/projects/${projectID}/environments/${environmentID}`;
const path = `${base}/sessions/${sessionID}/turns/${turnID}/asks/${askID}`;
const url = `/sessions/${sessionID}/turns/${turnID}/asks/${askID}?project=${projectID}&environment=${environmentID}`;
const now = "2026-10-06T00:00:00Z";
// The value of one rail entry on the Question page.
const detail = (page: Page, label: string) => page.getByRole("complementary").locator("div").filter({ has: page.locator("dt", { hasText: label }) }).locator("dd");
async function scope(page: Page) {
  const environment = {
    environment: environmentID, project: projectID,
    name: "Development",
    color_hex: "#008000",
    is_default: true,
    created_at: now,
    updated_at: now,
  };
  const project = {
    project: projectID,
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
    if (isAuthenticationPath(endpoint))
      json = {
        user_id: "local",
        display_name: "Local",
        org_id: "org",
        permissions: ["sessions.read"],
        admin: false,
        organization_required: false,
        project_required: false,
      };
    else if (endpoint === "/api/projects") json = { projects: [project] };
    else if (endpoint === `/api/projects/${projectID}`) json = project;
    else if (endpoint === base) json = environment;
    else {
      await route.fulfill({
        status: 404,
        json: { error: { code: "not_found", message: endpoint } },
      });
      return;
    }
    await route.fulfill({ json: authenticationResponse(endpoint, json) });
  });
}

function question(control: Record<string, unknown>) {
  return { id: askID, session_id: sessionID, turn_id: turnID, status: "pending", created_at: now,
    question: { prompt: [{ type: "text", text: "<script>do not execute</script>Choose an answer" }, { type: "json", value: { context: null } }], ...control } };
}
async function fixture(page: Page, control: Record<string, unknown>) {
  await scope(page);
  let state: Record<string, unknown> = question(control);
  const writes: string[] = [];
  await page.route(`**${path}**`, async route => {
    if (route.request().method() !== "GET") writes.push(route.request().method());
    await route.fulfill({ json: state });
  });
  await page.goto(url);
  await expect(page.getByText("<script>do not execute</script>Choose an answer", { exact: true })).toBeVisible();
  return { writes, setState: (next: Record<string, unknown>) => { state = next; } };
}

test("pending questions expose complete content and exact CLI commands as read-only guidance", async ({ page }, testInfo) => {
  const control = { type: "choice", title: "Release plan", multiple: true, allowCustom: true, options: [
    { id: "a", label: "First", description: "Full description", value: { v: 1 } }, { id: "b", label: "Second", value: null },
  ] };
  const { writes } = await fixture(page, control);
  await expect(page.getByRole("heading", { name: "Release plan", exact: true })).toBeVisible();
  await expect(page.getByText("First", { exact: true })).toBeVisible();
  await expect(page.getByText("Full description", { exact: true })).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath("question-desktop.png"), fullPage: true });
  await page.getByText("Inspect question and answer", { exact: true }).click();
  const definition = page.getByRole("figure", { name: "Question and answer" });
  await definition.getByRole("button", { name: "Raw", exact: true }).click();
  expect(JSON.parse(await definition.locator("pre").innerText()).question).toEqual(question(control).question);
  const origin = new URL(page.url()).origin;
  const guide = page.getByLabel("Answer through CLI");
  await expect(guide).toContainText(`helmr login '${origin}'`);
  await expect(guide).toContainText(`helmr --api-url '${origin}' session turn ask get '${askID}' --session '${sessionID}' --turn '${turnID}' --project '${projectID}' --env '${environmentID}' --json`);
  await expect(guide).toContainText(`answer '${askID}' --session '${sessionID}' --turn '${turnID}'`);
  await expect(guide).toContainText("--answer-file answer.json --answer-id ANSWER_ID");
  await expect(guide).toContainText("Reuse it with the same answer");
  await expect(page.locator("form")).toHaveCount(0);
  expect(writes).toEqual([]);
  await page.getByText("Inspect question and answer", { exact: true }).click();
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(page.getByRole("heading", { name: "Release plan", exact: true })).toBeVisible();
  await expect(page.getByText("Full description", { exact: true })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390);
  await page.screenshot({ path: testInfo.outputPath("question-mobile.png"), fullPage: true });
});

test("question observation replaces CLI guidance with the recorded answer and attribution", async ({ page }) => {
  await page.clock.install();
  const { writes, setState } = await fixture(page, { type: "text" });
  await expect(page.getByLabel("Answer through CLI")).toBeVisible();
  setState({ ...question({ type: "text" }), status: "answered", answer: "", answered_at: now, answered_by_user_id: userID });
  await page.clock.fastForward(5_000);
  await expect(page.getByRole("group", { name: "Recorded answer" })).toContainText("Empty text");
  await expect(detail(page, "Answered by")).toContainText("A user");
  await expect(detail(page, "Answered by").getByTitle(userID)).toBeVisible();
  await expect(page.getByLabel("Answer through CLI")).toHaveCount(0);
  expect(writes).toEqual([]);
  setState({ id: askID, session_id: sessionID, turn_id: turnID, status: "answered", created_at: now, answered_at: now, answered_by_user_id: userID, payload_expired: true });
  await page.reload();
  await expect(page.getByText("The question and answer payloads have expired.")).toBeVisible();
  await expect(page.getByLabel("Recorded answer")).toHaveCount(0);
  await expect(page.getByLabel("Question and answer")).toHaveCount(0);
  await expect(detail(page, "Answered by")).toContainText("A user");
  await expect(detail(page, "Answered by").getByTitle(userID)).toBeVisible();
});

test("withdrawn questions retain the prompt without offering a response", async ({ page }) => {
  const { setState } = await fixture(page, { type: "text" });
  setState({ ...question({ type: "text" }), status: "cancelled", cancelled_at: now });
  await page.reload();
  await expect(detail(page, "Status")).toHaveText("Cancelled");
  await expect(page.getByLabel("Answer through CLI")).toHaveCount(0);
  await expect(page.locator("form")).toHaveCount(0);
});

test("the Question breadcrumb leads through Sessions to its Session", async ({ page }) => {
  await scope(page);
  await page.route(`**${path}**`, route => route.fulfill({ json: question({ type: "text" }) }));
  await page.goto(url);
  const crumbs = page.getByRole("navigation", { name: "Breadcrumb" }).getByRole("link");
  await expect(crumbs).toHaveText(["Sessions", "Session"]);
  await expect(crumbs.nth(0)).toHaveAttribute("href", "/sessions");
  await expect(crumbs.nth(1)).toHaveAttribute("href", `/sessions/${sessionID}`);
});
