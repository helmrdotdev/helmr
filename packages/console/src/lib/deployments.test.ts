import { afterEach, expect, test } from "bun:test";

import {
  getCurrentDeployment,
  getDeployment,
  getDeploymentEvents,
  listDeployments,
  promoteDeployment,
} from "./deployments";

const originalFetch = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = originalFetch;
});

function captureFetch(status: number, body: unknown) {
  const calls: { url: string; init: RequestInit | undefined }[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ url: String(input), init });
    return Response.json(body, { status });
  }) as typeof fetch;
  return calls;
}

const scope = { projectID: "project-1", environmentID: "env/1" };

test("lists deployments with cursor and limit", async () => {
  const calls = captureFetch(200, { deployments: [] });
  await listDeployments(scope, { cursor: "c1", limit: 50 });
  expect(calls[0]?.url).toBe("/api/projects/project-1/environments/env%2F1/deployments?cursor=c1&limit=50");
});

test("lists deployments without empty params", async () => {
  const calls = captureFetch(200, { deployments: [] });
  await listDeployments(scope);
  expect(calls[0]?.url).toBe("/api/projects/project-1/environments/env%2F1/deployments");
});

test("gets one deployment and escapes its id", async () => {
  const calls = captureFetch(200, { id: "d/1", version: "v", bundle_digest: "sha256:a", created_at: "2026-01-01T00:00:00Z" });
  await getDeployment("d/1", scope);
  expect(calls[0]?.url).toBe("/api/projects/project-1/environments/env%2F1/deployments/d%2F1");
});

test("returns null when there is no current deployment", async () => {
  captureFetch(404, { error: { code: "no_current_deployment", message: "no current deployment" } });
  expect(await getCurrentDeployment(scope)).toBeNull();
});

test("promotes a deployment with an empty POST body", async () => {
  const calls = captureFetch(200, { id: "d-1", version: "v", bundle_digest: "sha256:a", created_at: "2026-01-01T00:00:00Z" });
  await promoteDeployment("d-1", scope);
  expect(calls[0]?.url).toBe("/api/projects/project-1/environments/env%2F1/deployments/d-1/promote");
  expect(calls[0]?.init?.method).toBe("POST");
  expect(calls[0]?.init?.body).toBe("{}");
});

test("pages deployment events with a cursor", async () => {
  const calls = captureFetch(200, { events: [], next_cursor: null });
  await getDeploymentEvents("d-1", scope, { cursor: "42", limit: 100 });
  expect(calls[0]?.url).toBe("/api/projects/project-1/environments/env%2F1/deployments/d-1/events?cursor=42&limit=100");
});
