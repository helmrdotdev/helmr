import { afterEach, expect, test } from "bun:test";

import {
  getSession,
  getSessionOutput,
  runSessionConsolePath,
  sessionConsolePath,
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
