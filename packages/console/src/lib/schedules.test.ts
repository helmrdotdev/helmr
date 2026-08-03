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
