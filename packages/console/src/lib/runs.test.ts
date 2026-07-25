import { afterEach, expect, test } from "bun:test";

import { getRunEvents, getRunLogs, listRuns } from "./runs";

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

  expect(requestedUrl).toBe("/api/projects/project-1/environments/env-1/runs?limit=100");
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

test("gets finite run log pages with a bound cursor", async () => {
  let requestedUrl: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input);
    return Response.json({ logs: [], next_cursor: null });
  }) as typeof fetch;

  await getRunLogs("run/1", "project-1", "env-1", {
    cursor: "rt1.cursor",
    limit: 50,
  });

  expect(requestedUrl).toBe(
    "/api/projects/project-1/environments/env-1/runs/run%2F1/logs?cursor=rt1.cursor&limit=50",
  );
});
