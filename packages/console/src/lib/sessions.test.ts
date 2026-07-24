import { afterEach, expect, test } from "bun:test";

import {
  cancelSession,
  closeSession,
  listSessionRuns,
  listSessions,
} from "./sessions";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("lists sessions with current scoped filters", async () => {
  let requestedUrl: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input);
    return Response.json({ sessions: [] });
  }) as typeof fetch;

  await listSessions({ projectID: "project-1", environmentID: "env-1", status: "open", taskID: "review", limit: 8 });

  expect(requestedUrl).toBe("/api/projects/project-1/environments/env-1/sessions?status=open&task_id=review&limit=8");
});

test("scopes session close and cancel actions", async () => {
  const requests: Array<{ url: string; body: unknown }> = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requests.push({ url: String(input), body: JSON.parse(String(init?.body)) });
    return Response.json({ id: "session-1", status: "closed" });
  }) as typeof fetch;

  await closeSession("session/1", { projectID: "project-1", environmentID: "env-1" }, "done");
  await cancelSession("session/1", { projectID: "project-1", environmentID: "env-1" }, "stop");

  expect(requests).toEqual([
    {
      url: "/api/projects/project-1/environments/env-1/sessions/session%2F1/close",
      body: { reason: "done" },
    },
    {
      url: "/api/projects/project-1/environments/env-1/sessions/session%2F1/cancel",
      body: { reason: "stop" },
    },
  ]);
});

test("reads session runs", async () => {
  let requestedUrl: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input);
    return Response.json({ runs: [] });
  }) as typeof fetch;

  await listSessionRuns("session/1", { projectID: "project-1", environmentID: "env-1" });

  expect(requestedUrl).toBe("/api/projects/project-1/environments/env-1/sessions/session%2F1/runs");
});
