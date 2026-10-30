import { isAuthenticationPath, authenticationResponse } from "./fixtures/authentication";
import { expect, test, type Page } from "@playwright/test";
import type { SlackDeliveryPost } from "../../packages/console/src/lib/slack";

const projectID = "console-demo";
const environmentID = "demo";
const sessionID = "00000000-0000-7000-8000-000000000799";
const postID = "00000000-0000-7000-8000-000000000797";
const attemptID = "00000000-0000-7000-8000-000000000795";
const now = "2026-10-09T00:00:00Z";
const base = `/api/projects/${projectID}/environments/${environmentID}`;
const path = `${base}/sessions/${sessionID}`;
const pageURL = `/sessions/${sessionID}?project=${projectID}&environment=${environmentID}`;
const initial = (): SlackDeliveryPost => ({ id: postID, turn_id: null, sequence: 1, role: "intermediate", continuation_ordinal: 0, status: "uncertain", text: "Agent progress that may already be in Slack", payload_expired_at: null, created_at: now, desired_revision: 2, confirmed_revision: 0, method: "chat.postMessage", attempt_id: attemptID, message_ts: null, root_known: true, opening: false, stream_status: "none", disposed_at: null, error: "history_no_match" });

async function fixture(page: Page, supplied?: SlackDeliveryPost[]) {
  const posts = supplied ?? [initial()];
  const writes: { path: string; body: unknown }[] = [];
  await page.route("**/api/**", async route => {
    const url = new URL(route.request().url()), endpoint = url.pathname;
    const environment = { environment: environmentID, project: projectID, name: "Development", color_hex: "#008000", is_default: true, created_at: now, updated_at: now };
    const project = { project: projectID, name: "Test", default_region_id: "local", is_default: true, environments: [environment], created_at: now, updated_at: now };
    if (endpoint.startsWith(`${path}/slack-delivery`) && route.request().method() !== "GET") {
      writes.push({ path: endpoint, body: route.request().postData() });
      await route.fulfill({ status: 405 }); return;
    }
    let json: unknown;
    if (isAuthenticationPath(endpoint)) json = { user_id: "local", display_name: "Local", org_id: "org", is_owner: false, permissions: [], permission_scopes: [{ project: projectID, environment: environmentID, agent: "support", permissions: ["sessions.read"] }], admin: false, organization_required: false, project_required: false };
    else if (endpoint === "/api/projects") json = { projects: [project] };
    else if (endpoint === `/api/projects/${projectID}`) json = project;
    else if (endpoint === base) json = environment;
    else if (endpoint === path) json = { activity: { process: null }, initial_turn: null, id: sessionID, agent: "support", template: "browser", deployment_id: postID, computer_id: postID, root_session_id: sessionID, parent_session_id: null, slack_channel_id: postID, status: "open", created_at: now, holds: [] };
    else if (endpoint === `${path}/turns`) json = { turns: [] };
    else if (endpoint === `${path}/events`) json = { records: [], next_after: 0, has_more: false, retained_after: 0 };
    else if (endpoint === `${path}/slack-delivery`) json = { posts: url.searchParams.has("cursor") ? posts.slice(1) : posts.slice(0, 1), ...(!url.searchParams.has("cursor") && posts.length > 1 ? { next_cursor: "1" } : {}) };
    else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: endpoint } } }); return; }
    await route.fulfill({ json: authenticationResponse(endpoint, json) });
  });
  await page.goto(pageURL);
  await expect(page.getByRole("heading", { name: "Slack delivery", exact: true })).toBeVisible();
  return { writes };
}

test("Slack uncertainty shows automatic confirmation without mutation controls", async ({ page }) => {
  const { writes } = await fixture(page);
  await expect(page.getByText("Checking delivery", { exact: true })).toBeVisible();
  await expect(page.getByText("This does not mean it was never delivered.", { exact: false })).toBeVisible();
  const row = page.getByRole("button", { name: /message 1/ });
  await expect(row).toHaveAttribute("aria-expanded", "true");
  await row.click();
  await expect(row).toHaveAttribute("aria-expanded", "false");
  await expect(page.getByText("This does not mean it was never delivered.", { exact: false })).toBeHidden();
  expect(writes).toEqual([]);
});

test("terminal rate-limited delivery explains uncertainty and preserves retained content", async ({ page }) => {
  await fixture(page, [{ ...initial(), status: "failed", disposed_at: now, error: "history_rate_limited" }]);
  await expect(page.getByText("Delivery ended; remote outcome unconfirmed", { exact: true })).toBeVisible();
  await expect(page.getByText("Slack rate limits exhausted the delivery confirmation attempts.", { exact: true })).toBeVisible();
  await expect(page.getByText("Remaining parts of this message will not be sent. Agent work continues.", { exact: true })).toBeVisible();
  await expect(page.getByText("Checking delivery", { exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: /Check Slack again|Stop delivery/ })).toHaveCount(0);
});

test("readers see unknown roots and retained stream evidence without recovery authority", async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const first = { ...initial(), root_known: false, opening: true, status: "failed", disposed_at: now };
  const second = { ...initial(), id: "00000000-0000-7000-8000-000000000796", sequence: 2, stream_status: "uncertain", method: "chat.startStream", status: "failed", disposed_at: now, text: "Stream evidence still retained" };
  const { writes } = await fixture(page, [first, second]);
  await expect(page.getByText("Later messages are suppressed", { exact: false })).toBeVisible();
  await expect(page.getByText("Send work from the CLI or Slack.", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Load more delivery issues" }).click();
  await page.getByRole("button", { name: /message 2/ }).click();
  await expect(page.getByText("Slack stream termination remains unconfirmed.", { exact: false })).toBeVisible();
  await expect(page.getByRole("button", { name: "Stop delivery", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Check Slack again" })).toHaveCount(0);
  expect(writes).toEqual([]);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await page.screenshot({ path: testInfo.outputPath("slack-delivery-reader.png"), fullPage: true });
});

test("late complete confirmation refines terminal evidence without offering recovery", async ({ page }) => {
  await fixture(page, [{ ...initial(), status: "failed", disposed_at: now, confirmed_revision: 2, message_ts: "123.456" }]);
  await expect(page.getByText("Delivery ended; content confirmed", { exact: true })).toBeVisible();
  await expect(page.getByText("Slack confirmed this message.", { exact: true })).toBeVisible();
  await expect(page.getByText("No matching Slack message was found.", { exact: false })).toHaveCount(0);
  await expect(page.getByRole("button", { name: /Check Slack again|Stop delivery/ })).toHaveCount(0);
});


test("a paced history rate limit is still pending before the confirmation budget ends", async ({ page }) => {
  await fixture(page, [{ ...initial(), error: "history_rate_limited" }]);
  await expect(page.getByText("Slack rate limits are delaying delivery confirmation.", { exact: true })).toBeVisible();
  await expect(page.getByText("Checking delivery", { exact: true })).toBeVisible();
  await expect(page.getByText("Slack rate limits exhausted the delivery confirmation attempts.", { exact: true })).toHaveCount(0);
});
