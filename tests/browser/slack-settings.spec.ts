import { expect, test, type Page } from "@playwright/test";

const now = "2026-10-09T00:00:00Z";
const project = "00000000-0000-7000-8000-000000000301";
const environment = "00000000-0000-7000-8000-000000000403";
const endpoint = `/api/projects/${project}/environments/${environment}/agents/demo-agent/slack`;
async function openAgent(page: Page) {
  await page.goto("/dev/login");
  await page.goto("/settings/environments");
  const demo = page.getByRole("row").filter({ hasText: "Demo" });
  await demo.getByRole("button", { name: "Use", exact: true }).click();
  await expect(demo.getByText("Current", { exact: true })).toBeVisible();
  await page.goto("/deployments/00000000-0000-7000-8000-000000000501");
  await page.getByRole("button", { name: "Slack connection", exact: true }).click();
}
const publication = (status: string, configured = true) => ({
  id: "publication-A", registration_id: "registration-A", installation_id: status === "setup_incomplete" ? null : "installation-A",
  status, credentials_configured: configured, client_id: configured ? "client-A" : null,
  app_id: "app-A", team_id: "team-A", workspace_name: "Workspace A", app_name: "Dedicated Agent", app_icon_url: null,
});

test("Agent connection setup saves credentials and authorizes only its exact generation", async ({ page }, testInfo) => {
  let current: ReturnType<typeof publication> | null = null;
  const writes: { path: string; body: unknown }[] = [];
  await page.route(`**${endpoint}**`, async route => {
    const request = route.request(), path = new URL(request.url()).pathname;
    if (request.method() !== "GET") {
      writes.push({ path, body: request.postDataJSON() });
      if (path === endpoint) current = publication("setup_incomplete", false);
      else if (path.endsWith("/credentials")) { current = publication("setup_incomplete"); await route.fulfill({ status: 204 }); return; }
      else if (path.endsWith("/authorize")) { await route.fulfill({ json: { redirect_url: "https://slack.com/oauth/v2/authorize?state=fixture" } }); return; }
    }
    await route.fulfill({ json: { publication: current, manifest: { settings: { event_subscriptions: { request_url: "https://example.test/integrations/slack/apps/registration-A/events" } } }, create_app_url: "https://api.slack.com/apps?new_app=1" } });
  });
  await page.route("https://slack.com/oauth/v2/authorize?**", route => route.fulfill({ contentType: "text/html", body: "<p>OAuth destination fixture</p>" }));
  await openAgent(page);
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("button", { name: "Connect Slack", exact: true }).click();
  await expect(dialog.getByRole("link", { name: "Create app in Slack" })).toBeVisible();
  await expect(dialog.getByRole("button", { name: "Install app in Slack" })).toHaveCount(0);
  await page.screenshot({ path: testInfo.outputPath("agent-slack-setup.png"), fullPage: true });
  await dialog.getByLabel("Client ID", { exact: true }).fill("client-A");
  await dialog.getByLabel("Client secret", { exact: true }).fill("synthetic-client-secret");
  await dialog.getByLabel("Signing secret", { exact: true }).fill("synthetic-signing-secret");
  await dialog.getByRole("button", { name: "Save app credentials" }).click();
  await expect(dialog.getByRole("button", { name: "Install app in Slack" })).toBeVisible();
  await expect(dialog.getByLabel("Client secret", { exact: true })).toHaveValue("");
  await expect(dialog.getByLabel("Signing secret", { exact: true })).toHaveValue("");
  await dialog.getByRole("button", { name: "Install app in Slack" }).click();
  await expect(page).toHaveURL("https://slack.com/oauth/v2/authorize?state=fixture");
  expect(writes).toEqual([
    { path: endpoint, body: {} },
    { path: `${endpoint}/publication-A/credentials`, body: { client_id: "client-A", client_secret: "synthetic-client-secret", signing_secret: "synthetic-signing-secret" } },
    { path: `${endpoint}/publication-A/authorize`, body: { return_to: "/deployments/00000000-0000-7000-8000-000000000501" } },
  ]);
});

