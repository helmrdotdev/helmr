import { expect, test, type Page } from "@playwright/test";
const projectID = "00000000-0000-7000-8000-000000000301";
const environmentID = "00000000-0000-7000-8000-000000000403";
const sessionID = "00000000-0000-7000-8000-000000000799";
const turnID = "00000000-0000-7000-8000-000000000798";
const askID = "00000000-0000-7000-8000-000000000797";
const userID = "00000000-0000-7000-8000-000000000796";
const base = `/api/projects/${projectID}/environments/${environmentID}`;
const path = `${base}/sessions/${sessionID}/turns/${turnID}/asks/${askID}`;
const url = `/sessions/${sessionID}/turns/${turnID}/asks/${askID}?project_id=${projectID}&environment_id=${environmentID}`;
const now = "2026-10-06T00:00:00Z";
async function scope(page: Page) {
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
    await route.fulfill({ json });
  });
}

function question(control: unknown) {
  return { id: askID, session_id: sessionID, turn_id: turnID, status: "pending", created_at: now,
    prompt: [{ type: "text", text: "<script>do not execute</script>Choose an answer" }, { type: "json", value: { context: null } }], answer_control: control };
}
async function fixture(page: Page, control: unknown) {
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

test("pending questions expose complete content and exact CLI commands as read-only guidance", async ({ page }) => {
  const control = { type: "choice", multiple: true, allowText: true, options: [
    { id: "a", label: "First", description: "Full description", value: { v: 1 } }, { id: "b", label: "Second", value: null },
  ] };
  const { writes } = await fixture(page, control);
  expect(JSON.parse(await page.getByLabel("Answer control").innerText())).toEqual(control);
  const origin = new URL(page.url()).origin;
  const guide = page.getByRole("region", { name: "Answer through CLI" });
  await expect(guide).toContainText(`helmr login '${origin}'`);
  await expect(guide).toContainText(`helmr --api-url '${origin}' session turn ask get '${sessionID}' '${turnID}' '${askID}' --project '${projectID}' --env '${environmentID}' --json`);
  await expect(guide).toContainText(`respond '${sessionID}' '${turnID}' '${askID}'`);
  await expect(guide).toContainText("--answer-file answer.json --response-id RESPONSE_ID");
  await expect(guide).toContainText("Reuse the same ID and unchanged answer");
  await expect(page.locator("form")).toHaveCount(0);
  expect(writes).toEqual([]);
});

test("question observation replaces CLI guidance with the recorded answer and attribution", async ({ page }) => {
  await page.clock.install();
  const { writes, setState } = await fixture(page, { type: "text" });
  await expect(page.getByLabel("Answer through CLI")).toBeVisible();
  setState({ ...question({ type: "text" }), status: "responded", answer: "", responded_at: now, responded_by_user_id: userID });
  await page.clock.fastForward(5_000);
  await expect(page.getByLabel("Recorded answer")).toHaveText('""');
  await expect(page.getByText(`Answered by user: ${userID}`)).toBeVisible();
  await expect(page.getByLabel("Answer through CLI")).toHaveCount(0);
  expect(writes).toEqual([]);
  setState({ id: askID, session_id: sessionID, turn_id: turnID, status: "responded", created_at: now, responded_at: now, responded_by_user_id: userID, payload_expired: true });
  await page.reload();
  await expect(page.getByText("The question and answer payloads have expired.")).toBeVisible();
  await expect(page.getByLabel("Recorded answer")).toHaveCount(0);
  await expect(page.getByLabel("Answer control")).toHaveCount(0);
  await expect(page.getByText(`Answered by user: ${userID}`)).toBeVisible();
});

test("withdrawn questions retain the prompt without offering a response", async ({ page }) => {
  const { setState } = await fixture(page, { type: "text" });
  setState({ ...question({ type: "text" }), status: "cancelled", cancelled_at: now });
  await page.reload();
  await expect(page.getByText("Status: cancelled", { exact: true })).toBeVisible();
  await expect(page.getByLabel("Answer through CLI")).toHaveCount(0);
  await expect(page.locator("form")).toHaveCount(0);
});
