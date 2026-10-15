import { expect, test, type Page } from "@playwright/test";

const now = "2026-10-09T00:00:00Z";
const project = "console-demo";
const environment = "demo";
const endpoint = `/api/projects/${project}/environments/${environment}/agents/demo-agent/slack`;
const slackTab = "/agents/demo-agent/slack";
async function openAgent(page: Page) {
  await page.goto("/dev/login");
  await page.goto("/settings/environments");
  const demo = page.getByRole("row").filter({ hasText: "Demo" });
  await demo.getByRole("button", { name: "Actions for Demo", exact: true }).click();
  await page.getByRole("menuitem", { name: "Use", exact: true }).click();
  await expect(demo.getByText("Working", { exact: true })).toBeVisible();
  await page.goto("/agents/demo-agent");
  await page.getByRole("tab", { name: "Slack", exact: true }).click();
  await expect(page).toHaveURL(slackTab);
  return page.getByRole("tabpanel", { name: "Slack" });
}
const connection = (status: string, configured = true) => ({
  id: "connection-A", registration_id: "registration-A", installation_id: status === "setup_incomplete" ? null : "installation-A",
  status, credentials_configured: configured, client_id: configured ? "client-A" : null,
  app_id: "app-A", team_id: "team-A", workspace_name: "Workspace A", app_name: "Dedicated Agent", app_icon_url: null,
});
const currentStep = (panel: ReturnType<Page["getByRole"]>) => panel.getByRole("list", { name: "Slack setup" }).locator("[aria-current=step]");

test("Agent Slack setup walks the steps on the page and authorizes only its exact generation", async ({ page }, testInfo) => {
  let current: ReturnType<typeof connection> | null = null;
  const writes: { path: string; body: unknown }[] = [];
  await page.route(`**${endpoint}**`, async route => {
    const request = route.request(), path = new URL(request.url()).pathname;
    if (request.method() !== "GET") {
      writes.push({ path, body: request.postDataJSON() });
      if (path === endpoint) current = connection("setup_incomplete", false);
      else if (path.endsWith("/credentials")) { current = connection("setup_incomplete"); await route.fulfill({ status: 204 }); return; }
      else if (path.endsWith("/authorize")) { await route.fulfill({ json: { redirect_url: "https://slack.com/oauth/v2/authorize?state=fixture" } }); return; }
    }
    await route.fulfill({ json: { connection: current, manifest: { settings: { event_subscriptions: { request_url: "https://example.test/integrations/slack/apps/registration-A/events" } } }, create_app_url: "https://api.slack.com/apps?new_app=1" } });
  });
  await page.route("https://slack.com/oauth/v2/authorize?**", route => route.fulfill({ contentType: "text/html", body: "<p>OAuth destination fixture</p>" }));
  const panel = await openAgent(page);
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect(currentStep(panel)).toContainText("Create the Slack app");
  await panel.getByRole("button", { name: "Connect Slack", exact: true }).click();

  await expect(panel).toContainText("Slack setup 1 of 4");
  await expect(currentStep(panel)).toContainText("Create the Slack app");
  await expect(panel.getByRole("link", { name: "Create app in Slack" })).toHaveAttribute("href", "https://api.slack.com/apps?new_app=1");
  const manifest = panel.getByLabel("App manifest");
  await manifest.getByRole("button", { name: "Raw", exact: true }).click();
  await expect(manifest).toContainText("registration-A/events");
  await expect(manifest.getByRole("button", { name: "Copy JSON" })).toBeVisible();
  await expect(panel.getByRole("button", { name: "Install app in Slack" })).toHaveCount(0);
  const icons = panel.getByRole("group", { name: "App icon" });
  await expect(icons.getByRole("radio")).toHaveCount(8);
  const download = icons.getByRole("link", { name: "Download icon" });
  const initial = await download.getAttribute("href");
  expect(initial).toMatch(/^\/slack-app-icons\/(violet|teal|orange|blue|pink|yellow)\.png$/);
  await expect(icons.getByRole("radio", { checked: true })).toHaveAttribute("value", initial!.slice("/slack-app-icons/".length, -".png".length));
  await icons.getByRole("radio", { name: "Dark icon" }).locator("..").click();
  await expect(icons.getByRole("radio", { name: "Dark icon" })).toBeChecked();
  await expect(download).toHaveAttribute("href", "/slack-app-icons/dark.png");
  await expect(download).toHaveAttribute("download", "demo-agent-slack-icon.png");
  await page.keyboard.press("ArrowLeft");
  await expect(icons.getByRole("radio", { name: "Light icon" })).toBeChecked();
  await expect(icons.getByRole("radio", { name: "Light icon" })).toBeFocused();
  await expect(download).toHaveAttribute("href", "/slack-app-icons/light.png");
  expect((await page.request.get("/slack-app-icons/dark.png")).headers()["content-type"]).toBe("image/png");
  await page.screenshot({ path: testInfo.outputPath("agent-slack-setup.png"), fullPage: true });

  await panel.getByRole("button", { name: "I created the app" }).click();
  await expect(panel).toContainText("Slack setup 2 of 4");
  await expect(currentStep(panel)).toContainText("Save app credentials");
  await panel.getByLabel("Client ID", { exact: true }).fill("client-A");
  await panel.getByLabel("Client secret", { exact: true }).fill("synthetic-client-secret");
  await panel.getByLabel("Signing secret", { exact: true }).fill("synthetic-signing-secret");
  await panel.getByRole("button", { name: "Save app credentials" }).click();

  await expect(panel).toContainText("Slack setup 3 of 4");
  await expect(currentStep(panel)).toContainText("Install in your workspace");
  await expect(panel.getByLabel("Client secret", { exact: true })).toBeHidden();
  await panel.getByText("Replace app credentials", { exact: true }).click();
  await expect(panel.getByLabel("Client ID", { exact: true })).toHaveAttribute("readonly", "");
  await expect(panel.getByLabel("Client secret", { exact: true })).toHaveValue("");
  await expect(panel.getByLabel("Signing secret", { exact: true })).toHaveValue("");
  await panel.getByRole("button", { name: "Install app in Slack" }).click();
  await expect(page).toHaveURL("https://slack.com/oauth/v2/authorize?state=fixture");
  expect(writes).toEqual([
    { path: endpoint, body: {} },
    { path: `${endpoint}/connection-A/credentials`, body: { client_id: "client-A", client_secret: "synthetic-client-secret", signing_secret: "synthetic-signing-secret" } },
    { path: `${endpoint}/connection-A/authorize`, body: { return_to: slackTab } },
  ]);
});

