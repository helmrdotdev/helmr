import { authenticationResponse, isAuthenticationPath } from "./fixtures/authentication";
import { expect, test } from "@playwright/test";

// This exercises the real Console flow with a synthetic API fixture. Revocation,
// reactivation and the raw-value inventory are covered by the DB tests.
test("Secret revocation explains its effect, keeps Update and links to Computers that may hold the value", async ({ page }) => {
  const projectID = "console-demo";
  const environmentID = "production";
  const secretID = "SYNTHETIC_TOKEN";
  const now = new Date().toISOString();
  const environment = { environment: environmentID, project: projectID, name: "Production", color_hex: "#008000", is_default: true, created_at: now, updated_at: now };
  const project = { project: projectID, name: "Test", default_region_id: "local", is_default: true, environments: [environment], created_at: now, updated_at: now };
  const secret: Record<string, unknown> = { secret: secretID, status: "active", created_at: now };
  const base = `/api/projects/${projectID}/environments/${environmentID}`;
  const computer = (id: string, exposure: string) => ({ id, template: "browser", deployment_id: "00000000-0000-7000-8000-000000000101", status: "available", residency: "cold", execution_control: null, execution_wait: null, created_at: now, updated_at: now, secret_exposure: exposure });
  const received = "00000000-0000-7000-8000-000000000721";
  const image = "00000000-0000-7000-8000-000000000722";
  const computerQueries: string[] = [];
  let revocations = 0;
  let rotations = 0;
  await page.route("**/api/**", async route => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    let json: unknown;
    if (isAuthenticationPath(path)) json = { user_id: "local", display_name: "Local", org_id: "org", permissions: ["secrets.read", "secrets.rotate", "secrets.revoke", "computers.read"], admin: false, organization_required: false, project_required: false };
    else if (path === "/api/projects") json = { projects: [project] };
    else if (path === `/api/projects/${projectID}`) json = project;
    else if (path === base) json = environment;
    else if (path === `${base}/secrets`) json = { secrets: [secret] };
    else if (path === `${base}/secrets/${secretID}/revoke` && route.request().method() === "POST") {
      revocations++;
      Object.assign(secret, { status: "revoked", revoked_at: now });
      json = secret;
    } else if (path === `${base}/secrets/${secretID}/rotate` && route.request().method() === "POST") {
      rotations++;
      expect(route.request().postDataJSON()).toMatchObject({ value: "new-value" });
      Object.assign(secret, { status: "active", rotated_at: now });
      delete secret.revoked_at;
      json = secret;
    } else if (path === `${base}/computers`) {
      computerQueries.push(url.searchParams.get("secret") ?? "");
      json = { computers: url.searchParams.get("secret") === secretID ? [computer(received, "received"), computer(image, "image")] : [] };
    } else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } }); return; }
    await route.fulfill({ json: authenticationResponse(path, json) });
  });
  await page.goto(`/settings/secrets?project=${projectID}&environment=${environmentID}`);
  const actions = page.getByRole("button", { name: "Actions for SYNTHETIC_TOKEN" });
  for (const accept of [false, true]) {
    await actions.click();
    await page.getByRole("menuitem", { name: "Revoke", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Revoke secret" });
    await expect(dialog).toContainText("SYNTHETIC_TOKEN");
    await expect(dialog.getByText("Production", { exact: true })).toBeVisible();
    await expect(dialog).toContainText("Protected requests that use this secret fail until it is updated");
    await expect(dialog).toContainText("raw copies already delivered stay on their computers");
    await expect(dialog).toContainText("Revoke the credential at its provider");
    if (accept) await dialog.getByRole("button", { name: "Revoke", exact: true }).click();
    else await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
    await expect(dialog).toHaveCount(0);
    if (accept) await expect.poll(() => revocations).toBe(1);
    else expect(revocations).toBe(0);
  }
  await expect(page.getByText("Revoked", { exact: true })).toBeVisible();

  // A revoked secret is updated, not recreated; it cannot be revoked again.
  await actions.click();
  await expect(page.getByRole("menuitem", { name: "Revoke", exact: true })).toHaveCount(0);
  await page.getByRole("menuitem", { name: "Update", exact: true }).click();
  const update = page.getByRole("dialog", { name: "Update secret" });
  await update.getByLabel("Value").fill("new-value");
  await update.getByRole("button", { name: "Save", exact: true }).click();
  await expect(update).toHaveCount(0);
  await expect.poll(() => rotations).toBe(1);
  await expect(page.getByText("Revoked", { exact: true })).toHaveCount(0);

  await actions.click();
  await page.getByRole("menuitem", { name: "View computers", exact: true }).click();
  await expect(page).toHaveURL(/\/computers\?secret=SYNTHETIC_TOKEN/);
  await expect(page.getByText("Computers that received a raw value of")).toBeVisible();
  await expect(page.getByRole("columnheader", { name: "Secret value" })).toBeVisible();
  await expect(page.getByRole("row", { name: new RegExp(received) })).toContainText("Received");
  await expect(page.getByRole("row", { name: new RegExp(image) })).toContainText("May hold (image)");
  expect(computerQueries).toContain(secretID);
  await page.getByRole("button", { name: "Clear filter", exact: true }).click();
  await expect(page.getByText("No computers yet.")).toBeVisible();
  await expect(page.getByRole("columnheader", { name: "Secret value" })).toHaveCount(0);
});
