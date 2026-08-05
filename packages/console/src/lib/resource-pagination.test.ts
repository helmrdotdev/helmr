import { afterEach, expect, test } from "bun:test";

import { listApiKeys } from "./api-keys";
import { listInvitations } from "./members";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("passes the API key collection cursor with its filter and scope", async () => {
  let requestedURL = "";
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURL = String(input);
    return Response.json({ api_keys: [] });
  }) as typeof fetch;

  await listApiKeys("project/1", "environment/1", "revoked", "cursor+/=");

  expect(requestedURL).toBe(
    "/api/projects/project%2F1/environments/environment%2F1/api-keys?filter=revoked&cursor=cursor%2B%2F%3D",
  );
});

test("passes the invitation collection cursor", async () => {
  let requestedURL = "";
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedURL = String(input);
    return Response.json({ invitations: [] });
  }) as typeof fetch;

  await listInvitations("cursor+/=");

  expect(requestedURL).toBe("/api/invitations?cursor=cursor%2B%2F%3D");
});
