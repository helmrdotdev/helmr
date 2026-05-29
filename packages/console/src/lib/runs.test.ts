import { afterEach, expect, test } from "bun:test";

import { getRunEvents, listRunEvents, listRuns } from "./runs";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("defaults to all filter when called with no arguments", async () => {
  let requestedUrl: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input);
    return Response.json({ runs: [] });
  }) as typeof fetch;

  await listRuns();

  expect(requestedUrl).toContain("status=all");
});

test("gets run events with cursor and limit", async () => {
  let requestedUrl: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input);
    return Response.json({ events: [], cursor: 7, next_cursor: null });
  }) as typeof fetch;

  await getRunEvents("run-1", { cursor: 7, limit: 50 });

  expect(requestedUrl).toBe("/api/runs/run-1/events?cursor=7&limit=50");
});

test("gets run events without empty params and escapes ids", async () => {
  let requestedUrl: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input);
    return Response.json({ events: [], cursor: 0, next_cursor: null });
  }) as typeof fetch;

  await getRunEvents("run/1");

  expect(requestedUrl).toBe("/api/runs/run%2F1/events");
});

test("lists run events across pages", async () => {
  const requestedUrls: string[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = String(input);
    requestedUrls.push(url);
    if (!url.includes("cursor=2")) {
      return Response.json({
        events: [{ id: "1", kind: "execution", message: "run.created", at: "2026-05-29T00:00:00Z", attributes: {} }],
        cursor: 0,
        next_cursor: 2,
      });
    }
    return Response.json({
      events: [{ id: "2", kind: "execution", message: "run.completed", at: "2026-05-29T00:00:01Z", attributes: {} }],
      cursor: 2,
      next_cursor: null,
    });
  }) as typeof fetch;

  const page = await listRunEvents("run-1", 100);

  expect(requestedUrls).toEqual([
    "/api/runs/run-1/events?limit=100",
    "/api/runs/run-1/events?cursor=2&limit=100",
  ]);
  expect(page.events.map((event) => event.message)).toEqual(["run.created", "run.completed"]);
});

test("stops listing run events when next cursor does not advance", async () => {
  const requestedUrls: string[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrls.push(String(input));
    return Response.json({
      events: [{ id: "1", kind: "execution", message: "run.created", at: "2026-05-29T00:00:00Z", attributes: {} }],
      cursor: 0,
      next_cursor: 0,
    });
  }) as typeof fetch;

  const page = await listRunEvents("run-1", 100);

  expect(requestedUrls).toEqual(["/api/runs/run-1/events?limit=100"]);
  expect(page.events).toHaveLength(1);
});