test("Agent disconnect describes the existing-thread boundary and requires confirmation", async ({ page }, testInfo) => {
  let connected = true;
  const deletions: string[] = [];
  await page.route(`**${endpoint}**`, async route => {
    if (route.request().method() === "DELETE") { deletions.push(new URL(route.request().url()).pathname); connected = false; await route.fulfill({ status: 204 }); return; }
    await route.fulfill({ json: { publication: connected ? publication("connected") : null } });
  });
  await openAgent(page);
  await page.setViewportSize({ width: 390, height: 844 });
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("Dedicated Agent");
  await dialog.getByRole("button", { name: "Disconnect", exact: true }).click();
  await expect(dialog).toContainText("existing threads stay disconnected and schedules require re-promotion");
  expect(deletions).toEqual([]);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.screenshot({ path: testInfo.outputPath("agent-slack-disconnect-mobile.png"), fullPage: true });
  await dialog.getByRole("button", { name: "Disconnect Slack", exact: true }).click();
  await expect(dialog.getByRole("button", { name: "Connect Slack", exact: true })).toBeVisible();
  expect(deletions).toEqual([`${endpoint}/publication-A`]);
});

test("Agent reauthorization keeps its app and workspace identity", async ({ page }) => {
  const starts: unknown[] = [];
  await page.route(`**${endpoint}**`, async route => {
    if (route.request().method() === "POST") { starts.push({ path: new URL(route.request().url()).pathname, body: route.request().postDataJSON() }); await route.fulfill({ json: { redirect_url: "https://slack.com/oauth/v2/authorize?state=repair" } }); }
    else await route.fulfill({ json: { publication: publication("reauthorization_required") } });
  });
  await page.route("https://slack.com/oauth/v2/authorize?**", route => route.fulfill({ contentType: "text/html", body: "<p>OAuth destination fixture</p>" }));
  await openAgent(page);
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("Reauthorize this same app and workspace");
  await expect(dialog.getByLabel("Client ID", { exact: true })).toHaveAttribute("readonly", "");
  await dialog.getByRole("button", { name: "Reauthorize in Slack" }).click();
  await expect(page).toHaveURL("https://slack.com/oauth/v2/authorize?state=repair");
  expect(starts).toEqual([{ path: `${endpoint}/publication-A/authorize`, body: { return_to: "/deployments/00000000-0000-7000-8000-000000000501" } }]);
});

test("Slack callback removes credentials from the URL and reports a changed account", async ({ page }) => {
  const finishes: unknown[] = [];
  await page.route("**/api/slack/installations/finish", async route => {
    finishes.push(route.request().postDataJSON());
    await route.fulfill({ status: 400, json: { error: { code: "bad_request", message: "the Slack authorization flow expired or account changed; start again" } } });
  });
  await page.goto("/auth/slack/callback?state=fixture-state&code=fixture-code");
  await expect(page).toHaveURL("/auth/slack/callback");
  await expect(page.getByRole("heading", { name: "Slack connection needs attention" })).toBeVisible();
  await expect(page.getByText("Sign in with the account and organization", { exact: false })).toBeVisible();
  expect(finishes).toEqual([{ state: "fixture-state", code: "fixture-code", error: "" }]);
});

