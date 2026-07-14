import { afterEach, expect, test } from "bun:test";

import { completePendingToken, getRunEvents, listRuns } from "./runs";

const originalFetch = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("lists runs under the selected environment", async () => {
  let requestedUrl: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input);
    return Response.json({ runs: [] });
  }) as typeof fetch;

  await listRuns({ projectID: "project-1", environmentID: "env-1" });

  expect(requestedUrl).toBe("/api/projects/project-1/environments/env-1/runs?status=all&limit=100");
});

test("gets run events with cursor and limit", async () => {
  let requestedUrl: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input);
    return Response.json({ events: [], cursor: "tc1.eyJzIjo3fQ", next_cursor: null });
  }) as typeof fetch;

  await getRunEvents("run-1", "project-1", "env-1", { cursor: "tc1.eyJzIjo3fQ", limit: 50 });

  expect(requestedUrl).toBe("/api/projects/project-1/environments/env-1/runs/run-1/events?cursor=tc1.eyJzIjo3fQ&limit=50");
});

test("gets run events without empty params and escapes ids", async () => {
  let requestedUrl: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input);
    return Response.json({ events: [], cursor: "tc1.eyJzIjowfQ", next_cursor: null });
  }) as typeof fetch;

  await getRunEvents("run/1", "project-1", "env-1");

  expect(requestedUrl).toBe("/api/projects/project-1/environments/env-1/runs/run%2F1/events");
});

test("completes a pending token through the authenticated scoped endpoint", async () => {
  let requestedUrl: string | undefined;
  let requestedBody: unknown;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input);
    requestedBody = JSON.parse(String(init?.body));
    return Response.json({ status: "already_completed", token: { id: "token/1" } });
  }) as typeof fetch;

  const response = await completePendingToken("token/1", { approved: false }, "project-1", "env-1");

  expect(requestedUrl).toBe("/api/projects/project-1/environments/env-1/tokens/token%2F1/complete");
  expect(requestedBody).toEqual({ data: { approved: false } });
  expect(response.status).toBe("already_completed");
});
