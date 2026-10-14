import { expect, test } from "bun:test";
import { scopeSwitchPath } from "./scope-navigation";

test("scope selection leaves the old resource for its collection", () => {
  for (const resource of ["sessions", "computers", "deployments"]) {
    expect(scopeSwitchPath(`/${resource}/old-id`)).toBe(`/${resource}`);
  }
  expect(scopeSwitchPath("/sessions/old-id/turns/turn/asks/ask")).toBe("/sessions");
});

test("scope selection keeps section pages and paths outside the sections", () => {
  for (const section of ["/agents", "/sessions", "/triggers", "/computers", "/templates", "/deployments"]) {
    expect(scopeSwitchPath(section)).toBe(section);
  }
  expect(scopeSwitchPath("/")).toBe("/");
  expect(scopeSwitchPath("/settings/secrets")).toBe("/settings/secrets");
});

test("scope selection keeps an Agent page and tab because Agents share names across environments", () => {
  expect(scopeSwitchPath("/agents/support")).toBe("/agents/support");
  expect(scopeSwitchPath("/agents/support/slack")).toBe("/agents/support/slack");
});
