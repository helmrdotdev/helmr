import { afterEach, expect, test } from "bun:test";

import { getProject, listProjects } from "./projects";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("projects pagination sends cursor and limit", async () => {
  let requested = "";
  globalThis.fetch = (async (input) => {
    requested = String(input);
    return Response.json({ projects: [], next_cursor: "next" });
  }) as typeof fetch;

  const page = await listProjects("cursor value", 50);

  expect(requested).toBe("/api/projects?cursor=cursor+value&limit=50");
  expect(page.next_cursor).toBe("next");
});

test("project detail encodes the project reference", async () => {
  let requested = "";
  globalThis.fetch = (async (input) => {
    requested = String(input);
    return Response.json({ id: "project-id", slug: "project slug", environments: [] });
  }) as typeof fetch;

  await getProject("project slug");

  expect(requested).toBe("/api/projects/project%20slug");
});
