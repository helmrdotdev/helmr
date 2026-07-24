import { afterEach, expect, test } from "bun:test";

import { getWorkspace } from "./workspaces";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("loads a Workspace by public ID", async () => {
  let requestedURL: string | undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURL = String(input);
    return Response.json({ id: "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa" });
  }) as typeof fetch;

  await getWorkspace("wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa", {
    projectID: "proj_aaaaaaaaaaaaaaaaaaaaaaaaaa",
    environmentID: "env_aaaaaaaaaaaaaaaaaaaaaaaaaa",
  });

  expect(requestedURL).toBe(
    "/api/projects/proj_aaaaaaaaaaaaaaaaaaaaaaaaaa/environments/env_aaaaaaaaaaaaaaaaaaaaaaaaaa/workspaces/wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa",
  );
});
