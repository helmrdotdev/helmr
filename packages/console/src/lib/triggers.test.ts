import "./browser-test-env";
import { afterEach, expect, test } from "bun:test";

import { listTriggers } from "./triggers";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("lists source-declared triggers with project and environment scope", async () => {
  let requestedUrl: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input);
    return Response.json({ triggers: [] });
  }) as typeof fetch;

  await listTriggers({ projectID: "project-1", environmentID: "env-1" });

  expect(requestedUrl).toBe("/api/projects/project-1/environments/env-1/triggers");
});

 test("preserves opaque pagination cursors", async () => {
 let requestedUrl = "";
 globalThis.fetch = (async (input: RequestInfo | URL) => { requestedUrl=String(input);return Response.json({triggers:[],next_cursor:"next"}); }) as typeof fetch;
 const result=await listTriggers({projectID:"p",environmentID:"e"}, "cursor/+&");
 expect(requestedUrl).toBe("/api/projects/p/environments/e/triggers?cursor=cursor%2F%2B%26");
 expect(result.next_cursor).toBe("next");
 });
