import { execFileSync } from "node:child_process";
import { expect, type Page } from "@playwright/test";

export const projectID = "console-demo";

// Seed catalog definitions and synthetic capacity. Admission and controls use HTTP.
// No Worker, executable bundle, VM or object-store behavior is represented.
export async function createAgentCatalog(page: Page, environmentPermissions: string[] = []) {
  if (process.env.HELMR_E2E_BASE_URL || process.env.DATABASE_URL) {
    throw new Error("Run Agent API acceptance with the disposable managed browser stack");
  }
  const slug = `browser-agent-${crypto.randomUUID()}`;
  const response = await page.request.post(`/api/projects/${projectID}/environments`, {
    data: { environment: slug, name: slug, color_hex: "#008000" },
  });
  expect(response.ok(), await response.text()).toBeTruthy();
  const environment = await response.json() as { environment: string };
  const deploymentID = `00000000-0000-7000-8000-${crypto.randomUUID().slice(-12)}`;
  const agentID = `00000000-0000-7000-8000-${crypto.randomUUID().slice(-12)}`;
  const environmentInternalID = execFileSync("bash", ["tests/browser/fixtures/agent-catalog.sh", environment.environment, deploymentID, agentID], { cwd: process.cwd(), encoding: "utf8" }).trim();
  // Ownership does not confer workload access. Grant this fixture member access
  // only to its newly seeded Agent, through the real access API.
  const meResponse = await page.request.get("/api/me");
  expect(meResponse.ok(), await meResponse.text()).toBeTruthy();
  const me = await meResponse.json() as { membership_id: string };
  const roleResponse = await page.request.post("/api/roles", { data: {
    name: slug, description: "Browser conversation fixture", permissions: ["agents.read", "agents.use", "sessions.read", "sessions.create_keyed", "sessions.send", "sessions.answer", "sessions.resume", "sessions.interrupt", "sessions.cancel", "sessions.close", "triggers.read"],
  } });
  expect(roleResponse.ok(), await roleResponse.text()).toBeTruthy();
  const role = await roleResponse.json() as { id: string; revision: number };
  const targetsResponse = await page.request.get(`/api/projects/${projectID}/access-targets`);
  expect(targetsResponse.ok(), await targetsResponse.text()).toBeTruthy();
  const targets = await targetsResponse.json() as { project_id: string };
  const grant = await page.request.post("/api/role-grants", { data: {
    role_id: role.id, expected_role_revision: role.revision,
    subject: { type: "member", member_id: me.membership_id },
    project_id: targets.project_id, environment_id: environmentInternalID,
    agents_mode: "selected", agent_ids: [agentID],
  } });
  expect(grant.ok(), await grant.text()).toBeTruthy();
  const grantEnvironmentPermissions = async (permissions: string[]) => {
    const response = await page.request.post("/api/roles", { data: {
      name: `browser-environment-${crypto.randomUUID()}`, description: "Browser environment fixture", permissions,
    } });
    expect(response.ok(), await response.text()).toBeTruthy();
    const environmentRole = await response.json() as { id: string; revision: number };
    const assignment = await page.request.post("/api/role-grants", { data: {
      role_id: environmentRole.id, expected_role_revision: environmentRole.revision,
      subject: { type: "member", member_id: me.membership_id },
      project_id: targets.project_id, environment_id: environmentInternalID,
      agents_mode: "none", agent_ids: [],
    } });
    expect(assignment.ok(), await assignment.text()).toBeTruthy();
  };
  if (environmentPermissions.length) await grantEnvironmentPermissions(environmentPermissions);

  await page.goto("/settings/environments");
  const environmentRow = page.getByRole("row").filter({ hasText: slug });
  await environmentRow.getByRole("button", { name: /^Actions for / }).click();
  await page.getByRole("menuitem", { name: "Use", exact: true }).click();
  await expect(environmentRow.getByText("working", { exact: true })).toBeVisible();
  return { grantEnvironmentPermissions, deploymentID, environmentID: environment.environment, environmentInternalID, scope: `project=${projectID}&environment=${environment.environment}`, base: `/api/projects/${projectID}/environments/${environment.environment}` };
}
