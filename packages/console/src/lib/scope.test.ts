import { expect, test } from "bun:test";

import { resolveProjectSelection, resolveScopeID, type Project } from "./projects";

const firstPage: Project[] = [
  {
    id: "default-id",
    slug: "default",
    name: "Default",
    default_region_id: "region",
    is_default: true,
    created_at: "2026-09-02T00:00:00Z",
    updated_at: "2026-09-02T00:00:00Z",
  },
];

test("does not replace a persisted project while detail is pending", () => {
  expect(resolveProjectSelection(firstPage, "page-two-id", undefined, false)).toEqual({
    project: undefined,
    settled: false,
  });
});

test("uses resolved detail for a project beyond the loaded page", () => {
  const detail = { ...firstPage[0], id: "page-two-id", slug: "page-two", is_default: false, environments: [] };
  expect(resolveProjectSelection(firstPage, "page-two-id", detail, false)).toEqual({
    project: detail,
    settled: true,
  });
});

test("falls back only after detail returns not found", () => {
  expect(resolveProjectSelection(firstPage, "deleted-id", undefined, true)).toEqual({
    project: firstPage[0],
    settled: true,
  });
});

test("does not reselect a not-found project from a stale page", () => {
  const stale = { ...firstPage[0], id: "deleted-id", slug: "deleted", is_default: false };
  expect(resolveProjectSelection([stale, firstPage[0]], "deleted-id", undefined, true)).toEqual({
    project: firstPage[0],
    settled: true,
  });
});

test("definitive not-found wins over stale cached detail", () => {
  const stale = { ...firstPage[0], id: "deleted-id", slug: "deleted", is_default: false };
  expect(resolveProjectSelection([stale, firstPage[0]], "deleted-id", stale, true)).toEqual({
    project: firstPage[0],
    settled: true,
  });
});

test("not-found with no fallback stays settled without reselecting the stale project", () => {
  const stale = { ...firstPage[0], id: "deleted-id", slug: "deleted", is_default: false };
  expect(resolveProjectSelection([stale], "deleted-id", stale, true)).toEqual({
    project: undefined,
    settled: true,
  });
});

test("consecutive not-found fallbacks exclude every rejected project", () => {
  const projectA = { ...firstPage[0], id: "deleted-a", slug: "deleted-a", is_default: true };
  const projectB = { ...firstPage[0], id: "deleted-b", slug: "deleted-b", is_default: false };
  expect(resolveProjectSelection([projectA, projectB], "deleted-b", projectB, true, new Set(["deleted-a"]))).toEqual({
    project: undefined,
    settled: true,
  });
});

test("scope IDs preserve pending requests but clear settled unresolved values", () => {
  expect(resolveScopeID(undefined, "requested-id", false)).toBe("requested-id");
  expect(resolveScopeID(undefined, "requested-id", true)).toBe("");
  expect(resolveScopeID("fallback-id", "requested-id", true)).toBe("fallback-id");
});
