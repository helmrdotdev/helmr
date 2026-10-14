import { expect, test } from "bun:test";
import { groupEntries, listKey, matchesQuery } from "./command";

test("a query matches when every word is in the label or keywords", () => {
  const entry = { id: "1", label: "Scope Tabs / Staging", keywords: "scope-tabs staging" };
  expect(matchesQuery(entry, "")).toBe(true);
  expect(matchesQuery(entry, "  stag ")).toBe(true);
  expect(matchesQuery(entry, "tabs STAG")).toBe(true);
  expect(matchesQuery(entry, "scope-tabs prod")).toBe(false);
});

test("entries group under their first appearance and number in shown order", () => {
  const groups = groupEntries([
    { id: "a", label: "A", group: "One" },
    { id: "b", label: "B", group: "Two" },
    { id: "c", label: "C", group: "One" },
  ]);
  expect(groups.map((group) => [group.label, group.entries.map(({ entry, index }) => `${entry.id}${index}`)])).toEqual([
    ["One", ["a0", "c1"]],
    ["Two", ["b2"]],
  ]);
});

test("list keys are ignored while an input method composes", () => {
  expect(listKey({ key: "Enter", isComposing: false, keyCode: 13 }, "")).toBe("choose");
  expect(listKey({ key: "Enter", isComposing: true, keyCode: 13 }, "")).toBeUndefined();
  expect(listKey({ key: "ArrowDown", isComposing: false, keyCode: 229 }, "")).toBeUndefined();
  expect(listKey({ key: "a", isComposing: false, keyCode: 65 }, "")).toBeUndefined();
});

test("Home and End move the list only while the search box is empty", () => {
  expect(listKey({ key: "Home", isComposing: false, keyCode: 36 }, "")).toBe("first");
  expect(listKey({ key: "End", isComposing: false, keyCode: 35 }, "")).toBe("last");
  expect(listKey({ key: "Home", isComposing: false, keyCode: 36 }, "sta")).toBeUndefined();
  expect(listKey({ key: "End", isComposing: false, keyCode: 35 }, "sta")).toBeUndefined();
  expect(listKey({ key: "ArrowDown", isComposing: false, keyCode: 40 }, "sta")).toBe("next");
});
