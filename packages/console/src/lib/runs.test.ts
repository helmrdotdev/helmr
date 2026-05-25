import { afterEach, expect, test } from "bun:test";

import { listRuns } from "./runs";

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
