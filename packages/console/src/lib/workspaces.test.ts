import { afterEach, expect, test } from "bun:test";

import {
  getWorkspace,
  getWorkspaceExec,
  listWorkspaceExecOutput,
  listWorkspaceExecs,
  materializeWorkspace,
  stopWorkspace,
} from "./workspaces";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("loads a workspace deep link without the currently selected scope", async () => {
  let requestedURL: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURL = String(input);
    return Response.json({ workspace: { id: "workspace/1" } });
  }) as typeof fetch;

  await getWorkspace("workspace/1");

  expect(requestedURL).toBe("/api/workspaces/workspace%2F1");
});

test("uses the workspace actual scope for exec list and exact exec lookup", async () => {
  const requestedURLs: string[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURLs.push(String(input));
    return Response.json(requestedURLs.length === 1 ? { execs: [] } : { exec: {} });
  }) as typeof fetch;
  const scope = { projectID: "project/actual", environmentID: "env actual" };

  await listWorkspaceExecs("workspace/1", scope);
  await getWorkspaceExec("workspace/1", "exec/selected", scope);

  expect(requestedURLs).toEqual([
    "/api/projects/project%2Factual/environments/env%20actual/workspaces/workspace%2F1/execs",
    "/api/projects/project%2Factual/environments/env%20actual/workspaces/workspace%2F1/execs/exec%2Fselected",
  ]);
});

test("loads both durable output streams from the exact exec", async () => {
  const requestedURLs: string[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURLs.push(String(input));
    return Response.json({ chunks: [] });
  }) as typeof fetch;
  const scope = { projectID: "project-1", environmentID: "env-1" };

  await listWorkspaceExecOutput("workspace-1", "exec-1", "stdout", scope);
  await listWorkspaceExecOutput("workspace-1", "exec-1", "stderr", scope);

  expect(requestedURLs).toEqual([
    "/api/projects/project-1/environments/env-1/workspaces/workspace-1/execs/exec-1/stdout",
    "/api/projects/project-1/environments/env-1/workspaces/workspace-1/execs/exec-1/stderr",
  ]);
});

test("sends lifecycle actions only to existing actual-scope routes", async () => {
  const requests: Array<{ url: string; method: string; body: string | null }> = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requests.push({
      url: String(input),
      method: init?.method ?? "GET",
      body: typeof init?.body === "string" ? init.body : null,
    });
    return Response.json({ id: "mount-1", workspace_id: "workspace-1", state: "requested" });
  }) as typeof fetch;
  const scope = { projectID: "project-1", environmentID: "env-1" };

  await materializeWorkspace("workspace-1", scope);
  await stopWorkspace("workspace-1", scope);

  expect(requests).toEqual([
    {
      url: "/api/projects/project-1/environments/env-1/workspaces/workspace-1/materialize",
      method: "POST",
      body: "{}",
    },
    {
      url: "/api/projects/project-1/environments/env-1/workspaces/workspace-1/stop",
      method: "POST",
      body: "{}",
    },
  ]);
});
