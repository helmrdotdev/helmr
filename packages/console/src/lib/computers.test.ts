import { afterEach, expect, test } from "bun:test";

import {
  createComputer,
  decodeCommandLogPage,
  deleteComputer,
  execComputer,
  getComputer,
  getCommand,
  listCommandLogs,
  isTerminalCommandStatus,
  listComputers,
  listComputerMembers,
  parseExecCommand,
} from "./computers";

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

test("creates a Computer from a Sandbox with key, placements and idempotency key", async () => {
  const calls = captureFetch({ id: "ws-1", secrets: [] }, 201);
  await createComputer("agent/box", scope, {
    key: "main",
    secrets: [{ secret: "API_TOKEN", env: { name: "API_TOKEN", mode: "raw" } }, { secret: "cert", file: { path: "/run/cert.pem" } }],
    idempotency_key: "key-1",
  });
  expect(calls[0]?.url).toBe(`${base}/sandboxes/agent%2Fbox/computers`);
  expect(calls[0]?.init?.method).toBe("POST");
  expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({
    key: "main",
    secrets: [{ secret: "API_TOKEN", env: { name: "API_TOKEN", mode: "raw" } }, { secret: "cert", file: { path: "/run/cert.pem" } }],
    idempotency_key: "key-1",
  });
});

test("creates a Computer without optional fields", async () => {
  const calls = captureFetch({ id: "ws-1", secrets: [] }, 201);
  await createComputer("box", scope, { idempotency_key: "key-2" });
  expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({ idempotency_key: "key-2" });
});

test("deletes a Computer with an idempotency key body", async () => {
  const calls = captureFetch({ computer_id: "ws/1" }, 202);
  await deleteComputer("ws/1", scope, { idempotency_key: "key-3" });
  expect(calls[0]?.url).toBe(`${base}/computers/ws%2F1`);
  expect(calls[0]?.init?.method).toBe("DELETE");
  expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({ idempotency_key: "key-3" });
});

test("submits an exec with argv, cwd, timeout and idempotency key", async () => {
  const calls = captureFetch({ command_id: "p-1" }, 202);
  const process = await execComputer("ws-1", scope, {
    command: ["ls", "-la"],
    cwd: "/work",
    timeout: "30s",
    idempotency_key: "key-4",
  });
  expect(calls[0]?.url).toBe(`${base}/computers/ws-1/exec`);
  expect(calls[0]?.init?.method).toBe("POST");
  expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({
    command: ["ls", "-la"],
    cwd: "/work",
    timeout: "30s",
    idempotency_key: "key-4",
  });
  expect(process.command_id).toBe("p-1");
});

test("retrieves Command outcome in the environment scope", async () => {
  const value = { id: "p/1", computer_id: "ws-1", status: "exited", process_reconciled: false, outcome: { command_id: "p/1", kind: "exited", exit_code: 7, terminal_at: "2026-09-27T00:00:00Z" } };
  const calls = captureFetch(value);
  expect(await getCommand("p/1", scope)).toEqual(value);
  expect(calls[0]?.url).toBe(`${base}/commands/p%2F1`);
});

test("reads a bounded Command log page with its scoped cursor and abort signal", async () => {
  const page = { logs: [], output_state: "closed" };
  const calls = captureFetch(page);
  const signal = new AbortController().signal;
  expect(await listCommandLogs("p/1", scope, "cursor/+", signal)).toEqual(page);
  expect(calls[0]?.url).toBe(`${base}/commands/p%2F1/logs?limit=100&cursor=cursor%2F%2B`);
  expect(calls[0]?.init?.signal).toBe(signal);
});

test("Command status distinguishes execution from terminal outcomes", () => {
  expect(isTerminalCommandStatus("pending")).toBe(false);
  expect(isTerminalCommandStatus("running")).toBe(false);
  expect(isTerminalCommandStatus("exited")).toBe(true);
  expect(isTerminalCommandStatus("failed")).toBe(true);
  for (const status of ["cancelled", "timed_out", "lost"] as const) expect(isTerminalCommandStatus(status)).toBe(true);
  for (const status of ["starting", "stopping"] as const) expect(isTerminalCommandStatus(status)).toBe(false);
});

test("parses the command line into argv on whitespace", () => {
  expect(parseExecCommand("  ls   -la\t/work ")).toEqual(["ls", "-la", "/work"]);
  expect(parseExecCommand("")).toEqual([]);
});

test("renders split UTF-8 chunks and explicit output gaps without mixing streams", () => {
  const output = (content_base64: string, stream: "stdout" | "stderr" = "stdout") => ({ kind: "output" as const, stream, cursor: "c", content_base64, observed_at: "2026-09-27T00:00:00Z" });
  expect(decodeCommandLogPage([output("4g=="), output("err", "stderr"), output("nJM=")], "stdout").text).toBe("✓");
  expect(decodeCommandLogPage([{ kind: "gap", stream: "stdout", cursor: "g", from_sequence: "2", through_sequence: "4" }, output("b2s=")], "stdout").text).toBe("\n[Output unavailable: chunks 2–4]\nok");
  const first = decodeCommandLogPage([output("4g==")], "stdout");
  expect(first.text).toBe("");
  expect(Array.from(first.pending)).toEqual([0xe2]);
  expect(decodeCommandLogPage([output("nJM=")], "stdout", first.pending).text).toBe("✓");
  expect(decodeCommandLogPage([], "stdout", first.pending, true).text).toBe("�");
  expect(decodeCommandLogPage([], "stdout").text).toBe("");
});

test("requires a scope", () => {
  expect(listComputers({ projectID: "", environmentID: "env-1" })).rejects.toThrow("Computer project and environment are required");
});

test("creates fixed nested bindings in the selected scope without values", async () => {
  const { createComputer } = await import("./computers");
  let body: unknown;
  let url = "";
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    url = String(input); body = JSON.parse(String(init?.body));
    return Response.json({id:"computer"});
  }) as typeof fetch;
  const bindings = [{secret:"github-token",env:{name:"GH_TOKEN",mode:"protected" as const,allowed_origins:["https://api.github.com"]}}];
  await createComputer("reviewer", {projectID:"project",environmentID:"environment"}, {secrets: bindings, key: "review", idempotency_key: "retry"});
  expect(url).toBe("/api/projects/project/environments/environment/sandboxes/reviewer/computers");
  expect(body).toEqual({secrets:bindings,key:"review",idempotency_key:"retry"});
});
