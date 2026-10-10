import { expect, test } from "@playwright/test";

// This exercises the real Console confirmation with a synthetic API fixture.
// Revocation state and queued-input preservation are covered by the DB tests.
test("Secret revocation explains running Turn interruption and retained pending inputs", async ({ page }) => {
  const projectID = "00000000-0000-7000-8000-000000000301";
  const environmentID = "00000000-0000-7000-8000-000000000401";
  const secretID = "00000000-0000-7000-8000-000000000701";
  const now = new Date().toISOString();
  const environment = { id: environmentID, project_id: projectID, slug: "dev", name: "Development", color_hex: "#008000", is_default: true, created_at: now, updated_at: now };
  const project = { id: projectID, slug: "test", name: "Test", default_region_id: "local", is_default: true, environments: [environment], created_at: now, updated_at: now };
  const secret = { id: secretID, name: "SYNTHETIC_TOKEN", status: "active", created_at: now };
  const base = `/api/projects/${projectID}/environments/${environmentID}`;
  let revocations = 0;
  await page.route("**/api/**", async route => {
    const path = new URL(route.request().url()).pathname;
    let json: unknown;
    if (path === "/api/me") json = { user_id: "local", display_name: "Local", org_id: "org", permissions: ["secrets.read", "secrets.revoke"], admin: false, organization_required: false, project_required: false };
    else if (path === "/api/projects") json = { projects: [project] };
    else if (path === `/api/projects/${projectID}`) json = project;
    else if (path === base) json = environment;
    else if (path === `${base}/secrets`) json = { secrets: [secret] };
    else if (path === `${base}/secrets/${secretID}/revoke` && route.request().method() === "POST") {
      revocations++;
      secret.status = "revoked";
      json = secret;
    } else { await route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } }); return; }
    await route.fulfill({ json });
  });
  await page.goto(`/settings/secrets?project_id=${projectID}&environment_id=${environmentID}`);
  for (const accept of [false, true]) {
    await page.getByRole("button", { name: "Actions for SYNTHETIC_TOKEN" }).click();
    const pending = page.waitForEvent("dialog");
    const click = page.getByRole("button", { name: "Revoke", exact: true }).click();
    const dialog = await pending;
    expect(dialog.type()).toBe("confirm");
    expect(dialog.message()).toContain("Affected running Turns may be interrupted and fail to complete.");
    expect(dialog.message()).toContain("Pending inputs are retained but cannot run on affected Computers.");
    if (accept) await dialog.accept(); else await dialog.dismiss();
    await click;
    if (accept) await expect.poll(() => revocations).toBe(1);
    else expect(revocations).toBe(0);
  }
  await expect(page.getByText("Revoked", { exact: true })).toBeVisible();
});
