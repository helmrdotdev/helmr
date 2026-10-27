import { expect, test, type Page } from "@playwright/test";
import { HARNESSES, PROVIDERS } from "../../packages/console/src/features/onboarding/harnesses";

// The Overview's setup panel on the local stack, whose Production Environment
// has no Deployment. Secret reads and writes are answered here so the panel's
// key states can be reached without storing keys.

type SecretState = "missing" | "active" | "revoked";

async function login(page: Page) {
  await page.goto("/dev/login");
  await expect(page).toHaveURL("/");
}

async function secrets(page: Page, initial: SecretState, options: { manage?: boolean; active?: string[] } = {}) {
  const writes: { path: string; body: Record<string, unknown> }[] = [];
  const state = new Map<string, SecretState>((options.active ?? []).map((id) => [id, "active"]));
  await page.route("**/api/projects/*/environments/*/secrets**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    // Simulate a principal without secret read or create permissions.
    if (options.manage === false) {
      await route.fulfill({ status: 403, json: { error: { code: "forbidden", message: "secrets.create is required" } } });
      return;
    }
    if (request.method() === "GET") {
      const listed = Object.values(PROVIDERS).map((provider) => ({ id: provider.secret, status: state.get(provider.secret) ?? initial })).filter((item) => item.status !== "missing");
      await route.fulfill({ json: { secrets: listed.map((item) => ({ secret: item.id, status: item.status, created_at: new Date().toISOString() })) } });
      return;
    }
    const body = request.postDataJSON() as Record<string, unknown>;
    writes.push({ path, body });
    const id = path.endsWith("/rotate") ? decodeURIComponent(path.split("/").at(-2)!) : String(body["secret"]);
    state.set(id, "active");
    await route.fulfill({ json: { secret: id, status: "active", created_at: new Date().toISOString() } });
  });
  if (options.manage === false) {
    await page.route("**/api/me", async (route) => {
      const response = await route.fetch();
      const me = await response.json() as import("../../packages/console/src/lib/auth").Me;
      const withoutSecrets = (permissions: string[]) => permissions.filter(permission => !["secrets.read", "secrets.create"].includes(permission));
      await route.fulfill({ response, json: { ...me, is_owner: false, permissions: withoutSecrets(me.permissions), permission_scopes: me.permission_scopes?.map(scope => ({ ...scope, permissions: withoutSecrets(scope.permissions) })) } });
    });
  }
  return writes;
}

const panel = (page: Page) => page.getByRole("region", { name: "Set up" });
const environmentTrigger = (page: Page) => page.locator("header").getByRole("button", { name: /^Environment:/ });
const prompt = (page: Page) => panel(page).getByLabel("Setup prompt");

test("an Environment without agents walks through agent, provider, key and prompt", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  const writes = await secrets(page, "missing");
  await login(page);
  const origin = new URL(page.url()).origin;

  const setup = panel(page);
  await expect(setup.getByRole("list", { name: "Set up" }).getByRole("listitem")).toHaveCount(4);
  await expect(setup.getByRole("radio")).toHaveCount(4);
  await expect(setup.getByRole("radio", { name: "Claude Code" })).toBeChecked();
  await expect(setup.getByText("Claude Code uses Anthropic.")).toBeVisible();
  await expect(setup.getByText("claude-opus-5-5", { exact: true })).toBeVisible();
  await expect(setup.getByRole("button", { name: "Copy" })).toHaveCount(0);

  await setup.getByText("OpenCode", { exact: true }).click();
  const providers = setup.getByRole("group", { name: "Model provider" });
  await expect(providers.getByRole("button")).toHaveText(["Anthropic", "OpenAI", "OpenCode Go"]);
  await expect(providers.getByRole("button", { name: "Anthropic" })).toHaveAttribute("aria-pressed", "true");
  await providers.getByRole("button", { name: "OpenCode Go" }).click();
  await expect(setup.getByText("https://opencode.ai", { exact: true })).toBeVisible();

  await setup.getByLabel("OPENCODE_GO_API_KEY").fill("test-value");
  await setup.getByRole("button", { name: "Save key" }).click();
  await expect(setup.getByText("Saved", { exact: true })).toBeVisible();
  await expect(setup.getByLabel("OPENCODE_GO_API_KEY")).toHaveCount(0);
  expect(writes).toEqual([{ path: "/api/projects/console-demo/environments/production/secrets", body: expect.objectContaining({ secret: "OPENCODE_GO_API_KEY", value: "test-value" }) }]);

  const expected = `Set up a Helmr agent for me by following\n${origin}/agent-setup/opencode.md?provider=opencode-go\n\nServer: ${origin}\nProject: console-demo\nEnvironment: production`;
  await expect(prompt(page)).toHaveText(expected);
  await setup.getByRole("button", { name: "Copy" }).click();
  await expect(setup.getByRole("button", { name: "Copied" })).toBeVisible();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(expected);
  expect(expected).not.toContain("test-value");
});

