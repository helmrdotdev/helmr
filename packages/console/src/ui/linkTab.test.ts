import { expect, test } from "bun:test";
import { selectedLinkTab } from "./linkTab";

const tabs = [
  { value: "sessions", href: "/agents/support" },
  { value: "slack", href: "/agents/support/slack" },
] as const;

test("a link tab is selected on its page with or without a trailing slash", () => {
  expect(selectedLinkTab(tabs, "/agents/support/slack")).toBe("slack");
  expect(selectedLinkTab(tabs, "/agents/support/slack/")).toBe("slack");
  expect(selectedLinkTab(tabs, "/agents/support/")).toBe("sessions");
  expect(selectedLinkTab(tabs, "/agents/support")).toBe("sessions");
});
