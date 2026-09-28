import { describe, expect, test } from "bun:test"
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
