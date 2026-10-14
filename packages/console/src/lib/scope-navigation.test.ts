import { expect, test } from "bun:test";
import { scopeSwitchPath } from "./scope-navigation";
test("scope selection leaves the old resource for its collection", () => {
  for (const resource of ["templates", "sessions", "computers", "deployments"]) {
    expect(scopeSwitchPath(`/${resource}/old-id`)).toBe(`/${resource}`);
    expect(scopeSwitchPath(`/${resource}`)).toBe(`/${resource}`);
  }
  expect(scopeSwitchPath("/agents")).toBe("/agents");
  expect(scopeSwitchPath("/deployments/old-id/agents/support")).toBe("/deployments");
  expect(scopeSwitchPath("/settings/secrets")).toBe("/settings/secrets");
});
test("scope selection keeps an Agent page because Agents share names across environments", () => {
  expect(scopeSwitchPath("/agents/support")).toBe("/agents/support");
});
