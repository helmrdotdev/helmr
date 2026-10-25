import { expect, test, type Page } from "@playwright/test";

// The Overview for the people who build and run an Environment's agents,
// against a synthetic API fixture.

const projectID = "acme-app";
const environmentID = "preview";
const base = `/api/projects/${projectID}/environments/${environmentID}`;
const now = Date.now();
const ago = (minutes: number) => new Date(now - minutes * 60_000).toISOString();
const id = (suffix: string) => `00000000-0000-7000-8000-${suffix.padStart(12, "0")}`;
const deploymentID = id("501");
const previousID = id("500");
const sessions = { running: id("a01"), held: id("a02"), queued: id("a03"), failed: id("a04"), asked: id("a05") };
const turns = { failed: id("b04"), asked: id("b05") };
const hours = (counts: number[]) => [...Array(24 - counts.length).fill(0), ...counts];

type Item = Record<string, unknown> & { kind: string; id: string };
const question: Item = { kind: "question", id: id("c05"), at: ago(180), agent: "support", session_id: sessions.asked, turn_id: turns.asked, prompt: "Ship the release today?" };
const hold: Item = { kind: "hold", id: id("e02"), at: ago(240), agent: "deployer", session_id: sessions.held, reason: "Approve the production release" };
const lost: Item = { kind: "slack_reauthorization", id: id("d01"), at: ago(90), agent: "support", workspace_name: "Acme" };

type State = {
  permissions: string[];
  items: Item[];
  failures: Record<string, unknown>[];
  waiting: boolean;
  current: boolean;
  agents: boolean;
  writes: string[];
};

const overview = (state: State) => ({
  turns: { total: 132, failed: state.failures.length ? 11 : 2 },
  previous: { total: 120, failed: 2 },
  agents: [
    { agent: "support", active: 1, turns: 71, failed: 1, last_turn_at: ago(4), hourly: hours([3, 4, 5, 6]), hourly_failed: hours([0, 1, 0, 0]) },
    { agent: "deployer", active: 2, turns: 38, failed: 0, last_turn_at: ago(12), hourly: hours([1, 2]), hourly_failed: hours([0, 0]) },
  ],
  active: { count: 3, running: 1, queued: 1, held: 1, sessions: [
    { id: sessions.held, agent: "deployer", state: "held", since: ago(240), origin: "trigger" },
    { id: sessions.running, agent: "support", state: "running", since: ago(2), origin: "slack", slack_channel_id: "C123" },
    { id: sessions.queued, agent: "deployer", state: "queued", since: ago(1), origin: "api_key" },
  ] },
  failures: state.failures,
  deployment: { id: deploymentID, turns: { total: 96, failed: 2 }, older_open_sessions: 1 },
});

const computer = (suffix: string, residency: string, minutes: number, extra: Record<string, unknown> = {}) => ({
  id: id(suffix), template: "base", deployment_id: deploymentID, status: "available", execution_control: null, execution_wait: null, residency,
  last_activity_at: ago(minutes), created_at: ago(900), updated_at: ago(minutes), ...extra,
});

