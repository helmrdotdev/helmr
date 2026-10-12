import { execFileSync } from "node:child_process";
import { expect, type Page } from "@playwright/test";

export const projectID = "console-demo";

// Seed catalog definitions only. All admission and control mutations use HTTP.
// No Worker, executable bundle, VM or object-store behavior is represented.
export async function createAgentCatalog(page: Page) {
  if (process.env.HELMR_E2E_BASE_URL || process.env.DATABASE_URL) {
    throw new Error("Run Agent API acceptance with the disposable managed browser stack");
  }
  const slug = `browser-agent-${crypto.randomUUID()}`;
  const response = await page.request.post(`/api/projects/${projectID}/environments`, {
    data: { environment: slug, name: slug, color_hex: "#008000", history_retention_mode: "until_environment_deletion" },
  });
  expect(response.ok(), await response.text()).toBeTruthy();
  const environment = await response.json() as { environment: string };
  const deploymentID = `00000000-0000-7000-8000-${crypto.randomUUID().slice(-12)}`;
  const environmentInternalID = execFileSync("bash", ["tests/browser/fixtures/agent-catalog.sh", environment.environment, deploymentID, `00000000-0000-7000-8000-${crypto.randomUUID().slice(-12)}`], { cwd: process.cwd(), encoding: "utf8" }).trim();
  await page.goto("/settings/environments");
  const environmentRow = page.getByRole("row").filter({ hasText: slug });
  await environmentRow.getByRole("button", { name: "Use", exact: true }).click();
  await expect(environmentRow.getByText("Current", { exact: true })).toBeVisible();
  return { deploymentID, environmentID: environment.environment, environmentInternalID, scope: `project=${projectID}&environment=${environment.environment}`, base: `/api/projects/${projectID}/environments/${environment.environment}` };
}
