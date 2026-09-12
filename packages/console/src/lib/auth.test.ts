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
  const developer: Me = { ...base, permissions: ["runs.read", "runs.manage", "tasks.deploy"] };
  expect(hasPermission(developer, "runs.manage")).toBe(true);
  expect(hasPermission(developer, "tasks.deploy")).toBe(true);
  expect(hasPermission(developer, "tokens.complete")).toBe(false);
});

test("denies everything for a viewer-like session and while unauthenticated", () => {
  const viewer: Me = { ...base, permissions: ["runs.read", "sessions.read", "tokens.read", "workspaces.read"] };
  for (const permission of ["runs.manage", "tokens.complete", "tokens.cancel", "tasks.deploy"]) {
    expect(hasPermission(viewer, permission)).toBe(false);
  }
  expect(hasPermission(undefined, "runs.read")).toBe(false);
  expect(hasPermission(base, "runs.read")).toBe(false);
});
