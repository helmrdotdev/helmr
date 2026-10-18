import { expect, test, type Page } from "@playwright/test";

// The Overview as the Environment's queue of work waiting on a person, against a
// synthetic API fixture.

const projectID = "acme-app";
const environmentID = "preview";
const base = `/api/projects/${projectID}/environments/${environmentID}`;
const now = Date.now();
const ago = (minutes: number) => new Date(now - minutes * 60_000).toISOString();
const id = (suffix: string) => `00000000-0000-7000-8000-${suffix.padStart(12, "0")}`;
const deploymentID = id("501");
const sessions = { held: id("a01"), failed: id("a02"), asked: id("a03"), later: id("a04") };
const turns = { failed: id("b02"), asked: id("b03"), later: id("b04") };
const asks = { oldest: id("c03"), later: id("c04") };
const connection = id("d01");

type Item = Record<string, unknown> & { kind: string; id: string };
const hold: Item = { kind: "hold", id: id("e01"), at: ago(300), agent: "deployer", session_id: sessions.held, reason: "Approve the production release" };
const failed: Item = { kind: "failed_turn", id: turns.failed, at: ago(240), agent: "reviewer", session_id: sessions.failed, turn_id: turns.failed, turn_sequence: 4, error_code: "agent_error" };
const lost: Item = { kind: "slack_reauthorization", id: connection, at: ago(180), agent: "support", workspace_name: "Acme" };
const oldest: Item = { kind: "question", id: asks.oldest, at: ago(120), agent: "support", session_id: sessions.asked, turn_id: turns.asked, prompt: "Ship the release today?" };
const later: Item = { kind: "question", id: asks.later, at: ago(60), agent: "support", session_id: sessions.later, turn_id: turns.later, prompt: "Which plan should the customer get?" };

type State = {
  items: Item[];
  slack: boolean;
  current: boolean;
  agents: boolean;
  writes: string[];
};

function counts(state: State) {
  const of = (kind: string) => state.items.filter((item) => item.kind === kind).length;
  return { question: of("question"), hold: of("hold"), failed_turn: of("failed_turn"), ...(state.slack ? { slack_reauthorization: of("slack_reauthorization") } : {}) };
}

async function fixture(page: Page, overrides: Partial<State> = {}): Promise<State> {
  const state: State = {
    items: [hold, failed, lost, oldest, later],
    slack: true,
    current: true,
    agents: true,
    writes: [],
    ...overrides,
  };
  const environment = { environment: environmentID, project: projectID, name: "Preview", color_hex: "#06B6D4", is_default: true, created_at: ago(0), updated_at: ago(0) };
  const project = { project: projectID, name: "Acme App", default_region_id: "local", is_default: true, environments: [environment], created_at: ago(0), updated_at: ago(0) };
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    const method = route.request().method();
    if (method !== "GET") state.writes.push(`${method} ${path}`);
    const ask = path.match(/\/sessions\/([^/]+)\/turns\/([^/]+)\/asks\/([^/]+)$/);
    let json: unknown;
    if (path === "/api/me") json = { user_id: "local", display_name: "Local", org_id: "org", role: state.slack ? "owner" : "developer", permissions: ["sessions.read"], admin: false, organization_required: false, project_required: false };
    else if (path === "/api/projects") json = { projects: [project] };
    else if (path === `/api/projects/${projectID}`) json = project;
    else if (path === base) json = environment;
    else if (path === `${base}/attention`) json = { items: state.items, counts: counts(state) };
    else if (path === `${base}/deployments/current`) {
      if (!state.current) { await route.fulfill({ status: 404, json: { error: { code: "no_current_deployment", message: "none" } } }); return; }
      json = { id: deploymentID, version: "v1", bundle_digest: `sha256:${"b".repeat(64)}`, created_at: ago(600) };
    }
    else if (path === `${base}/deployments/${deploymentID}/events`) json = { events: [
      { id: "1", kind: "deployment.registered", message: "Deployment registered", at: ago(600), attributes: {} },
      { id: "2", kind: "deployment.promoted", message: "Deployment promoted", at: ago(125), attributes: {} },
    ] };
    else if (path === `${base}/agents`) json = { deployment_id: deploymentID, agents: state.agents ? [{ agent: "support", template: "support" }] : [] };
    else if (path === `${base}/secrets`) json = { secrets: [] };
    else if (ask && method === "GET") json = { id: ask[3], session_id: ask[1], turn_id: ask[2], status: "pending", created_at: ago(120), question: { type: "text", prompt: [{ type: "text", text: "Ship the release today?" }] } };
    else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } }); return; }
    await route.fulfill({ json });
  });
  return state;
}

