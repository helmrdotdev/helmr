import "./browser-test-env";
import { afterEach, expect, test } from "bun:test";
import { finishSlackAuthorization, getAgentSlackConnection, authorizeAgentSlackConnection } from "./slack";

const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });

test("Slack authorization names the exact Agent connection generation", async () => {
  const calls: { url: string; body: unknown }[] = [];
  globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ url: String(url), body: init?.body ? JSON.parse(String(init.body)) : null });
    return Response.json({});
  }) as typeof fetch;
  const address = { projectID: "project", environmentID: "environment", agentName: "sales" };
  await getAgentSlackConnection(address);
  await authorizeAgentSlackConnection(address, "generation-1", "/deployments/current");
  expect(calls).toEqual([
    { url: "/api/projects/project/environments/environment/agents/sales/slack", body: null },
    { url: "/api/projects/project/environments/environment/agents/sales/slack/generation-1/authorize", body: { return_to: "/deployments/current" } },
  ]);
});

test("an expired Slack callback reports authentication failure without navigating or forwarding its code", async () => {
  const calls: string[] = [];
  globalThis.fetch = (async (input: string | URL | Request) => { calls.push(String(input)); return Response.json({ error: { code: "unauthorized", message: "Authentication is required." } }, { status: 401 }); }) as typeof fetch;
  await expect(finishSlackAuthorization({ state: "state", code: "fixture-code" })).rejects.toThrow("Authentication is required.");
  expect(calls).toHaveLength(2);
  expect(calls[1]).toBe("/api/me");
});
