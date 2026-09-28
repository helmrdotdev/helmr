import { expect, test } from "@playwright/test";

// Exercise the Console against the public Command wire contract. This fixture
// covers UI observation; it does not stand in for Worker or database acceptance.
test("Computer command receipt, live output, outcome and paged gaps", async ({ page }) => {
  const projectID = "00000000-0000-7000-8000-000000000301";
  const environmentID = "00000000-0000-7000-8000-000000000401";
  const computerID = "00000000-0000-7000-8000-000000000501";
  const commandID = "00000000-0000-7000-8000-000000000601";
  const now = new Date().toISOString();
  const environment = { id: environmentID, project_id: projectID, slug: "dev", name: "Development", color_hex: "#008000", is_default: true, created_at: now, updated_at: now };
  const project = { id: projectID, slug: "test", name: "Test", default_region_id: "local", is_default: true, environments: [environment], created_at: now, updated_at: now };
  const base = `/api/projects/${projectID}/environments/${environmentID}`;
  let finished = false;
  let statusReads = 0;
  let logReads = 0;
  let submissions = 0;
  let submitted: unknown;
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    let json: unknown;
    if (url.pathname === "/api/me") json = { user_id: "local", display_name: "Local", profile_image_url: null, org_id: "org", permissions: ["computer.exec.create"], admin: false, organization_required: false, project_required: false };
    else if (url.pathname === "/api/projects") json = { projects: [project] };
    else if (url.pathname === `/api/projects/${projectID}`) json = project;
    else if (url.pathname === `${base}/computers/${computerID}`) json = { id: computerID, sandbox_id: "shell", deployment_id: "deployment", status: "available", secrets: [], last_activity_at: now, created_at: now, updated_at: now };
    else if (url.pathname === `${base}/computers/${computerID}/members`) json = { members: [] };
    else if (url.pathname === `${base}/computers/${computerID}/exec`) {
      submissions++;
      submitted = route.request().postDataJSON();
      json = { command_id: commandID };
    } else if (url.pathname === `${base}/commands/${commandID}`) {
      if (++statusReads === 1) { await route.fulfill({ status: 503, json: { error: { code: "temporary", message: "Try again" } } }); return; }
      json = { id: commandID, computer_id: computerID, status: finished ? "exited" : "running", process_reconciled: false, ...(finished ? { outcome: { command_id: commandID, kind: "exited", exit_code: 7, terminal_at: now } } : {}) };
    } else if (url.pathname === `${base}/commands/${commandID}/logs`) {
      if (++logReads === 1) { await route.fulfill({ status: 503, json: { error: { code: "temporary", message: "Try again" } } }); return; }
      expect(url.searchParams.get("limit")).toBe("100");
      json = url.searchParams.has("cursor")
        ? { logs: [{ kind: "output", stream: "stdout", cursor: "unicode", content_base64: "nJM=", observed_at: now }, { kind: "gap", stream: "stdout", cursor: "gap", from_sequence: "2", through_sequence: "4" }], output_state: "unavailable", next_cursor: "gap" }
        : { logs: [{ kind: "output", stream: "stdout", cursor: "after/1", content_base64: Buffer.concat([Buffer.from("live ✓"), Buffer.from([0xe2])]).toString("base64"), observed_at: now }], output_state: finished ? "closed" : "open", next_cursor: "after/1" };
    } else {
      await route.fulfill({ status: 404, json: { error: { code: "not_found", message: `Unexpected test request ${url.pathname}` } } });
      return;
    }
    await route.fulfill({ json });
  });
  await page.goto(`/computers/${computerID}?project_id=${projectID}&environment_id=${environmentID}`);
  await page.getByLabel("Command (argv, split on whitespace)").fill("sh -c exit");
  await page.getByRole("button", { name: "Run command", exact: true }).click();
  await page.getByRole("button", { name: "Run", exact: true }).click();
  await page.getByRole("button", { name: "Retry Command status" }).click();
  await page.getByRole("button", { name: "Retry output", exact: true }).click();
  await expect(page.getByText("live ✓", { exact: true })).toBeVisible();
  expect(submitted).toMatchObject({ command: ["sh", "-c", "exit"], idempotency_key: expect.any(String) });
  await expect(page.getByText("exit code 7", { exact: false })).toHaveCount(0);
  finished = true;
  await expect(page.getByText("exit code 7", { exact: false })).toBeVisible();
  await page.getByRole("button", { name: "Next output" }).click();
  await expect(page.getByText("[Output unavailable: chunks 2–4]", { exact: false })).toBeVisible();
  await expect(page.locator("pre").filter({ hasText: "✓\n[Output unavailable" })).toBeVisible();
  await expect(page.getByRole("alert").filter({ hasText: "without confirming all output" })).toBeVisible();
  expect(submissions).toBe(1);
  await page.getByRole("button", { name: "Previous output" }).click();
  await expect(page.getByText("live ✓", { exact: true })).toBeVisible();
});