test("an active key can be replaced, and the prompt is ready at once", async ({ page }) => {
  const writes = await secrets(page, "active");
  await login(page);
  const setup = panel(page);
  await expect(setup.getByText("is set in this environment.")).toBeVisible();
  await expect(setup.getByRole("button", { name: "Copy" })).toBeVisible();
  await setup.getByRole("button", { name: "Replace key" }).click();
  await setup.getByLabel("ANTHROPIC_API_KEY").fill("replacement");
  await setup.getByRole("button", { name: "Save key" }).click();
  await expect(setup.getByText("Saved", { exact: true })).toBeVisible();
  expect(writes.map((write) => write.path)).toEqual(["/api/projects/console-demo/environments/production/secrets/ANTHROPIC_API_KEY/rotate"]);
});

test("a revoked key blocks the prompt and says why", async ({ page }) => {
  await secrets(page, "revoked");
  await login(page);
  const setup = panel(page);
  await expect(setup.getByText("ANTHROPIC_API_KEY was revoked in this environment, and a revoked ID cannot be used again.", { exact: false })).toBeVisible();
  await expect(setup.getByRole("button", { name: "Copy" })).toHaveCount(0);
  await expect(setup.getByRole("button", { name: "Save key" })).toHaveCount(0);
});

test("someone who cannot manage secrets is told who adds the key and can still copy the prompt", async ({ page }) => {
  await secrets(page, "missing", { manage: false });
  await login(page);
  const setup = panel(page);
  await expect(setup.getByText("A member with Manage secrets adds", { exact: false })).toBeVisible();
  await expect(setup.getByRole("link", { name: "Secrets" })).toHaveAttribute("href", "/settings/secrets?project=console-demo&environment=production");
  await expect(setup.getByRole("button", { name: "Save key" })).toHaveCount(0);
  await expect(setup.getByRole("button", { name: "Copy" })).toBeVisible();
});

test("moving between agents with the keyboard keeps focus as the key's state changes", async ({ page }) => {
  await secrets(page, "missing", { active: ["ANTHROPIC_API_KEY"] });
  await login(page);
  const setup = panel(page);
  await expect(setup.getByRole("button", { name: "Copy" })).toBeVisible();
  await setup.getByRole("radio", { name: "Claude Code" }).focus();
  await page.keyboard.press("ArrowRight");
  await expect(setup.getByRole("radio", { name: "Codex" })).toBeChecked();
  await expect(setup.getByLabel("OPENAI_API_KEY")).toBeVisible();
  await expect(setup.getByRole("radio", { name: "Codex" })).toBeFocused();
});

test("a key typed for one environment is not kept when switching to another", async ({ page }) => {
  await secrets(page, "missing");
  await login(page);
  const choose = async (name: string) => {
    await environmentTrigger(page).click();
    await page.getByRole("dialog", { name: "Choose environment" }).getByRole("option", { name }).click();
    await expect(environmentTrigger(page)).toContainText(name);
    await expect(panel(page).getByLabel("ANTHROPIC_API_KEY")).toBeVisible();
  };
  // Visit Staging first so switching back to it is answered from cache and
  // the panel stays mounted.
  await choose("Staging");
  await choose("Production");
  await panel(page).getByLabel("ANTHROPIC_API_KEY").fill("typed-for-production");
  await choose("Staging");
  await expect(panel(page).getByLabel("Setup prompt")).toContainText("Environment: staging");
  await expect(panel(page).getByLabel("ANTHROPIC_API_KEY")).toHaveValue("");
});

test("the panel fits a phone without horizontal scrolling", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await secrets(page, "missing");
  await login(page);
  await expect(panel(page).getByRole("radio")).toHaveCount(4);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(375);
});

test("the Console's agents and providers match the setup instructions the server renders", async ({ request }) => {
  for (const harness of HARNESSES) {
    for (const binding of harness.bindings) {
      const provider = PROVIDERS[binding.provider];
      const response = await request.get(`/agent-setup/${harness.id}.md?provider=${provider.id}`);
      expect(response.status(), `${harness.id} ${provider.id}`).toBe(200);
      const text = await response.text();
      expect(text, `${harness.id} ${provider.id}`).toContain(`- Harness/template: \`${harness.id}\``);
      expect(text).toContain(`- Provider: \`${provider.id}\``);
      expect(text).toContain(`- Model: \`${binding.model}\``);
      expect(text).toContain(`- Secret ID: \`${provider.secret}\``);
      expect(text).toContain(`- Protected credential destination: \`${provider.origin}\``);
    }
    const fallback = await (await request.get(`/agent-setup/${harness.id}.md`)).text();
    expect(fallback).toContain(`- Provider: \`${harness.bindings[0]!.provider}\``);
  }
});
