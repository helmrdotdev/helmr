import { expect, test } from "@playwright/test";
import { createAgentCatalog, projectID } from "./fixtures/agent-catalog";

test("Console inspects a Computer and all secret placement modes without submitting work", async ({ page }) => {
  await page.goto("/dev/login");
  const { base, environmentID } = await createAgentCatalog(page, ["secrets.read", "secrets.create", "secrets.rotate", "secrets.revoke", "computers.read", "commands.read_output", "computers.execute", "commands.cancel", "computers.create", "computers.wake", "computers.hibernate", "computers.delete", "templates.read", "secrets.read"]);
  const response = await page.request.post(`${base}/secrets`, {
    data: { secret: `browser-${crypto.randomUUID()}`, value: "synthetic-browser-only", idempotency_key: crypto.randomUUID() },
  });
  expect(response.ok(), await response.text()).toBeTruthy();
  const secret = await response.json();
  const created = await page.request.post(`${base}/computers`, {
    data: { template: "browser-computer", secrets: [
      { secret: secret.secret, env: { name: "GH_TOKEN", mode: "protected", allowedOrigins: ["https://api.github.com"] } },
      { secret: secret.secret, env: { name: "PGPASSWORD", mode: "raw" } },
      { secret: secret.secret, file: { path: "/run/secrets/client.key" } },
    ], idempotency_key: crypto.randomUUID() },
  });
  expect(created.ok(), await created.text()).toBeTruthy();
  const computer = await created.json();
  const mutations: string[] = [];
  page.on("request", request => { if (new URL(request.url()).pathname.startsWith(base) && request.method() !== "GET") mutations.push(request.method()); });
  await page.goto(`/computers?project=${projectID}&environment=${environmentID}`);
  await expect(page.getByRole("link", { name: computer.id.slice(-12), exact: true })).toBeVisible();
  await page.getByRole("link", { name: computer.id.slice(-12), exact: true }).click();
  await page.getByRole("tablist", { name: "Computer sections" }).getByRole("tab", { name: /^Secret placements/ }).click();
  await expect(page.getByText("Protected env", { exact: true })).toBeVisible();
  await expect(page.getByText("Raw env", { exact: true })).toBeVisible();
  await expect(page.getByText("Raw file", { exact: true })).toBeVisible();
  await expect(page.getByText("https://api.github.com", { exact: true })).toBeVisible();
  await expect(page.getByText("Use the CLI to run commands or delete this computer.")).toBeVisible();
  await expect(page.locator("form")).toHaveCount(0);
  expect(mutations).toEqual([]);
});