const queue = (page: Page) => page.getByRole("list", { name: "Waiting items" });
// The queue's items as each names itself: kind and summary.
const itemNames = (page: Page) => queue(page).getByRole("listitem").evaluateAll((items) => items.map((item) => item.getAttribute("aria-label")));
const overviewTab = (page: Page) => page.getByRole("navigation", { name: "Sections" }).getByRole("link", { name: /^Overview/ });
const filter = (page: Page) => page.getByRole("group", { name: "Filter by kind" });

test("the Overview queues every kind oldest first with its context, links and action", async ({ page }) => {
  await fixture(page);
  await page.goto(`/?project=${projectID}&environment=${environmentID}`);
  await expect(page.getByRole("heading", { name: "Overview", exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: "000000000501", exact: true })).toHaveAttribute("href", `/deployments/${deploymentID}`);
  await expect(page.getByText(/Promoted 2 hours ago/)).toBeVisible();
  await expect(overviewTab(page)).toHaveAccessibleName("Overview, 5 waiting");
  // The current section is the bar's bordered box, not an underline, with its
  // count badge inside the 26px box clear of every edge.
  const tab = await overviewTab(page).evaluate((link) => {
    const box = link.getBoundingClientRect();
    const badge = link.querySelector("span")!.getBoundingClientRect();
    const style = getComputedStyle(link);
    return {
      height: box.height,
      border: style.borderTopColor,
      underline: getComputedStyle(link, "::after").content,
      clear: Math.min(badge.top - box.top, box.bottom - badge.bottom, box.right - badge.right) - parseFloat(style.borderTopWidth),
    };
  });
  expect(tab.height).toBe(26);
  expect(tab.border).not.toBe("rgba(0, 0, 0, 0)");
  expect(tab.underline).toBe("none");
  expect(tab.clear).toBeGreaterThan(2);

  const items = queue(page).getByRole("listitem");
  await expect(items).toHaveCount(5);
  await expect.poll(() => itemNames(page)).toEqual([
    "Held: Approve the production release",
    "Failed: Turn #4 failed: agent_error",
    "Connections: Slack connection lost authorization in Acme",
    "Questions: Ship the release today?",
    "Questions: Which plan should the customer get?",
  ]);
  const actions: [number, string, string][] = [
    [0, "Review", `/sessions/${sessions.held}`],
    [1, "Inspect", `/sessions/${sessions.failed}#turn-${turns.failed}`],
    [2, "Reauthorize", "/agents/support/slack"],
    [3, "Answer", `/sessions/${sessions.asked}/turns/${turns.asked}/asks/${asks.oldest}`],
    [4, "Answer", `/sessions/${sessions.later}/turns/${turns.later}/asks/${asks.later}`],
  ];
  for (const [index, name, href] of actions) {
    await expect(items.nth(index).getByRole("link", { name, exact: true })).toHaveAttribute("href", href);
  }
  await expect(items.nth(0).getByRole("link", { name: "deployer", exact: true })).toHaveAttribute("href", "/agents/deployer");
  await expect(items.nth(0).getByRole("link", { name: "000000000a01", exact: true })).toHaveAttribute("href", `/sessions/${sessions.held}`);
  await expect(items.nth(2).getByRole("link", { name: "support", exact: true })).toHaveAttribute("href", "/agents/support");
  await expect(items.nth(0).getByText("5 hours ago")).toBeVisible();
  // The Overview no longer repeats the Sessions list.
  await expect(page.getByRole("heading", { name: "Recent Sessions" })).toHaveCount(0);
});

