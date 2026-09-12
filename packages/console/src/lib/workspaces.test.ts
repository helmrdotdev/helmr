import { afterEach, expect, test } from "bun:test";

import {
  createWorkspace,
  decodeExecOutput,
  deleteWorkspace,
  execWorkspace,
  getWorkspace,
  getWorkspaceExec,
  isTerminalExecStatus,
  listWorkspaces,
  parseExecCommand,
} from "./workspaces";

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

test("loads a Workspace by ID", async () => {
  const calls = captureFetch({ id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32" });
  await getWorkspace("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32", scope);
  expect(calls[0]?.url).toBe(`${base}/workspaces/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32`);
});

test("lists Workspaces with a bounded cursor", async () => {
  const calls = captureFetch({ workspaces: [] });
  await listWorkspaces({ projectID: "project/1", environmentID: "env-1" }, { cursor: "c1", limit: 100 });
  expect(calls[0]?.url).toBe("/api/projects/project%2F1/environments/env-1/workspaces?cursor=c1&limit=100");
});

test("creates a Workspace from a Sandbox with key, placements and idempotency key", async () => {
  const calls = captureFetch({ id: "ws-1", secrets: [] }, 201);
  await createWorkspace("agent/box", scope, {
    key: "main",
    secrets: [{ name: "API_TOKEN", env: "API_TOKEN" }, { name: "cert", file: "/run/cert.pem" }],
    idempotency_key: "key-1",
  });
  expect(calls[0]?.url).toBe(`${base}/sandboxes/agent%2Fbox/workspaces`);
  expect(calls[0]?.init?.method).toBe("POST");
  expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({
    key: "main",
    secrets: [{ name: "API_TOKEN", env: "API_TOKEN" }, { name: "cert", file: "/run/cert.pem" }],
    idempotency_key: "key-1",
  });
});

test("creates a Workspace without optional fields", async () => {
  const calls = captureFetch({ id: "ws-1", secrets: [] }, 201);
  await createWorkspace("box", scope, { idempotency_key: "key-2" });
  expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({ idempotency_key: "key-2" });
});

test("deletes a Workspace with an idempotency key body", async () => {
  const calls = captureFetch({ workspace_id: "ws/1" }, 202);
  await deleteWorkspace("ws/1", scope, { idempotency_key: "key-3" });
  expect(calls[0]?.url).toBe(`${base}/workspaces/ws%2F1`);
  expect(calls[0]?.init?.method).toBe("DELETE");
  expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({ idempotency_key: "key-3" });
});

test("submits an exec with argv, cwd, timeout and idempotency key", async () => {
  const calls = captureFetch({ process_id: "p-1", status: "pending" }, 202);
  const process = await execWorkspace("ws-1", scope, {
    command: ["ls", "-la"],
    cwd: "/work",
    timeout: "30s",
    idempotency_key: "key-4",
  });
  expect(calls[0]?.url).toBe(`${base}/workspaces/ws-1/exec`);
  expect(calls[0]?.init?.method).toBe("POST");
  expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({
    command: ["ls", "-la"],
    cwd: "/work",
    timeout: "30s",
    idempotency_key: "key-4",
  });
  expect(process.status).toBe("pending");
});

test("polls an exec process by ID", async () => {
  const calls = captureFetch({ process_id: "p/1", status: "exited", exit_code: 0, stdout_base64: "aGk=", stderr_base64: "" });
  const process = await getWorkspaceExec("ws-1", "p/1", scope);
  expect(calls[0]?.url).toBe(`${base}/workspaces/ws-1/exec/p%2F1`);
  expect(calls[0]?.init?.method).toBeUndefined();
  expect(process.exit_code).toBe(0);
});

test("exec status is terminal once exited or failed", () => {
  expect(isTerminalExecStatus("pending")).toBe(false);
  expect(isTerminalExecStatus("running")).toBe(false);
  expect(isTerminalExecStatus("exited")).toBe(true);
  expect(isTerminalExecStatus("failed")).toBe(true);
});

test("parses the command line into argv on whitespace", () => {
  expect(parseExecCommand("  ls   -la\t/work ")).toEqual(["ls", "-la", "/work"]);
  expect(parseExecCommand("")).toEqual([]);
});

test("decodes base64 exec output as UTF-8", () => {
  expect(decodeExecOutput("aGVsbG8g4pyT")).toBe("hello ✓");
  expect(decodeExecOutput(undefined)).toBe("");
  expect(decodeExecOutput("")).toBe("");
});

test("requires a scope", () => {
  expect(listWorkspaces({ projectID: "", environmentID: "env-1" })).rejects.toThrow("Workspace project and environment are required");
});
