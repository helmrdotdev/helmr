import { afterEach, expect, test } from "bun:test";

import {
  cancelTaskSession,
  closeTaskSession,
  listTaskSessionRuns,
  listTaskSessionStreamRecords,
  listTaskSessionStreams,
  listTaskSessions,
  type TaskSessionStream,
} from "./task-sessions";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("lists task sessions with current scoped filters", async () => {
  let requestedUrl: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input);
    return Response.json({ sessions: [] });
  }) as typeof fetch;

  await listTaskSessions({ projectID: "project-1", environmentID: "env-1", status: "open", taskID: "review", limit: 8 });

  expect(requestedUrl).toBe("/api/projects/project-1/environments/env-1/sessions?status=open&task_id=review&limit=8");
});

test("scopes session close and cancel actions", async () => {
  const requests: Array<{ url: string; body: unknown }> = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requests.push({ url: String(input), body: JSON.parse(String(init?.body)) });
    return Response.json({ id: "session-1", status: "closed" });
  }) as typeof fetch;

  await closeTaskSession("session/1", { projectID: "project-1", environmentID: "env-1" }, "done");
  await cancelTaskSession("session/1", { projectID: "project-1", environmentID: "env-1" }, "stop");

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

test("reads session runs, streams, and stream records", async () => {
  const requestedUrls: string[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrls.push(String(input));
    if (String(input).endsWith("/runs")) return Response.json({ runs: [] });
    if (String(input).endsWith("/streams")) return Response.json({ streams: [] });
    return Response.json({ records: [] });
  }) as typeof fetch;

  const stream: TaskSessionStream = {
    id: "stream-1",
    task_session_id: "session-1",
    name: "agent.report",
    direction: "output",
    next_sequence: 1,
    created_at: "2026-06-18T00:00:00Z",
  };
  await listTaskSessionRuns("session/1", { projectID: "project-1", environmentID: "env-1" });
  await listTaskSessionStreams("session/1", { projectID: "project-1", environmentID: "env-1" });
  await listTaskSessionStreamRecords("session/1", { projectID: "project-1", environmentID: "env-1" }, stream, { limit: 25 });

  expect(requestedUrls).toEqual([
    "/api/projects/project-1/environments/env-1/sessions/session%2F1/runs",
    "/api/projects/project-1/environments/env-1/sessions/session%2F1/streams",
    "/api/projects/project-1/environments/env-1/sessions/session%2F1/outputs/agent.report?limit=25",
  ]);
});
