import { expect, test } from "bun:test";

import { isOrganizationAdmin } from "./me";

test("owners and admins administer the organization", () => {
  expect(isOrganizationAdmin("owner")).toBe(true);
  expect(isOrganizationAdmin("admin")).toBe(true);
});

test("other roles and a missing role do not", () => {
  for (const role of ["developer", "viewer", "", null, undefined]) expect(isOrganizationAdmin(role)).toBe(false);
});
