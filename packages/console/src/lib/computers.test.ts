import { afterEach, expect, test } from "bun:test";

import { getComputer, listComputers, listComputerMembers } from "./computers";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

function captureFetch(body: unknown, status = 200) {
  const calls: { url: string; init: RequestInit | undefined }[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ url: String(input), init });
    return Response.json(body, { status });
  }) as typeof fetch;
  return calls;
}

const scope = {
  projectID: "proj_aaaaaaaaaaaaaaaaaaaaaaaaaa",
  environmentID: "env_aaaaaaaaaaaaaaaaaaaaaaaaaa",
};
const base = "/api/projects/proj_aaaaaaaaaaaaaaaaaaaaaaaaaa/environments/env_aaaaaaaaaaaaaaaaaaaaaaaaaa";

test("loads a Computer by ID", async () => {
  const calls = captureFetch({ id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32" });
  await getComputer("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32", scope);
  expect(calls[0]?.url).toBe(`${base}/computers/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32`);
});

test("lists Computers with a bounded cursor", async () => {
  const calls = captureFetch({ computers: [] });
  await listComputers({ projectID: "project/1", environmentID: "env-1" }, { cursor: "c1", limit: 100 });
  expect(calls[0]?.url).toBe("/api/projects/project%2F1/environments/env-1/computers?cursor=c1&limit=100");
});

test("lists Computer members within scope and preserves pagination", async () => {
  const page = { members: [{ kind: "session", id: "session-1", state: "admitted", created_at: "2026-09-26T00:00:00Z" }], next_cursor: "next" };
  const calls = captureFetch(page);
  const result = await listComputerMembers("computer/1", scope, { cursor: "after/+", limit: 50 });
  expect(calls[0]?.url).toBe(`${base}/computers/computer%2F1/members?cursor=after%2F%2B&limit=50`);
  expect(result).toEqual(page);
});

test("requires a scope", () => {
  expect(listComputers({ projectID: "", environmentID: "env-1" })).rejects.toThrow("Computer project and environment are required");
});