test("Agent Slack setup can be discarded only after confirming", async ({ page }) => {
  let current: ReturnType<typeof connection> | null = connection("setup_incomplete");
  const deletions: string[] = [];
  await page.route(`**${endpoint}**`, async route => {
    if (route.request().method() === "DELETE") { deletions.push(new URL(route.request().url()).pathname); current = null; await route.fulfill({ status: 204 }); return; }
    await route.fulfill({ json: { connection: current, manifest: {}, create_app_url: "https://api.slack.com/apps?new_app=1" } });
  });
  const panel = await openAgent(page);
  await expect(currentStep(panel)).toContainText("Install in your workspace");
  const zone = panel.getByRole("region", { name: "Discard setup" });
  await expect(zone).toContainText("The app in Slack is not deleted.");
  await zone.getByRole("button", { name: "Discard setup", exact: true }).click();
  const confirmation = page.getByRole("dialog", { name: "Discard setup" });
  await expect(confirmation).toContainText("Discard this setup to release the agent’s connection slot.");
  await confirmation.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(confirmation).toHaveCount(0);
  expect(deletions).toEqual([]);
  await zone.getByRole("button", { name: "Discard setup", exact: true }).click();
  await page.getByRole("dialog", { name: "Discard setup" }).getByRole("button", { name: "Discard setup", exact: true }).click();
  await expect(panel.getByRole("button", { name: "Connect Slack", exact: true })).toBeVisible();
  expect(deletions).toEqual([`${endpoint}/connection-A`]);
});

test("Agent disconnect describes the existing-thread boundary and requires confirmation", async ({ page }, testInfo) => {
  let connected = true;
  const deletions: string[] = [];
  await page.route(`**${endpoint}**`, async route => {
    if (route.request().method() === "DELETE") { deletions.push(new URL(route.request().url()).pathname); connected = false; await route.fulfill({ status: 204 }); return; }
    await route.fulfill({ json: { connection: connected ? connection("connected") : null } });
  });
  const panel = await openAgent(page);
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(panel.getByRole("status").filter({ hasText: "Connected to Workspace A as Dedicated Agent." })).toBeVisible();
  await expect(panel.locator("dl")).toContainText("Dedicated Agent");
  await expect(panel.locator("dl")).toContainText("Workspace A");
  await expect(panel.getByRole("button", { name: "Reauthorize in Slack" })).toBeVisible();
  await panel.getByText("Download app icon", { exact: true }).click();
  await expect(panel.getByRole("group", { name: "App icon" }).getByRole("link", { name: "Download icon" })).toHaveAttribute("href", /^\/slack-app-icons\/[a-z]+\.png$/);
  await panel.getByText("Update app credentials", { exact: true }).click();
  await expect(panel.getByLabel("Client ID", { exact: true })).toHaveValue("client-A");
  const zone = panel.getByRole("region", { name: "Disconnect Slack" });
  await zone.getByRole("button", { name: "Disconnect", exact: true }).click();
  const confirmation = page.getByRole("dialog", { name: "Disconnect Slack" });
  await expect(confirmation).toContainText("existing threads stay disconnected and triggers require re-promotion");
  await expect(confirmation).toContainText("Demo");
  expect(deletions).toEqual([]);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.screenshot({ path: testInfo.outputPath("agent-slack-disconnect-mobile.png"), fullPage: true });
  await confirmation.getByRole("button", { name: "Disconnect Slack", exact: true }).click();
  await expect(confirmation).toHaveCount(0);
  await expect(panel.getByRole("button", { name: "Connect Slack", exact: true })).toBeVisible();
  expect(deletions).toEqual([`${endpoint}/connection-A`]);
});

