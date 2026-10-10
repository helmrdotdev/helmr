import { test, expect } from "bun:test"
import { mkdtemp, readFile, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { HelmrClient } from "@helmr/sdk"
import { verify, waitOutput } from "./context"

// Failure evidence must survive a broken cleanup action, and later cleanup must run.
test("case failure keeps its verdict and attempts every registered cleanup", async () => {
  const root = await mkdtemp(join(tmpdir(), "helmr-case-"))
  const old = { ...process.env }
  const calls: string[] = []
  try {
    process.env.HELMR_API_URL = "http://127.0.0.1:1"
    process.env.HELMR_API_KEY = "fixture"
    process.env.HELMR_EVIDENCE_DIR = join(root, "evidence")
    delete process.env.HELMR_EXPECTED_BUNDLE_DIGEST
    await expect(verify("failure", async ({ cleanup }) => {
      cleanup(async () => { calls.push("first") })
      cleanup(async () => { calls.push("second"); throw new Error("cleanup failed") })
      throw new Error("assertion failed")
    })).rejects.toThrow("assertion failed")
    expect(calls).toEqual(["second", "first"])
    const result = JSON.parse(await readFile(join(root, "evidence/result.json"), "utf8"))
    expect(result.passed).toBe(false)
    expect(result.failure).toBe("assertion failed")
    expect(result.cleanup).toEqual({ status: "failed", failures: ["cleanup failed"] })
    let started = false
    await expect(verify("overwrite", async () => { started = true })).rejects.toThrow()
    expect(started).toBe(false)
  } finally {
    for (const key of ["HELMR_API_URL", "HELMR_API_KEY", "HELMR_EVIDENCE_DIR", "HELMR_EXPECTED_BUNDLE_DIGEST"]) {
      if (old[key] === undefined) delete process.env[key]
      else process.env[key] = old[key]
    }
    await rm(root, { recursive: true, force: true })
  }
})

for (const cancelFails of [false, true]) {
  test(`owned Sessions are cancelled before resource cleanup (failure=${cancelFails})`, async () => {
    const root = await mkdtemp(join(tmpdir(), "helmr-session-cleanup-"))
    const old = { ...process.env }
    const calls: string[] = []
    const sessionId = "01900000-0000-7000-8000-000000000001"
    const server = Bun.serve({
      hostname: "127.0.0.1", port: 0,
      async fetch(request) {
        calls.push(new URL(request.url).pathname)
        expect(request.method).toBe("POST")
        expect((await request.json()).idempotency_key).toContain(sessionId)
        if (cancelFails) return Response.json({ error: { code: "invalid_state", message: "cannot cancel" } }, { status: 409 })
        return Response.json({ id: "01900000-0000-7000-8000-000000000002", session_id: sessionId, status: "accepted" })
      },
    })
    try {
      process.env.HELMR_API_URL = server.url.toString()
      process.env.HELMR_API_KEY = "fixture"
      process.env.HELMR_EVIDENCE_DIR = join(root, "evidence")
      delete process.env.HELMR_EXPECTED_BUNDLE_DIGEST
      const result = verify("session-cleanup", async ({ objects, cleanup }) => {
        objects.session_ids.push(sessionId, sessionId)
        cleanup(async () => { calls.push("delete-computer") })
        return { completed: true }
      })
      if (cancelFails) await expect(result).rejects.toThrow()
      else await result
      expect(calls).toEqual([`/v1/sessions/${sessionId}/cancel`, "delete-computer"])
      const evidence = JSON.parse(await readFile(join(root, "evidence/result.json"), "utf8"))
      expect(evidence.passed).toBe(!cancelFails)
      expect(evidence.cleanup.status).toBe(cancelFails ? "failed" : "requests-accepted")
      if (cancelFails) expect(evidence.cleanup.failures[0]).toContain(sessionId)
    } finally {
      server.stop(true)
      for (const key of ["HELMR_API_URL", "HELMR_API_KEY", "HELMR_EVIDENCE_DIR", "HELMR_EXPECTED_BUNDLE_DIGEST"]) {
        if (old[key] === undefined) delete process.env[key]
        else process.env[key] = old[key]
      }
      await rm(root, { recursive: true, force: true })
    }
  })
}

for (const [signal, status] of [["SIGTERM", 143], ["SIGINT", 130]] as const) {
  test(`${signal} preserves owned resources for caller cleanup`, async () => {
    const root = await mkdtemp(join(tmpdir(), "helmr-case-interrupt-"))
    const output = join(root, "evidence")
    const child = Bun.spawn([process.execPath, "-e", `
      import { verify } from ${JSON.stringify(import.meta.dir + "/context.ts")};
      await verify("interrupt", async ({ objects, cleanup }) => {
        objects.computer_ids.push("computer-owned");
        objects.secret_ids.push("secret-owned");
        cleanup(async () => { throw new Error("must not race cleanup with the body"); });
        console.log("ready");
        await new Promise(() => { setInterval(() => {}, 1000); });
      });
    `], {
      env: {
        ...process.env,
        HELMR_API_URL: "http://127.0.0.1:1", HELMR_API_KEY: "fixture",
        HELMR_EVIDENCE_DIR: output, HELMR_EXPECTED_BUNDLE_DIGEST: "",
      },
      stdout: "pipe", stderr: "pipe",
    })
    try {
      const reader = child.stdout.getReader()
      const first = await reader.read()
      expect(new TextDecoder().decode(first.value)).toContain("ready")
      child.kill(signal)
      expect(await child.exited).toBe(status)
      const result = JSON.parse(await readFile(join(output, "result.json"), "utf8"))
      expect(result.passed).toBe(false)
      expect(result.failure).toBe(`Interrupted by ${signal}`)
      expect(result.cleanup.status).toBe("interrupted")
      expect(result.objects.computer_ids).toEqual(["computer-owned"])
      expect(result.objects.secret_ids).toEqual(["secret-owned"])
    } finally {
      child.kill()
      await child.exited
      await rm(root, { recursive: true, force: true })
    }
  })
}


test("progress wait reads canonical Content through the SDK before terminal settlement", async () => {
  const sessionID = "01900000-0000-7000-8000-000000000011"
  const turnID = "01900000-0000-7000-8000-000000000012"
  const otherTurnID = "01900000-0000-7000-8000-000000000013"
  const cursors: string[] = []
  const event = (sequence: number, turn_id: string, kind: string, data: unknown) => ({
    session_id: sessionID, turn_id, sequence, kind, data, created_at: "2026-10-09T00:00:00Z",
  })
  const server = Bun.serve({ hostname: "127.0.0.1", port: 0, fetch(request) {
    const url = new URL(request.url)
    expect(url.pathname).toBe(`/v1/sessions/${sessionID}/events`)
    const after = url.searchParams.get("after")!
    cursors.push(after)
    if (after === "0") return Response.json({
      records: [event(1, otherTurnID, "turn.completed", null)], next_after: 1, has_more: true, retained_after: 0,
    })
    return Response.json({ records: [
      event(2, turnID, "turn.output", [{ type: "text", text: "ready" }, { type: "json", value: null }]),
      event(3, turnID, "turn.completed", null),
    ], next_after: 3, has_more: false, retained_after: 0 })
  } })
  try {
    const client = new HelmrClient({ url: server.url.toString(), apiKey: "fixture" })
    const session = client.sessions.get(sessionID)
    expect(await waitOutput(session, session.turn(turnID), value => value === null, 1000)).toBeNull()
    expect(cursors).toEqual(["0", "1"])
  } finally { server.stop(true) }
})
