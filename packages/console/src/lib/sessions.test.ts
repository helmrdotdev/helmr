import { afterEach, expect, test } from "bun:test";

import {
  messageDeliveryNotice, questionCLICommands, getSession, getSessionEvents, listSessions, cancelSession,
  interruptSession, sessionConsolePath,
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
      agent_id: "operator",
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

  await listSessions({ projectID: "project-1", environmentID: "env-1", statuses: ["open", "cancelled"], limit: 100 });

  expect(requestedURL).toBe("/api/projects/project-1/environments/env-1/sessions?status=open&status=cancelled&limit=100");
});

test("safety controls preserve scope and retry identity", async () => {
  const address = { sessionID: "s/1", projectID: "p/1", environmentID: "e/1" };
  const calls: { path: string; body: unknown }[] = [];
  globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ path: String(url), body: JSON.parse(String(init?.body)) });
    return Response.json({ id: "receipt", status: "stopping" });
  }) as typeof fetch;
  expect((await interruptSession(address, { idempotency_key: "stop-key" })).status).toBe("stopping");
  await cancelSession(address, { idempotency_key: "cancel-key" });
  const base = "/api/projects/p%2F1/environments/e%2F1/sessions/s%2F1";
  expect(calls).toEqual([
    { path: base + "/interrupt", body: { idempotency_key: "stop-key" } },
    { path: base + "/cancel", body: { idempotency_key: "cancel-key" } },
  ]);
});

test("distinguishes a rejected message from an uncertain native callback", () => {
 const event = { session_id: "session", turn_id: "turn", sequence: 5, kind: "message.rejected", created_at: "2026-10-10T00:00:00Z", data: { delivery: "uncertain" } };
 expect(messageDeliveryNotice(event)).toContain("delivery uncertain");
 expect(messageDeliveryNotice({ ...event, data: { delivery: "not_delivered" } })).toContain("not delivered");
 expect(messageDeliveryNotice({ ...event, kind: "message.delivered" })).toBeUndefined();
});

test("question CLI instructions bind origin, scope and exact question without interpolating shell syntax", () => {
  const commands = questionCLICommands({ sessionID: "s'$(command)", turnID: "t", askID: "a", projectID: "p", environmentID: "e" }, "https://helmr.example");
  expect(commands.login).toBe("helmr login 'https://helmr.example'");
  expect(commands.get).toBe(`helmr --api-url 'https://helmr.example' session turn ask get 's'"'"'$(command)' 't' 'a' --project 'p' --env 'e' --json`);
  expect(commands.respond).toBe(`helmr --api-url 'https://helmr.example' session turn ask respond 's'"'"'$(command)' 't' 'a' --project 'p' --env 'e' --answer-file answer.json --response-id RESPONSE_ID`);
});