async function fixture(page: Page, overrides: Partial<State> = {}): Promise<State> {
  const state: State = {
    permissions: ["sessions.read", "computers.read", "deployments.read"],
    items: [],
    failures: [],
    waiting: false,
    current: true,
    agents: true,
    writes: [],
    ...overrides,
  };
  const environment = { environment: environmentID, project: projectID, name: "Preview", color_hex: "#06B6D4", is_default: true, created_at: ago(0), updated_at: ago(0) };
  const project = { project: projectID, name: "Acme App", default_region_id: "local", is_default: true, environments: [environment], created_at: ago(0), updated_at: ago(0) };
  const deployment = (did: string, version: string, minutes: number) => ({ id: did, version, bundle_digest: `sha256:${"b".repeat(64)}`, created_at: ago(minutes), preparation_state: "ready" });
  await page.route("**/api/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    const method = route.request().method();
    if (method !== "GET") state.writes.push(`${method} ${path}`);
    let json: unknown;
    if (path === "/api/me") json = { user_id: "local", display_name: "Local", org_id: "org", role: "member", permissions: state.permissions, admin: false, organization_required: false, project_required: false };
    else if (path === "/api/projects") json = { projects: [project] };
    else if (path === `/api/projects/${projectID}`) json = project;
    else if (path === base) json = environment;
    else if (path === `${base}/attention`) {
      const of = (kind: string) => state.items.filter((item) => item.kind === kind).length;
      json = { items: state.items, counts: { question: of("question"), hold: of("hold"), failed_turn: 0, slack_reauthorization: of("slack_reauthorization") } };
    } else if (path === `${base}/overview`) {
      if (!state.permissions.includes("sessions.read")) { await route.fulfill({ status: 403, json: { error: { code: "forbidden", message: "denied" } } }); return; }
      json = overview(state);
    } else if (path === `${base}/computers`) json = { computers: [
      computer("f01", "parked", 120),
      computer("f02", "running", 2),
      computer("f03", state.waiting ? "cold" : "parked", 30, state.waiting ? { execution_wait: { reason: "environment_limit" } } : {}),
      computer("f04", "unavailable", 800, { status: "deleted" }),
    ] };
    else if (path === `${base}/deployments/current`) {
      if (!state.current) { await route.fulfill({ status: 404, json: { error: { code: "no_current_deployment", message: "none" } } }); return; }
      json = deployment(deploymentID, "v2", 200);
    } else if (path === `${base}/deployments`) json = { deployments: state.current ? [deployment(deploymentID, "v2", 200), deployment(previousID, "v1", 3000)] : [] };
    else if (path === `${base}/deployments/${deploymentID}/events`) json = { events: [{ id: "1", kind: "deployment.promoted", message: "Deployment promoted", at: ago(125), attributes: {} }] };
    else if (path === `${base}/agents`) json = { deployment_id: deploymentID, agents: state.agents ? [{ agent: "support", template: "support" }] : [] };
    else if (path === `${base}/secrets`) json = { secrets: [] };
    else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } }); return; }
    await route.fulfill({ json });
  });
  return state;
}

const open = (page: Page) => page.goto(`/?project=${projectID}&environment=${environmentID}`);
const attention = (page: Page) => page.getByRole("region", { name: /^Needs attention/ });
const panel = (page: Page, name: string) => page.getByRole("region", { name, exact: true });
const rows = (page: Page, name: string) => panel(page, name).getByRole("listitem");

test("on a normal day the Overview shows the environment's health without Needs attention", async ({ page }) => {
  const state = await fixture(page, { items: [{ ...question, at: ago(20) }] });
  await open(page);
  await expect(page.getByRole("heading", { name: "Overview", exact: true })).toBeVisible();
  await expect(panel(page, "Agents")).toBeVisible();
  await expect(attention(page)).toHaveCount(0);

  await expect(page.getByText("1 queued · 1 held")).toBeVisible();
  await expect(page.getByText("1.5% failed · 1.7% the day before")).toBeVisible();
  await expect(page.getByText("2 sleeping · none waiting")).toBeVisible();
  await expect(page.getByText(/^Oldest 20 minutes ago$/)).toBeVisible();

  await expect(rows(page, "Agents")).toHaveCount(2);
  await expect(rows(page, "Agents").first()).toContainText("1 active");
  await expect(rows(page, "Agents").first()).toContainText("71 turns");
  // Titled lists carry their own labels: no column header under a title.
  await expect(page.getByRole("main").getByRole("columnheader")).toHaveCount(0);
  await expect(rows(page, "Agents").first().getByRole("link", { name: "support", exact: true })).toHaveAttribute("href", "/agents/support");

  const running = rows(page, "Running now");
  await expect(running).toHaveCount(3);
  await expect(running.nth(0)).toContainText("Trigger");
  await expect(running.nth(0)).toContainText("Held");
  await expect(running.nth(1)).toContainText("Slack channel");
  await expect(running.nth(2)).toContainText("API key");
  await expect(running.nth(1).getByRole("link", { name: "000000000a01", exact: true })).toHaveAttribute("href", `/sessions/${sessions.running}`);

  const deployment = panel(page, "Current deployment");
  await expect(deployment.getByRole("link", { name: "v2", exact: true })).toHaveAttribute("href", `/deployments/${deploymentID}`);
  await expect(deployment).toContainText("Promoted 2 hours ago");
  await expect(deployment).toContainText("96");
  await expect(rows(page, "Recent deployments")).toHaveCount(2);
  await expect(rows(page, "Recent deployments").first()).toContainText("Current");

  // Deleted computers are left out; awake ones come before sleeping ones.
  const machines = rows(page, "Computers");
  await expect(machines).toHaveCount(3);
  await expect(machines.nth(0)).toContainText("Awake");
  expect(state.writes).toEqual([]);
});

