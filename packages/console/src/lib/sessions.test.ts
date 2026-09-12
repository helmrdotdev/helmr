import { afterEach, expect, test } from "bun:test";

import {
  closeSession,
  getSession,
  getSessionInput,
  getSessionOutput,
  interleaveSessionRecords,
  listSessions,
  runSessionConsolePath,
  sendSessionInput,
  sessionConsolePath,
  type SessionInputRecord,
  type SessionOutputRecord,
} from "./sessions";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("loads a Session from the scoped read API", async () => {
  let requestedURL: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURL = String(input);
    return Response.json({
      id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
      actor_id: "operator",
      deployment_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
      status: "open",
      created_at: "2026-07-25T00:00:00Z",
      updated_at: "2026-07-25T00:00:00Z",
    });
  }) as typeof fetch;

  await getSession({
    sessionID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
    projectID: "project/1",
    environmentID: "env/1",
  });

  expect(requestedURL).toBe(
    "/api/projects/project%2F1/environments/env%2F1/sessions/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
  );
});

test("loads the next Session output page with a bounded cursor", async () => {
  let requestedURL: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURL = String(input);
    return Response.json({ records: [], next_after: 42, has_more: false });
  }) as typeof fetch;

  await getSessionOutput({
    sessionID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
    projectID: "project-1",
    environmentID: "env-1",
  }, { after: 42, limit: 100 });

  expect(requestedURL).toBe(
    "/api/projects/project-1/environments/env-1/sessions/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33/outputs?after=42&limit=100",
  );
});

test("escapes a Session console route and preserves its scope", () => {
  expect(sessionConsolePath("session/id", "project/1", "env/1")).toBe(
    "/sessions/session%2Fid?project_id=project%2F1&environment_id=env%2F1",
  );
});

test("links only Runs that belong to a Session", () => {
  expect(runSessionConsolePath({
    session_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
  }, "prj_aaaaaaaaaaaaaaaaaaaaaaaaaa", "env_aaaaaaaaaaaaaaaaaaaaaaaaaa")).toBe(
    "/sessions/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33?project_id=prj_aaaaaaaaaaaaaaaaaaaaaaaaaa&environment_id=env_aaaaaaaaaaaaaaaaaaaaaaaaaa",
  );
  expect(runSessionConsolePath(
    {},
    "prj_aaaaaaaaaaaaaaaaaaaaaaaaaa",
    "env_aaaaaaaaaaaaaaaaaaaaaaaaaa",
  )).toBeUndefined();
});

test("lists Sessions with a bounded cursor", async () => {
  let requestedURL: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURL = String(input);
    return Response.json({ sessions: [] });
  }) as typeof fetch;

  await listSessions({ projectID: "project/1", environmentID: "env-1", cursor: "c1", limit: 100 });

  expect(requestedURL).toBe("/api/projects/project%2F1/environments/env-1/sessions?cursor=c1&limit=100");
});

test("lists Sessions by public status as repeated params", async () => {
  let requestedURL: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURL = String(input);
    return Response.json({ sessions: [] });
  }) as typeof fetch;

  await listSessions({ projectID: "project-1", environmentID: "env-1", statuses: ["open", "failed"], limit: 100 });

  expect(requestedURL).toBe("/api/projects/project-1/environments/env-1/sessions?status=open&status=failed&limit=100");
});

test("loads the next Session input page with a bounded cursor", async () => {
  let requestedURL: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURL = String(input);
    return Response.json({ records: [], next_after: 7, has_more: false });
  }) as typeof fetch;

  await getSessionInput({
    sessionID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
    projectID: "project-1",
    environmentID: "env-1",
  }, { after: 7, limit: 100 });

  expect(requestedURL).toBe(
    "/api/projects/project-1/environments/env-1/sessions/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33/inputs?after=7&limit=100",
  );
});

test("loads the first Session input page without query parameters", async () => {
  let requestedURL: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURL = String(input);
    return Response.json({ records: [], next_after: 0, has_more: false });
  }) as typeof fetch;

  await getSessionInput({ sessionID: "session/1", projectID: "project-1", environmentID: "env-1" });

  expect(requestedURL).toBe("/api/projects/project-1/environments/env-1/sessions/session%2F1/inputs");
});

test("sends Session input with its idempotency key", async () => {
  let requestedURL: string | undefined;
  let requestInit: RequestInit | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedURL = String(input);
    requestInit = init;
    return Response.json({
      id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34",
      sequence: 3,
      data: { prompt: "hello" },
      source: { type: "external" },
      created_at: "2026-07-25T00:00:00Z",
    }, { status: 201 });
  }) as typeof fetch;

  const record = await sendSessionInput(
    { sessionID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33", projectID: "project-1", environmentID: "env-1" },
    { input: { prompt: "hello" }, idempotency_key: "key-1" },
  );

  expect(requestedURL).toBe(
    "/api/projects/project-1/environments/env-1/sessions/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33/inputs",
  );
  expect(requestInit?.method).toBe("POST");
  expect(JSON.parse(String(requestInit?.body))).toEqual({ input: { prompt: "hello" }, idempotency_key: "key-1" });
  expect(record.sequence).toBe(3);
});

test("closes a Session with its idempotency key", async () => {
  let requestedURL: string | undefined;
  let requestInit: RequestInit | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedURL = String(input);
    requestInit = init;
    return Response.json({ session_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33", accepted_at: "2026-07-25T00:00:00Z" });
  }) as typeof fetch;

  await closeSession(
    { sessionID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33", projectID: "project/1", environmentID: "env-1" },
    { idempotency_key: "key-2" },
  );

  expect(requestedURL).toBe(
    "/api/projects/project%2F1/environments/env-1/sessions/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33/close",
  );
  expect(requestInit?.method).toBe("POST");
  expect(JSON.parse(String(requestInit?.body))).toEqual({ idempotency_key: "key-2" });
});

test("interleaves input and output records by time with input first on ties", () => {
  const input = (sequence: number, created_at: string): SessionInputRecord => ({
    id: `in-${sequence}`, sequence, data: {}, source: { type: "external" }, created_at,
  });
  const output = (sequence: number, created_at: string): SessionOutputRecord => ({
    id: `out-${sequence}`,
    sequence,
    data: {},
    content_type: "application/json",
    created_at,
    provenance: { run_id: "run-1", attempt_number: 1, deployment_id: "dep-1" },
  });

  const entries = interleaveSessionRecords(
    [input(1, "2026-07-25T00:00:00Z"), input(2, "2026-07-25T00:00:02.500Z")],
    [output(1, "2026-07-25T00:00:01Z"), output(2, "2026-07-25T00:00:02.500Z"), output(3, "2026-07-25T00:00:02.750Z")],
  );

  expect(entries.map((entry) => `${entry.direction}:${entry.record.sequence}`)).toEqual([
    "input:1",
    "output:1",
    "input:2",
    "output:2",
    "output:3",
  ]);
});
