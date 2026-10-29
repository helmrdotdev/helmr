import { authenticationResponse, isAuthenticationPath } from "./fixtures/authentication";
import { expect, test } from "@playwright/test";

// This exercises the real Console confirmation with a synthetic API fixture.
// Revocation state and queued-input preservation are covered by the DB tests.
test("Secret revocation explains running Turn interruption and retained pending inputs", async ({ page }) => {
  const projectID = "console-demo";
  const environmentID = "production";
  const secretID = "SYNTHETIC_TOKEN";
  const now = new Date().toISOString();
  const environment = { environment: environmentID, project: projectID, name: "Production", color_hex: "#008000", is_default: true, created_at: now, updated_at: now };
  const project = { project: projectID, name: "Test", default_region_id: "local", is_default: true, environments: [environment], created_at: now, updated_at: now };
  const secret = { secret: secretID, status: "active", created_at: now };
  const base = `/api/projects/${projectID}/environments/${environmentID}`;
  let revocations = 0;
  await page.route("**/api/**", async route => {
    const path = new URL(route.request().url()).pathname;
    let json: unknown;
    if (isAuthenticationPath(path)) json = { user_id: "local", display_name: "Local", org_id: "org", permissions: ["secrets.read", "secrets.revoke"], admin: false, organization_required: false, project_required: false };
    else if (path === "/api/projects") json = { projects: [project] };
    else if (path === `/api/projects/${projectID}`) json = project;
    else if (path === base) json = environment;
    else if (path === `${base}/secrets`) json = { secrets: [secret] };
    else if (path === `${base}/secrets/${secretID}/revoke` && route.request().method() === "POST") {
      revocations++;
      secret.status = "revoked";
      json = secret;
    } else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } }); return; }
    await route.fulfill({ json: authenticationResponse(path,json) });
  });
  await page.goto(`/settings/secrets?project=${projectID}&environment=${environmentID}`);
  for (const accept of [false, true]) {
    await page.getByRole("button", { name: "Actions for SYNTHETIC_TOKEN" }).click();
    await page.getByRole("menuitem", { name: "Revoke", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Revoke secret" });
    await expect(dialog).toContainText("SYNTHETIC_TOKEN");
    await expect(dialog.getByText("Production", { exact: true })).toBeVisible();
    await expect(dialog).toContainText("Affected running turns may be interrupted and fail to complete.");
    await expect(dialog).toContainText("Pending inputs are retained but cannot run on affected computers.");
    if (accept) await dialog.getByRole("button", { name: "Revoke", exact: true }).click();
    else await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
    await expect(dialog).toHaveCount(0);
    if (accept) await expect.poll(() => revocations).toBe(1);
    else expect(revocations).toBe(0);
  }
  await expect(page.getByText("Revoked", { exact: true })).toBeVisible();
});
