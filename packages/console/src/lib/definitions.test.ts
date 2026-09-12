import { afterEach, expect, test } from "bun:test";

import { listActors, listSandboxes, listTasks, startActor, startTask } from "./definitions";

const originalFetch = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = originalFetch;
});

function captureFetch(body: unknown) {
  const urls: string[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    urls.push(String(input));
    return Response.json(body);
  }) as typeof fetch;
  return urls;
}

const scope = { projectID: "project-1", environmentID: "env-1" };

test("lists current-deployment tasks without params", async () => {
  const urls = captureFetch({ deployment_id: "d-1", tasks: [] });
  await listTasks(scope);
  expect(urls[0]).toBe("/api/projects/project-1/environments/env-1/tasks");
});

test("lists definitions for a specific deployment", async () => {
  const urls = captureFetch({ deployment_id: "d/1", tasks: [], actors: [], sandboxes: [] });
  await listTasks({ ...scope, deploymentID: "d/1", limit: 100 });
  await listActors({ ...scope, deploymentID: "d/1", cursor: "c1" });
  await listSandboxes({ ...scope, deploymentID: "d/1" });
  expect(urls).toEqual([
    "/api/projects/project-1/environments/env-1/tasks?deployment_id=d%2F1&limit=100",
    "/api/projects/project-1/environments/env-1/actors?deployment_id=d%2F1&cursor=c1",
    "/api/projects/project-1/environments/env-1/sandboxes?deployment_id=d%2F1",
  ]);
});

function captureRequests(body: unknown) {
  const calls: { url: string; init: RequestInit | undefined }[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ url: String(input), init });
    return Response.json(body, { status: 201 });
  }) as typeof fetch;
  return calls;
}

test("starts a task with a workspace target and payload", async () => {
  const calls = captureRequests({ run_id: "run-1" });
  const result = await startTask("send/email", scope, {
    workspace: { id: "ws-1" },
    payload: { to: "a@example.com" },
    idempotency_key: "key-1",
  });
  expect(calls[0]?.url).toBe("/api/projects/project-1/environments/env-1/tasks/send%2Femail/start");
  expect(calls[0]?.init?.method).toBe("POST");
  expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({
    workspace: { id: "ws-1" },
    payload: { to: "a@example.com" },
    idempotency_key: "key-1",
  });
  expect(result.run_id).toBe("run-1");
});

test("starts a task without a payload when none is given", async () => {
  const calls = captureRequests({ run_id: "run-2" });
  await startTask("nightly", scope, { workspace: { id: "ws-1" }, idempotency_key: "key-2" });
  expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({ workspace: { id: "ws-1" }, idempotency_key: "key-2" });
});

test("starts an actor with a workspace, key and initial input", async () => {
  const calls = captureRequests({ session_id: "sess-1", run_id: "run-3" });
  const result = await startActor("support/agent", scope, {
    workspace: { id: "ws-2" },
    key: "customer-42",
    input: { greeting: "hi" },
    idempotency_key: "key-3",
  });
  expect(calls[0]?.url).toBe("/api/projects/project-1/environments/env-1/actors/support%2Fagent/start");
  expect(calls[0]?.init?.method).toBe("POST");
  expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({
    workspace: { id: "ws-2" },
    key: "customer-42",
    input: { greeting: "hi" },
    idempotency_key: "key-3",
  });
  expect(result.session_id).toBe("sess-1");
});

test("starts an actor without optional key or input", async () => {
  const calls = captureRequests({ session_id: "sess-2", run_id: "run-4" });
  await startActor("agent", scope, { workspace: { id: "ws-2" }, idempotency_key: "key-4" });
  expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({ workspace: { id: "ws-2" }, idempotency_key: "key-4" });
});