test("Needs attention appears on top with one row per cause, most urgent first", async ({ page }) => {
  await fixture(page, {
    waiting: true,
    items: [hold, lost, question],
    failures: [{ agent: "support", error_code: "tool_error", count: 9, last_hour: 4, first_at: ago(50), last_at: ago(1), last_session_id: sessions.failed, last_turn_id: turns.failed, last_turn_sequence: 12 }],
  });
  await open(page);
  const region = attention(page);
  await expect(region).toBeVisible();
  const items = region.getByRole("listitem");
  await expect(items).toHaveCount(5);
  const expected: [string, string, string][] = [
    ["support is failing · 4 turns in the last hour", "Inspect", `/sessions/${sessions.failed}#turn-${turns.failed}`],
    ["1 computer is waiting for capacity", "View", "/computers"],
    ["A session is held", "Open", `/sessions/${sessions.held}`],
    ["Slack connection needs reauthorization", "Reauthorize", "/agents/support/slack"],
    ["1 question unanswered for over 2 hours", "Answer", `/sessions/${sessions.asked}/turns/${turns.asked}/asks/${question.id}`],
  ];
  for (const [index, [title, action, href]] of expected.entries()) {
    await expect(items.nth(index)).toContainText(title);
    await expect(items.nth(index).getByRole("link", { name: action, exact: true })).toHaveAttribute("href", href);
  }
  // It sits above the numbers; the rest of the page keeps its place.
  const [top, tiles] = await Promise.all([region.boundingBox(), page.getByText("Turns · 24h").first().boundingBox()]);
  expect(top!.y).toBeLessThan(tiles!.y);
  await expect(rows(page, "Computers").first()).toContainText("Waiting for capacity");
});

test("without session access the Overview still shows computers and deployments", async ({ page }) => {
  await fixture(page, { permissions: ["computers.read", "deployments.read"] });
  await open(page);
  await expect(panel(page, "Computers")).toBeVisible();
  await expect(page.getByText("No access to sessions").first()).toBeVisible();
  // Each section keeps its place and says why it is empty.
  await expect(panel(page, "Agents")).toContainText("You do not have access to sessions in this environment.");
  await expect(panel(page, "Current deployment")).toBeVisible();
});

test("before the first Deployment the Overview shows setup for this tab's scope", async ({ page }) => {
  await fixture(page, { current: false, permissions: ["sessions.read", "deployments.read", "deployments.publish", "deployments.promote"] });
  await open(page);
  const setup = page.getByRole("region", { name: "Set up" });
  await expect(setup.getByLabel("Setup prompt")).toContainText(`Project: ${projectID}\nEnvironment: ${environmentID}`);
  await expect(panel(page, "Agents")).toHaveCount(0);
});

test("a current Deployment without agents still shows setup, with Needs attention above it", async ({ page }) => {
  await fixture(page, { agents: false, items: [hold], permissions: ["sessions.read", "deployments.read", "deployments.publish", "deployments.promote"] });
  await open(page);
  await expect(page.getByRole("region", { name: "Set up" })).toBeVisible();
  await expect(attention(page)).toBeVisible();
});