test("the kind filter shows counts for each kind and narrows the queue", async ({ page }) => {
  await fixture(page);
  await page.goto(`/?project=${projectID}&environment=${environmentID}`);
  const buttons = filter(page).getByRole("button");
  await expect(buttons).toHaveText(["All 5", "Questions 2", "Held 1", "Failed 1", "Connections 1"]);
  await expect(buttons.first()).toHaveAttribute("aria-pressed", "true");
  await filter(page).getByRole("button", { name: /^Questions/ }).click();
  await expect.poll(() => itemNames(page)).toEqual(["Questions: Ship the release today?", "Questions: Which plan should the customer get?"]);
  await filter(page).getByRole("button", { name: /^Failed/ }).click();
  await expect.poll(() => itemNames(page)).toEqual(["Failed: Turn #4 failed: agent_error"]);
  await filter(page).getByRole("button", { name: /^All/ }).click();
  await expect(queue(page).getByRole("listitem")).toHaveCount(5);
});

test("people who do not manage Slack have no Connections kind", async ({ page }) => {
  await fixture(page, { slack: false, items: [hold, oldest] });
  await page.goto(`/?project=${projectID}&environment=${environmentID}`);
  await expect(filter(page).getByRole("button")).toHaveText(["All 2", "Questions 1", "Held 1", "Failed 0"]);
  await expect(overviewTab(page)).toHaveAccessibleName("Overview, 2 waiting");
});

test("Questions are answered from their own page; the Overview never sends an answer", async ({ page }) => {
  const state = await fixture(page);
  await page.goto(`/?project=${projectID}&environment=${environmentID}`);
  const questions = queue(page).getByRole("listitem", { name: /^Questions:/ });
  await expect(questions).toHaveCount(2);
  await expect(questions.nth(0).getByRole("link", { name: "Answer", exact: true })).toHaveAttribute("href", `/sessions/${sessions.asked}/turns/${turns.asked}/asks/${asks.oldest}`);
  await expect(questions.nth(1).getByRole("link", { name: "Answer", exact: true })).toHaveAttribute("href", `/sessions/${sessions.later}/turns/${turns.later}/asks/${asks.later}`);
  await expect(page.locator("main form")).toHaveCount(0);
  await expect(page.locator("main textarea, main input[type=radio], main input[type=checkbox]")).toHaveCount(0);
  await expect(page.getByRole("region", { name: "Waiting on you" }).getByText("Oldest first", { exact: true })).toBeVisible();
  await questions.nth(0).getByRole("link", { name: "Answer", exact: true }).click();
  await expect(page).toHaveURL(`/sessions/${sessions.asked}/turns/${turns.asked}/asks/${asks.oldest}`);
  expect(state.writes).toEqual([]);
});

test("with nothing waiting the Overview says so and links to Sessions", async ({ page }) => {
  await fixture(page, { items: [] });
  await page.goto(`/?project=${projectID}&environment=${environmentID}`);
  await expect(page.getByText("Nothing is waiting on you.")).toBeVisible();
  await expect(page.getByRole("link", { name: "View sessions", exact: true })).toHaveAttribute("href", "/sessions");
  await expect(overviewTab(page)).toHaveAccessibleName("Overview");
  await expect(page.getByRole("region", { name: /^Set up/ })).toHaveCount(0);
});

test("before the first Deployment the Overview shows setup for this tab's scope", async ({ page }) => {
  await fixture(page, { items: [], current: false });
  await page.goto(`/?project=${projectID}&environment=${environmentID}`);
  const setup = page.getByRole("region", { name: "Set up" });
  await expect(setup.getByLabel("Setup prompt")).toContainText(`Project: ${projectID}\nEnvironment: ${environmentID}`);
  await expect(page.getByText("No current deployment")).toBeVisible();
  await expect(page.getByText("Nothing is waiting on you.")).toHaveCount(0);
});

test("a current Deployment without agents still shows setup, with anything waiting below it", async ({ page }) => {
  await fixture(page, { agents: false });
  await page.goto(`/?project=${projectID}&environment=${environmentID}`);
  await expect(page.getByRole("region", { name: "Set up" })).toBeVisible();
  await expect(page.getByRole("region", { name: "Waiting on you" })).toBeVisible();
});
