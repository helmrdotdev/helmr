import { afterEach, expect, test } from "bun:test";

import { listSchedules } from "./schedules";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("lists source-declared schedules with project and environment scope", async () => {
  let requestedUrl: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input);
    return Response.json({ schedules: [] });
  }) as typeof fetch;

  await listSchedules({ projectID: "project-1", environmentID: "env-1" });

  expect(requestedUrl).toBe("/api/projects/project-1/environments/env-1/schedules");
});

 test("preserves opaque pagination cursors", async () => {
 let requestedUrl = "";
 globalThis.fetch = (async (input: RequestInfo | URL) => { requestedUrl=String(input);return Response.json({schedules:[],next_cursor:"next"}); }) as typeof fetch;
 const result=await listSchedules({projectID:"p",environmentID:"e"}, "cursor/+&");
 expect(requestedUrl).toBe("/api/projects/p/environments/e/schedules?cursor=cursor%2F%2B%26");
 expect(result.next_cursor).toBe("next");
 });
