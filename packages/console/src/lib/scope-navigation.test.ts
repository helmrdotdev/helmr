import { expect, test } from "bun:test";
import { scopeSwitchPath } from "./scope-navigation";
test("scope selection leaves the old resource for its collection", () => {
  for (const resource of ["agents", "templates", "sessions", "computers", "deployments"]) {
    expect(scopeSwitchPath(`/${resource}/old-id`)).toBe(`/${resource}`);
    expect(scopeSwitchPath(`/${resource}`)).toBe(`/${resource}`);
  }
  expect(scopeSwitchPath("/settings/secrets")).toBe("/settings/secrets");
});
