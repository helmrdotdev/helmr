import { afterEach, expect, test } from "bun:test";

import { getProject, listProjects, updateEnvironment } from "./projects";

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

test("environment update patches name and color and resends the slug", async () => {
  let requested = "";
  let init: RequestInit | undefined;
  globalThis.fetch = (async (input, requestInit) => {
    requested = String(input);
    init = requestInit;
    return Response.json({ id: "env-1", slug: "staging", name: "Staging 2", color_hex: "#F59E0B" });
  }) as typeof fetch;

  const environment = await updateEnvironment("project/1", "env/1", { slug: "staging", name: "Staging 2", color_hex: "#F59E0B" });

  expect(requested).toBe("/api/projects/project%2F1/environments/env%2F1");
  expect(init?.method).toBe("PATCH");
  expect(JSON.parse(String(init?.body))).toEqual({ slug: "staging", name: "Staging 2", color_hex: "#F59E0B" });
  expect(environment.name).toBe("Staging 2");
});
