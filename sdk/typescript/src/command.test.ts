import { describe, expect, test } from "bun:test"
import { HelmrClient } from "./client"
import { installRuntimeMcp } from "./internal/mcp"
import { parseCancelReceipt, parseCommandInfo, waitForCommand, type CommandInfo } from "./command"

const id = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36"
const computerId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"
const running: CommandInfo = { id, computerId, status: "running", processReconciled: false }

describe("Command observation", () => {
  test("abort stops a pending polling delay", async () => {
    const controller = new AbortController()
    let reads = 0
    const wait = waitForCommand(id, async () => { reads++; return running }, { signal: controller.signal })
    const observed = wait.catch(error => error)
    await new Promise(resolve => setTimeout(resolve, 10))
    const reason = new Error("stop observing")
    controller.abort(reason)
    expect(await observed).toBe(reason)
    expect(reads).toBe(1)
  })

  test("timeout interrupts a stalled status request without cancelling the execution", async () => {
    const wait = waitForCommand(id, (signal) => new Promise((_resolve, reject) => {
      signal!.addEventListener("abort", () => reject(signal!.reason), { once: true })
    }), { waitTimeout: "10ms" })
    await expect(wait).rejects.toMatchObject({ name: "TimeoutError" })
  })

  test("nonzero exit and pending cleanup are an available outcome", async () => {
    const info = parseCommandInfo({
      id, computer_id: computerId, status: "exited", process_reconciled: false,
      outcome: { command_id: id, kind: "exited", exit_code: 17, terminal_at: "2026-09-26T00:00:00Z" },
    }, id)
    expect(await waitForCommand(id, async () => info)).toMatchObject({ kind: "exited", exitCode: 17 })
    expect(info.processReconciled).toBe(false)
  })

  test("malformed terminal results and changed identities are rejected", () => {
    const wire = { id, computer_id: computerId, status: "exited", process_reconciled: false }
    expect(() => parseCommandInfo(wire)).toThrow("outcome")
    expect(() => parseCommandInfo({ ...wire, outcome: { command_id: id, kind: "cancelled", terminal_at: "2026-09-26T00:00:00Z" } })).toThrow("kind")
    expect(() => parseCommandInfo({ ...wire, status: "running" }, computerId)).toThrow("changed ID")
  })
})

test("Command cancellation receipt validates operation and target identities", () => {
  const wire = { id: computerId, target_id: id, status: "accepted" }
  expect(parseCancelReceipt(wire, id)).toEqual({ id: computerId, targetId: id, status: "accepted" })
  expect(() => parseCancelReceipt(wire, computerId)).toThrow("target ID")
  expect(() => parseCancelReceipt({ ...wire, status: "cancelled" }, id)).toThrow("status")
  expect(() => parseCancelReceipt({ ...wire, id: "invalid" }, id)).toThrow()
})

test("client cancellation addresses one Command and returns a receipt, not an outcome", async () => {
  const { createClientCommandRef } = await import("./client-computer")
  const calls: unknown[] = []
  const signal = new AbortController().signal
  const ref = createClientCommandRef(id, {
    async request(method, path, options) {
      calls.push({ method, path, options })
      return { id: computerId, target_id: id, status: "accepted" }
    },
  })
  const receipt = await ref.cancel({ signal })
  expect(receipt).toEqual({ id: computerId, targetId: id, status: "accepted" })
  expect(calls).toEqual([{ method: "POST", path: `/v1/commands/${id}/cancel`, options: { signal } }])
})

test("explicit client command authority is independent of the installed runtime", async () => {
  const uninstall = installRuntimeMcp(async () => { throw new Error("explicit client must not invoke runtime authority") })
  const signal = new AbortController().signal
  const calls: Array<{ path: string; init: RequestInit | undefined }> = []
  let denied = false
  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "explicit-key", fetch: (async (input, init) => {
    const path = new URL(String(input)).pathname
    calls.push({ path, init })
    if (denied) return Response.json({ error: { code: "permission_required", message: "permission required" } }, { status: 403 })
    if (path.endsWith("/exec")) return Response.json({ command_id: id })
    if (path.endsWith("/cancel")) return Response.json({ id: computerId, target_id: id, status: "accepted" })
    if (path.endsWith("/logs")) return Response.json({ logs: [], output_state: "closed" })
    return Response.json({ id, computer_id: computerId, status: "exited", process_reconciled: true,
      outcome: { command_id: id, kind: "exited", exit_code: 0, terminal_at: "2026-10-08T00:00:00Z" } })
  }) as typeof fetch })
  try {
    const ref = await client.computers.ref(computerId).exec({ command: ["true"], idempotencyKey: "command" }, { signal })
    expect(ref.id).toBe(id)
    expect((await ref.retrieve({ signal })).status).toBe("exited")
    expect(await ref.wait({ signal })).toMatchObject({ kind: "exited", exitCode: 0 })
    expect(await ref.logs({}, { signal })).toEqual({ items: [] })
    expect(await ref.streamLogs({}, { signal }).next()).toEqual({ done: true, value: undefined })
    expect(await client.commands.ref(id).cancel({ signal })).toEqual({ id: computerId, targetId: id, status: "accepted" })
    expect(calls.map(call => call.path)).toEqual([
      `/v1/computers/${computerId}/exec`, `/v1/commands/${id}`, `/v1/commands/${id}`,
      `/v1/commands/${id}/logs`, `/v1/commands/${id}/logs`, `/v1/commands/${id}/cancel`,
    ])
    for (const call of calls) {
      expect(call.init?.headers).toMatchObject({ Authorization: "Bearer explicit-key" })
      expect(call.init?.signal).toBeDefined()
    }
    expect(JSON.parse(String(calls[0]!.init?.body))).toEqual({ command: ["true"], idempotency_key: "command" })
    denied = true
    await expect(ref.retrieve()).rejects.toMatchObject({ code: "permission_required" })
  } finally {
    uninstall()
  }
})
