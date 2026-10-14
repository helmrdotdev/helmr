import { afterEach, expect, test } from "bun:test";
import { listAgentDefinitions, listTemplateDefinitions, getAgentDefinition, getTemplateDefinition, definitionHref, agentHref, templateHref } from "./definitions";

const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });
const scope = { projectID: "project/1", environmentID: "env/1" };
function capture(body: unknown) {
  const calls: { url: string; init?: RequestInit }[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ url: String(input), init });
    return Response.json(body);
  }) as typeof fetch;
  return calls;
}

test("lists current and historical Agent and Templates with pagination", async () => {
  const calls = capture({ deployment_id: "d/1", agents: [], templates: [] });
  await listAgentDefinitions(scope);
  await listAgentDefinitions({ ...scope, deploymentID: "d/1", cursor: "next/page", limit: 100 });
  await listTemplateDefinitions({ ...scope, deploymentID: "d/1", cursor: "next/page", limit: 100 });
  const base = "/api/projects/project%2F1/environments/env%2F1";
  expect(calls.map(call => call.url)).toEqual([
    `${base}/agents`,
    `${base}/agents?deployment_id=d%2F1&cursor=next%2Fpage&limit=100`,
    `${base}/templates?deployment_id=d%2F1&cursor=next%2Fpage&limit=100`,
  ]);
});

test("missing scope fails before sending a request", async () => {
  const calls = capture({});
  await expect(listAgentDefinitions({ projectID: "", environmentID: "env" })).rejects.toThrow("project and environment are required");
  expect(calls).toHaveLength(0);
});

test("definition detail links pin the exact Deployment without scope params", async () => {
  const calls = capture({ id: "worker", deployment_id: "old/version" });
  const options = { ...scope, deploymentID: "old/version" };
  const href = definitionHref("agents", "worker/key", options.deploymentID);
  expect(href).toBe("/deployments/old%2Fversion/agents/worker%2Fkey");
  await getAgentDefinition("worker/key", options);
  await getTemplateDefinition("image/key", options);
  expect(calls.map(call => call.url)).toEqual([
    "/api/projects/project%2F1/environments/env%2F1/agents/worker%2Fkey?deployment_id=old%2Fversion",
    "/api/projects/project%2F1/environments/env%2F1/templates/image%2Fkey?deployment_id=old%2Fversion",
  ]);
});

test("an Agent page opens on its Sessions and its other tabs are pages below it", () => {
  expect(agentHref("worker/key")).toBe("/agents/worker%2Fkey");
  expect(agentHref("worker", "sessions")).toBe("/agents/worker");
  expect(agentHref("worker", "triggers")).toBe("/agents/worker/triggers");
  expect(agentHref("worker", "slack")).toBe("/agents/worker/slack");
  expect(agentHref("worker", "definition")).toBe("/agents/worker/definition");
  expect(templateHref("image/key")).toBe("/templates/image%2Fkey");
});
