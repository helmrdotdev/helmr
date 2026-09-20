import { afterEach, expect, test } from "bun:test";

import {
  closeSession, getSession, getSessionEvents, listSessions, runSessionConsolePath,
  sendSession, sendTurnMessage, interruptTurn, resumeSession, sessionConsolePath,
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

test("loads the next Session event page with a bounded cursor", async () => {
  let requestedURL: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURL = String(input);
    return Response.json({ records: [], next_after: 42, has_more: false });
  }) as typeof fetch;

  await getSessionEvents({
    sessionID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
    projectID: "project-1",
    environmentID: "env-1",
  }, { after: 42, limit: 100 });

  expect(requestedURL).toBe(
    "/api/projects/project-1/environments/env-1/sessions/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33/events?after=42&limit=100",
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

test("closes a Session with its idempotency key", async () => {
  let requestedURL: string | undefined;
  let requestInit: RequestInit | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedURL = String(input);
    requestInit = init;
    return Response.json({ session_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33", id: "operation-1", status: "closing" });
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


test("mutations bind application data and exact Turn or hold to the scoped endpoint", async () => {
  const address = { sessionID: "s/1", projectID: "p/1", environmentID: "e/1" };
  const calls: { path: string; body: unknown }[] = [];
  globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ path: String(url), body: JSON.parse(String(init?.body)) });
    return Response.json({ id: "receipt", status: "stopping" });
  }) as typeof fetch;
  const data = { data: { type: "answer", value: null }, idempotency_key: "message-key" };
  await sendSession(address, data, "enqueue");
  await sendTurnMessage(address, "t/1", data);
  expect((await interruptTurn(address, "t/1", { idempotency_key: "stop-key" })).status).toBe("stopping");
  await resumeSession(address, { hold_id: "h1", idempotency_key: "resume-key" });
  const base = "/api/projects/p%2F1/environments/e%2F1/sessions/s%2F1";
  expect(calls).toEqual([
    { path: base + "/enqueue", body: data },
    { path: base + "/turns/t%2F1/messages", body: data },
    { path: base + "/turns/t%2F1/interrupt", body: { idempotency_key: "stop-key" } },
    { path: base + "/resume", body: { hold_id: "h1", idempotency_key: "resume-key" } },
  ]);
});
