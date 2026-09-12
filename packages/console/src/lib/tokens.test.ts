import { afterEach, expect, test } from "bun:test";

import { cancelToken, completeToken, listTokens } from "./tokens";

const originalFetch = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = originalFetch;
});

function captureFetch(body: unknown) {
  const calls: { url: string; init: RequestInit | undefined }[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ url: String(input), init });
    return Response.json(body);
  }) as typeof fetch;
  return calls;
}

test("lists tokens without filters", async () => {
  const calls = captureFetch({ tokens: [] });
  await listTokens({ projectID: "project/1", environmentID: "env-1" });
  expect(calls[0]?.url).toBe("/api/projects/project%2F1/environments/env-1/tokens");
});

test("lists pending tokens with a bounded page", async () => {
  const calls = captureFetch({ tokens: [], next_cursor: "next" });
  const page = await listTokens({ projectID: "project-1", environmentID: "env-1" }, { status: "pending", limit: 99, cursor: "c1" });
  expect(calls[0]?.url).toBe("/api/projects/project-1/environments/env-1/tokens?status=pending&cursor=c1&limit=99");
  expect(page.next_cursor).toBe("next");
});

test("completes a token with a JSON result and idempotency key", async () => {
  const calls = captureFetch({ id: "tok/1", status: "completed", tags: [] });
  await completeToken("tok/1", { projectID: "project-1", environmentID: "env-1" }, {
    result: { approved: true },
    idempotency_key: "key-1",
  });
  expect(calls[0]?.url).toBe("/api/projects/project-1/environments/env-1/tokens/tok%2F1/complete");
  expect(calls[0]?.init?.method).toBe("POST");
  expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({ result: { approved: true }, idempotency_key: "key-1" });
});

test("cancels a token with an idempotency key", async () => {
  const calls = captureFetch({ id: "tok-1", status: "cancelled", tags: [] });
  await cancelToken("tok-1", { projectID: "project-1", environmentID: "env-1" }, { idempotency_key: "key-2" });
  expect(calls[0]?.url).toBe("/api/projects/project-1/environments/env-1/tokens/tok-1/cancel");
  expect(calls[0]?.init?.method).toBe("POST");
  expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({ idempotency_key: "key-2" });
});

test("requires a scope", () => {
  expect(listTokens({ projectID: "", environmentID: "env-1" })).rejects.toThrow("Token project and environment are required");
});