test("Members manage only their Slack identity and explicitly unlink it", async ({ page }) => {
  let linked = true;
  const deletions: string[] = [];
  await page.route("**/api/me", async route => {
    const response = await route.fetch();
    await route.fulfill({ json: { ...await response.json(), role: "viewer", permissions: [] } });
  });
  await page.route("**/api/slack/user-links**", async route => {
    if (route.request().method() === "DELETE") { deletions.push(new URL(route.request().url()).pathname); linked = false; await route.fulfill({ status: 204 }); }
    else await route.fulfill({ json: { links: linked ? [{ team_id: "T1", slack_user_id: "U1", linked_at: now }] : [] } });
  });
  await page.route("**/api/slack/link-workspaces**", route => route.fulfill({ json: { configured: true, workspaces: [{ installation_id: "I1", team_id: "T1", workspace_name: "Research workspace" }] } }));
  await page.goto("/dev/login"); await page.goto("/account/slack");
  await expect(page.getByRole("heading", { name: "Your Slack identity" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Connect workspace", exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "Unlink U1", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("Work already accepted keeps its original user identity and continues");
  expect(deletions).toEqual([]);
  await dialog.getByRole("button", { name: "Unlink identity", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(page.getByText("No Slack identities linked.")).toBeVisible();
  expect(deletions).toEqual(["/api/slack/user-links/T1/U1"]);
});

test("Slack identity proof needs explicit account confirmation and clears callback fragments", async ({ page }, testInfo) => {
  const verifies: unknown[] = [], confirms: unknown[] = [];
  await page.route("**/api/slack/user-links/verify", async route => {
    verifies.push(route.request().postDataJSON());
    await route.fulfill({ json: { identity: { team_id: "T1", slack_user_id: "U1", name: "Slack Person" }, confirmation: "proof-state", helmr_user_id: "user", helmr_display_name: "Helmr Person", helmr_org_name: "Research", workspace_name: "Research workspace" } });
  });
  await page.route("**/api/slack/user-links/confirm", async route => { confirms.push(route.request().postDataJSON()); await route.fulfill({ status: 204 }); });
  await page.goto("/dev/login");
  await page.goto("/auth/slack/link#state=flow-state&code=temporary-code");
  await expect(page).toHaveURL("/auth/slack/link");
  await expect(page.getByText("Slack Person", { exact: true })).toBeVisible();
  await expect(page.getByText("Helmr Person", { exact: true })).toBeVisible();
  expect(verifies).toEqual([{ state: "flow-state", code: "temporary-code", error: "" }]);
  expect(confirms).toEqual([]);
  await page.screenshot({ path: testInfo.outputPath("slack-identity-confirmation.png"), fullPage: true });
  await page.getByRole("button", { name: "Confirm these accounts are mine" }).click();
  await expect(page).toHaveURL("/account/slack");
  expect(confirms).toEqual([{ confirmation: "proof-state", confirmed: true }]);
});

test("Slack identity verification cannot redirect an expired account or silently link it", async ({ page }) => {
  let confirmations = 0;
  await page.route("**/api/slack/user-links/verify", route => route.fulfill({ status: 401, json: { error: { code: "unauthorized", message: "Sign in again with the account that started linking." } } }));
  await page.route("**/api/slack/user-links/confirm", route => { confirmations++; return route.fulfill({ status: 204 }); });
  await page.goto("/auth/slack/link#state=expired&code=temporary");
  await expect(page).toHaveURL("/auth/slack/link");
  await expect(page.getByRole("alert")).toContainText("Sign in again with the account that started linking");
  await expect(page.getByRole("button", { name: "Confirm these accounts are mine" })).toHaveCount(0);
  expect(confirmations).toBe(0);
});

test("Slack form callback crosses sites without cookies and resumes the authenticated browser flow", async ({ page, context }) => {
  await page.goto("/dev/login");
  const origin = new URL(page.url()).origin;
  const session = (await context.cookies(origin)).find(cookie => cookie.name.includes("session"));
  expect(session).toBeTruthy();
  await context.addCookies([{ name: "helmr_auth_flow_dev", value: "browser-flow-fixture", url: origin, httpOnly: true, sameSite: "Lax" }]);
  let callbackCookie = "not-observed", verificationCookie = "not-observed";
  await page.route("**/api/slack/user-links/callback", async route => {
    callbackCookie = (await route.request().allHeaders())["cookie"] ?? "";
    await route.continue();
  });
  await page.route("**/api/slack/user-links/verify", async route => {
    verificationCookie = (await route.request().allHeaders())["cookie"] ?? "";
    expect(route.request().postDataJSON()).toEqual({ state: "flow-state", code: "bridge-code", error: "" });
    await route.fulfill({ json: { identity: { team_id: "T1", slack_user_id: "U1", name: "Slack Person" }, confirmation: "proof-state", helmr_user_id: "user", helmr_display_name: "Helmr Person", helmr_org_name: "Research", workspace_name: "Research workspace" } });
  });
  await page.route("https://slack.example.test/identity", route => route.fulfill({ contentType: "text/html", body: `<form method="post" action="${origin}/api/slack/user-links/callback"><input type="hidden" name="state" value="flow-state"><input type="hidden" name="code" value="bridge-code"><button>Return identity proof</button></form>` }));
  await page.goto("https://slack.example.test/identity");
  await page.getByRole("button", { name: "Return identity proof" }).click();
  await expect(page).toHaveURL(`${origin}/auth/slack/link`);
  await expect(page.getByRole("button", { name: "Confirm these accounts are mine" })).toBeVisible();
  expect(callbackCookie).not.toContain(session!.name);
  expect(callbackCookie).not.toContain("helmr_auth_flow_dev");
  expect(verificationCookie).toContain(`${session!.name}=${session!.value}`);
  expect(verificationCookie).toContain("helmr_auth_flow_dev=browser-flow-fixture");
});
