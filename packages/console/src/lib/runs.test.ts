import { afterEach, expect, test } from "bun:test";

import { createWaitpointResponseToken, getRunEvents, listRuns, respondWaitpoint } from "./runs";

const originalFetch = globalThis.fetch;
const originalWindow = (globalThis as unknown as { window?: unknown }).window;

afterEach(() => {
  globalThis.fetch = originalFetch;
  if (originalWindow === undefined) {
    delete (globalThis as unknown as { window?: unknown }).window;
  } else {
    (globalThis as unknown as { window?: unknown }).window = originalWindow;
  }
});

function installWindow(): void {
  (globalThis as unknown as { window: unknown }).window = {
    location: {
      origin: "https://console.example.test",
      pathname: "/runs/run-1",
      search: "",
      href: "https://console.example.test/runs/run-1",
    },
  };
}

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
    return Response.json({ events: [], cursor: 7, next_cursor: null });
  }) as typeof fetch;

  await getRunEvents("run-1", "project-1", "env-1", { cursor: 7, limit: 50 });

  expect(requestedUrl).toBe("/api/projects/project-1/environments/env-1/runs/run-1/events?cursor=7&limit=50");
});

test("gets run events without empty params and escapes ids", async () => {
  let requestedUrl: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input);
    return Response.json({ events: [], cursor: 0, next_cursor: null });
  }) as typeof fetch;

  await getRunEvents("run/1", "project-1", "env-1");

  expect(requestedUrl).toBe("/api/projects/project-1/environments/env-1/runs/run%2F1/events");
});

test("creates human wait confirmation links with respond action", async () => {
  installWindow();
  let requestedUrl: string | undefined;
  let requestedBody: unknown;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input);
    requestedBody = JSON.parse(String(init?.body));
    return Response.json({ id: "response-1", token: "secret", expires_at: null });
  }) as typeof fetch;

  const token = await createWaitpointResponseToken("wait-human", "human", "project-1", "env-1");

  expect(requestedUrl).toBe("/api/projects/project-1/environments/env-1/waitpoints/tokens");
  expect(requestedBody).toEqual({
    waitpoint_id: "wait-human",
  });
  expect(token.url).toBe("https://console.example.test/waitpoints/respond?id=response-1&token=secret");
});

test("responds to waitpoint when server returns empty accepted response", async () => {
  let requestedUrl: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input);
    return new Response(null, { status: 202 });
  }) as typeof fetch;

  await respondWaitpoint("wait-1", "project-1", "env-1", { action: "approve" });

  expect(requestedUrl).toBe("/api/projects/project-1/environments/env-1/waitpoints/wait-1/respond");
});

test("does not create delay wait confirmation links", async () => {
  let called = false;
  globalThis.fetch = (async () => {
    called = true;
    return Response.json({});
  }) as typeof fetch;

  await expect(createWaitpointResponseToken("wait-delay", "delay", "project-1", "env-1")).rejects.toThrow(
    "Delay waitpoints do not support confirmation links.",
  );
  expect(called).toBe(false);
});
