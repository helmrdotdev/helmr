import { afterEach, expect, test } from "bun:test";
import { listAgentDefinitions, listTemplateDefinitions, getAgentDefinition, getTemplateDefinition, definitionHref } from "./definitions";

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

test("lists current and historical Agent and Template definitions with pagination", async () => {
  const calls = capture({ deployment_id: "d/1", agent_definitions: [], template_definitions: [] });
  await listAgentDefinitions(scope);
  await listAgentDefinitions({ ...scope, deploymentID: "d/1", cursor: "next/page", limit: 100 });
  await listTemplateDefinitions({ ...scope, deploymentID: "d/1", cursor: "next/page", limit: 100 });
  const base = "/api/projects/project%2F1/environments/env%2F1";
  expect(calls.map(call => call.url)).toEqual([
    `${base}/agent-definitions`,
    `${base}/agent-definitions?deployment_id=d%2F1&cursor=next%2Fpage&limit=100`,
    `${base}/template-definitions?deployment_id=d%2F1&cursor=next%2Fpage&limit=100`,
  ]);
});

test("missing scope fails before sending a request", async () => {
  const calls = capture({});
  await expect(listAgentDefinitions({ projectID: "", environmentID: "env" })).rejects.toThrow("project and environment are required");
  expect(calls).toHaveLength(0);
});

test("definition detail links preserve the Session's exact scope and Deployment", async () => {
  const calls = capture({ id: "worker", deployment_id: "old/version" });
  const options = { ...scope, deploymentID: "old/version" };
  const href = definitionHref("agents", "worker/key", options);
  expect(href).toBe("/agents/worker%2Fkey?project_id=project%2F1&environment_id=env%2F1&deployment_id=old%2Fversion");
  await getAgentDefinition("worker/key", options);
  await getTemplateDefinition("image/key", options);
  expect(calls.map(call => call.url)).toEqual([
    "/api/projects/project%2F1/environments/env%2F1/agent-definitions/worker%2Fkey?deployment_id=old%2Fversion",
    "/api/projects/project%2F1/environments/env%2F1/template-definitions/image%2Fkey?deployment_id=old%2Fversion",
  ]);
});
