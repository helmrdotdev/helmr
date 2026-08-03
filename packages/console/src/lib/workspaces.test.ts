import { afterEach, expect, test } from "bun:test";

import { getWorkspace } from "./workspaces";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("loads a Workspace by ID", async () => {
  let requestedURL: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURL = String(input);
    return Response.json({ id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32" });
  }) as typeof fetch;

  await getWorkspace("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32", {
    projectID: "proj_aaaaaaaaaaaaaaaaaaaaaaaaaa",
    environmentID: "env_aaaaaaaaaaaaaaaaaaaaaaaaaa",
  });

  expect(requestedURL).toBe(
    "/api/projects/proj_aaaaaaaaaaaaaaaaaaaaaaaaaa/environments/env_aaaaaaaaaaaaaaaaaaaaaaaaaa/workspaces/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
  );
});
