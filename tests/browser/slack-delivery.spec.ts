import { expect, test, type Page } from "@playwright/test";
import type { SlackDeliveryPost } from "../../packages/console/src/lib/slack";

const projectID = "00000000-0000-7000-8000-000000000301";
const environmentID = "00000000-0000-7000-8000-000000000403";
const sessionID = "00000000-0000-7000-8000-000000000799";
const postID = "00000000-0000-7000-8000-000000000797";
const attemptID = "00000000-0000-7000-8000-000000000795";
const now = "2026-10-09T00:00:00Z";
const base = `/api/projects/${projectID}/environments/${environmentID}`;
const path = `${base}/sessions/${sessionID}`;
const pageURL = `/sessions/${sessionID}?project_id=${projectID}&environment_id=${environmentID}`;
const initial = (): SlackDeliveryPost => ({ id: postID, turn_id: null, sequence: 1, role: "intermediate", continuation_ordinal: 0, status: "uncertain", text: "Agent progress that may already be in Slack", payload_expired_at: null, created_at: now, desired_revision: 2, confirmed_revision: 0, publication_revision: 2, method: "chat.postMessage", attempt_id: attemptID, message_ts: null, root_known: true, opening: false, stream_state: "none", check_paused_at: now, disposed_at: null, disposed_by: null, error: "history_no_match" });

async function fixture(page: Page, role = "owner", supplied?: SlackDeliveryPost[]) {
  let posts = supplied ?? [initial()];
  const writes: { path: string; body: unknown }[] = [];
  let conflict = false;
  await page.route("**/api/**", async route => {
    const url = new URL(route.request().url()), endpoint = url.pathname;
    const environment = { id: environmentID, project_id: projectID, slug: "dev", name: "Development", color_hex: "#008000", is_default: true, created_at: now, updated_at: now };
    const project = { id: projectID, slug: "test", name: "Test", default_region_id: "local", is_default: true, environments: [environment], created_at: now, updated_at: now };
    let json: unknown;
    if (endpoint === "/api/me") json = { user_id: "local", display_name: "Local", org_id: "org", role, permissions: ["sessions.read"], admin: false, organization_required: false, project_required: false };
    else if (endpoint === "/api/projects") json = { projects: [project] };
    else if (endpoint === `/api/projects/${projectID}`) json = project;
    else if (endpoint === base) json = environment;
    else if (endpoint === path) json = { id: sessionID, agent_id: postID, deployment_id: postID, computer_id: postID, root_session_id: sessionID, parent_session_id: null, slack_channel_id: postID, status: "open", created_at: now, holds: [] };
    else if (endpoint === `${path}/turns`) json = { turns: [] };
    else if (endpoint === `${path}/events`) json = { records: [], next_after: 0, has_more: false, retained_after: 0 };
    else if (endpoint === `${path}/slack-delivery`) json = { posts: url.searchParams.has("cursor") ? posts.slice(1) : posts.slice(0, 1), ...(!url.searchParams.has("cursor") && posts.length > 1 ? { next_cursor: "1" } : {}) };
    else if (endpoint.startsWith(`${path}/slack-delivery/`) && route.request().method() === "POST") {
      writes.push({ path: endpoint, body: route.request().postDataJSON() });
      if (conflict) {
        posts = posts.map(p => ({ ...p, publication_revision: 3 })); conflict = false;
        await route.fulfill({ status: 409, json: { error: { code: "conflict", message: "the Slack delivery changed; refresh before trying again" } } }); return;
      }
      posts = posts.map(post => endpoint.includes(post.id) ? endpoint.endsWith("/check") ? { ...post, check_paused_at: null } : { ...post, status: "failed", disposed_at: now, disposed_by: "local", check_paused_at: now } : post);
      await route.fulfill({ status: 204 }); return;
    } else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: endpoint } } }); return; }
    await route.fulfill({ json });
  });
  await page.goto(pageURL);
  await expect(page.getByRole("heading", { name: "Slack delivery", exact: true })).toBeVisible();
  return { writes, conflictNext: () => { conflict = true; } };
}

test("Slack uncertainty can be checked and explicitly abandoned without cancelling the Agent", async ({ page }) => {
  const { writes } = await fixture(page);
  await expect(page.getByText("This does not mean it was never delivered.", { exact: false })).toBeVisible();
  await page.getByRole("button", { name: "Check Slack again" }).click();
  await expect(page.getByText("A Slack delivery check is scheduled.", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Stop delivery", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("This does not stop the Agent.");
  expect(writes).toHaveLength(1);
  await dialog.getByRole("button", { name: "Stop delivery", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(page.getByRole("heading", { name: "Delivery stopped; remote outcome unconfirmed", exact: true })).toBeVisible();
  expect(writes).toEqual([
    { path: `${path}/slack-delivery/${postID}/check`, body: { attempt_id: attemptID, publication_revision: 2 } },
    { path: `${path}/slack-delivery/${postID}/abandon`, body: { attempt_id: attemptID, publication_revision: 2 } },
  ]);
});

test("Slack recovery reports stale evidence and refreshes the selected publication before retry", async ({ page }) => {
  const control = await fixture(page); control.conflictNext();
  await page.getByRole("button", { name: "Stop delivery", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("button", { name: "Stop delivery", exact: true }).click();
  await expect(dialog.getByRole("alert")).toContainText("refresh before trying again");
  await dialog.getByRole("button", { name: "Keep", exact: true }).click();
  await page.reload();
  await page.getByRole("button", { name: "Stop delivery", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Stop delivery", exact: true }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  expect(control.writes.map(w => w.body)).toEqual([
    { attempt_id: attemptID, publication_revision: 2 }, { attempt_id: attemptID, publication_revision: 3 },
  ]);
});

test("readers see unknown roots and retained stream evidence without recovery authority", async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const first = { ...initial(), root_known: false, opening: true, status: "failed", disposed_at: now, disposed_by: "manager" };
  const second = { ...initial(), id: "00000000-0000-7000-8000-000000000796", sequence: 2, stream_state: "uncertain", method: "chat.startStream", status: "failed", disposed_at: now, disposed_by: "manager", text: "Stream evidence still retained" };
  const { writes } = await fixture(page, "viewer", [first, second]);
  await expect(page.getByText("Later messages remain blocked", { exact: false })).toBeVisible();
  await page.getByRole("button", { name: "Load more delivery issues" }).click();
  await expect(page.getByText("Slack stream termination remains unconfirmed.", { exact: false })).toBeVisible();
  await expect(page.getByRole("button", { name: "Stop delivery", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Check Slack again" })).toHaveCount(0);
  expect(writes).toEqual([]);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await page.screenshot({ path: testInfo.outputPath("slack-delivery-reader.png"), fullPage: true });
});
