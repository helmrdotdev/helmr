import { expect, test } from "@playwright/test";
import type { RoleGrant } from "../../packages/console/src/lib/permissions";

test("creators can use new environments immediately and recover revoked access from Overview", async ({ page }) => {
  await page.goto("/dev/login");
  await expect(page).toHaveURL("/");
  const name = `Creator ${crypto.randomUUID().slice(0, 8)}`;
  await page.goto("/projects/new");
  await page.getByLabel("Name", { exact: true }).fill(name);
  const created = page.waitForResponse(response => response.url().endsWith("/api/projects") && response.request().method() === "POST");
  await page.getByRole("button", { name: "Create project", exact: true }).click();
  const response = await created;
  expect(response.ok(), await response.text()).toBeTruthy();
  const project = await response.json();
  await expect(page.getByRole("region", { name: "Set up", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Grant access", exact: true })).toHaveCount(0);
  const organization = await (await page.request.get("/api/organization")).json();
  const member = organization.current_member;
  for (const environment of ["production", "staging"]) {
    const scope = member.permission_scopes.find((scope: { project: string; environment: string; permissions: string[] }) => scope.project === project.project && scope.environment === environment && scope.permissions.includes("secrets.create"));
    expect(scope, `creator has operational access in ${environment}`).toBeTruthy();
  }

  // Exercise dialog creation too: it must refresh the signed-in member's access,
  // so the new Environment is usable without reloading the browser.
  await page.goto(`/settings/environments?project=${project.project}&environment=production`);
  await page.getByRole("button", { name: "New environment", exact: true }).click();
  const newEnvironment = page.getByRole("dialog", { name: `New environment in ${name}`, exact: true });
  await newEnvironment.getByLabel("Name", { exact: true }).fill("Creator extra");
  await newEnvironment.getByRole("button", { name: "Create", exact: true }).click();
  await expect(newEnvironment).toHaveCount(0);
  await page.getByRole("link", { name: "Overview", exact: true }).click();
  await expect(page.getByRole("region", { name: "Set up", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Grant access", exact: true })).toHaveCount(0);

  const scope = `project=${project.project}&environment=creator-extra`;
  const grantsResponse = await page.request.get("/api/role-grants?limit=100");
  const { grants } = await grantsResponse.json() as { grants: RoleGrant[] };
  const grant = grants.find(item => item.project === project.project && item.environment === "creator-extra" && item.member_id === member.id && item.role_name === "Environment Resource Administrator");
  expect(grant).toBeTruthy();
  const removed = await page.request.delete(`/api/role-grants/${grant!.id}`, { headers: { "If-Match": `"${grant!.revision}"` } });
  expect(removed.ok(), await removed.text()).toBeTruthy();
  await page.goto(`/?${scope}`);
  await expect(page.getByRole("region", { name: "Set up", exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "Grant access", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Grant access", exact: true });
  await expect(dialog.getByRole("combobox", { name: "Role", exact: true })).toContainText("Environment Resource Administrator");
  await dialog.getByRole("combobox", { name: "Who", exact: true }).click();
  await page.getByRole("option", { name: member.display_name, exact: true }).click();
  await dialog.getByRole("button", { name: "All, including new ones", exact: true }).click();
  await dialog.getByRole("button", { name: "Grant access", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(page.getByRole("region", { name: "Set up", exact: true })).toBeVisible();
  const secrets = await page.request.get(`/api/projects/${project.project}/environments/creator-extra/secrets`);
  expect(secrets.ok(), await secrets.text()).toBeTruthy();
});
