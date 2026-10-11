import { expect, test } from "bun:test";

import { hasPermission, type Me } from "./auth";

const base: Me = {
  user_id: "u1",
  display_name: null,
  profile_image_url: null,
  admin: false,
  permissions: [],
  organization_required: false,
  project_required: false,
};

test("grants only permissions the session reports", () => {
  const developer: Me = { ...base, permissions: ["sessions.read", "sessions.cancel", "deployments.write"] };
  expect(hasPermission(developer, "sessions.cancel")).toBe(true);
  expect(hasPermission(developer, "deployments.write")).toBe(true);
  expect(hasPermission(developer, "asks.respond")).toBe(false);
});

test("denies everything for a viewer-like session and while unauthenticated", () => {
  const viewer: Me = { ...base, permissions: ["sessions.read", "computers.read"] };
  for (const permission of ["sessions.cancel", "asks.respond", "sessions.interrupt", "deployments.write"]) {
    expect(hasPermission(viewer, permission)).toBe(false);
  }
  expect(hasPermission(undefined, "sessions.read")).toBe(false);
  expect(hasPermission(base, "sessions.read")).toBe(false);
});

test("account lookup leaves 401 routing to the authentication guard", async () => {
  const { getMe } = await import("./auth");
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (async () => Response.json({ error: { code: "unauthorized", message: "expired" } }, { status: 401 })) as typeof fetch;
  try {
    await expect(getMe()).rejects.toMatchObject({ status: 401 });
  } finally {
    globalThis.fetch = originalFetch;
  }
});
