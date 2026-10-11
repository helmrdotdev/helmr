import { afterEach, expect, test } from "bun:test";
import { listAgents, listComputerDefinitions } from "./definitions";

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

test("lists current and historical Agent and Computer definitions with pagination", async () => {
  const calls = capture({ deployment_id: "d/1", agents: [], computer_definitions: [] });
  await listAgents(scope);
  await listAgents({ ...scope, deploymentID: "d/1", cursor: "next/page", limit: 100 });
  await listComputerDefinitions({ ...scope, deploymentID: "d/1", cursor: "next/page", limit: 100 });
  const base = "/api/projects/project%2F1/environments/env%2F1";
  expect(calls.map(call => call.url)).toEqual([
    `${base}/agents`,
    `${base}/agents?deployment_id=d%2F1&cursor=next%2Fpage&limit=100`,
    `${base}/computer-definitions?deployment_id=d%2F1&cursor=next%2Fpage&limit=100`,
  ]);
});

test("missing scope fails before sending a request", async () => {
  const calls = capture({});
  await expect(listAgents({ projectID: "", environmentID: "env" })).rejects.toThrow("project and environment are required");
  expect(calls).toHaveLength(0);
});