test("Agent reauthorization keeps its app and workspace identity", async ({ page }) => {
  const starts: unknown[] = [];
  await page.route(`**${endpoint}**`, async route => {
    if (route.request().method() === "POST") { starts.push({ path: new URL(route.request().url()).pathname, body: route.request().postDataJSON() }); await route.fulfill({ json: { redirect_url: "https://slack.com/oauth/v2/authorize?state=repair" } }); }
    else await route.fulfill({ json: { connection: connection("reauthorization_required") } });
  });
  await page.route("https://slack.com/oauth/v2/authorize?**", route => route.fulfill({ contentType: "text/html", body: "<p>OAuth destination fixture</p>" }));
  const panel = await openAgent(page);
  const warning = panel.getByRole("status").filter({ hasText: "Reauthorize this same app and workspace" });
  await expect(warning).toBeVisible();
  await panel.getByText("Update app credentials", { exact: true }).click();
  await expect(panel.getByLabel("Client ID", { exact: true })).toHaveAttribute("readonly", "");
  await warning.getByRole("button", { name: "Reauthorize in Slack" }).click();
  await expect(page).toHaveURL("https://slack.com/oauth/v2/authorize?state=repair");
  expect(starts).toEqual([{ path: `${endpoint}/connection-A/authorize`, body: { return_to: slackTab } }]);
});

test("a connection Slack disconnected can only be removed", async ({ page }) => {
  await page.route(`**${endpoint}**`, route => route.fulfill({ json: { connection: connection("disconnected") } }));
  const panel = await openAgent(page);
  await expect(panel.getByRole("status").filter({ hasText: "Slack no longer accepts this app’s installation." })).toBeVisible();
  await expect(panel.getByRole("button", { name: /Reauthorize|Install app/ })).toHaveCount(0);
  await expect(panel.getByText("Update app credentials", { exact: true })).toHaveCount(0);
  await panel.getByRole("region", { name: "Disconnect Slack" }).getByRole("button", { name: "Disconnect", exact: true }).click();
  await expect(page.getByRole("dialog", { name: "Disconnect Slack" })).toBeVisible();
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
  await page.goto("/dev/login"); await page.goto("/settings/account");
  await expect(page.getByRole("heading", { name: "Your Slack identity" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Connect workspace", exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: /^Unlink U1 in/ }).click();
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
  await expect(page.getByLabel("Accounts")).toContainText("Slack Person in Research workspace");
  await expect(page.getByLabel("Accounts")).toContainText("Helmr Person in Research");
  expect(verifies).toEqual([{ state: "flow-state", code: "temporary-code", error: "" }]);
  expect(confirms).toEqual([]);
  await page.screenshot({ path: testInfo.outputPath("slack-identity-confirmation.png"), fullPage: true });
  await page.getByRole("button", { name: "Confirm these accounts are mine" }).click();
  await expect(page).toHaveURL("/settings/account");
  expect(confirms).toEqual([{ confirmation: "proof-state", confirmed: true }]);
});

test("Slack identity verification cannot redirect an expired account or silently link it", async ({ page }) => {
  let confirmations = 0;
  await page.route("**/api/slack/user-links/verify", route => route.fulfill({ status: 401, json: { error: { code: "unauthorized", message: "Sign in again with the account that started linking." } } }));
  await page.route("**/api/slack/user-links/confirm", route => { confirmations++; return route.fulfill({ status: 204 }); });
  await page.goto("/auth/slack/link#state=expired&code=temporary");
  await expect(page).toHaveURL("/auth/slack/link");
  await expect(page.getByRole("alert")).toHaveText("Your Helmr sign-in expired before linking finished.");
  await expect(page.getByRole("link", { name: "Sign in again", exact: true })).toHaveAttribute("href", `/login?next=${encodeURIComponent("/settings/account")}`);
  const details = page.locator("details").filter({ hasText: "Technical details" });
  await expect(details).not.toHaveAttribute("open", "");
  await details.getByText("Technical details", { exact: true }).click();
  await expect(details).toContainText("unauthorized: Sign in again with the account that started linking.");
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
