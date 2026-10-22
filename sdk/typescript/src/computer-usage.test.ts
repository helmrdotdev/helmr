import { expect, test } from "bun:test"
import { createClientComputerRef } from "./client-computer"
import { parseComputerUsage } from "./computer"
const id = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"
for (const collection of ["sessions", "commands"] as const) {
  test(`Computer ${collection} preserves collection, cursor and observation signal`, async () => {
    const signal = new AbortController().signal
    const requests: string[] = []
    const computer = createClientComputerRef(id, {
      async request(method, path, options) {
        expect(method).toBe("GET"); expect(options?.signal).toBe(signal); requests.push(path)
        return { [collection]: [{ id, usage_state: "unreconciled", created_at: "2026-09-26T00:00:00Z" }], next_cursor: "next" }
      },
    })
    const page = await computer[collection].list({ cursor: "a+b", limit: 1 }, { signal })
    expect(requests).toEqual([`/v1/computers/${id}/${collection}?cursor=a%2Bb&limit=1`])
    expect(page.items).toEqual([{ id, usageState: "unreconciled", createdAt: "2026-09-26T00:00:00Z" }])
    expect(page.nextCursor).toBe("next"); expect(Object.isFrozen(page.items)).toBe(true)
    expect("members" in computer).toBe(false)
    await expect(computer[collection].list({ limit: 0 })).rejects.toThrow("limit")
    await expect(computer[collection].list({ cursor: "" })).rejects.toThrow("cursor")
    expect(requests.length).toBe(1)
  })
}
test("Computer usage distinguishes occupancy from lifecycle state and rejects the wrong collection", () => {
  for (const state of ["admitted", "running", "waiting", "parked", "draining", "unreconciled"]) {
    const page = parseComputerUsage({ sessions: [{ id, usage_state: state, created_at: "2026-09-26T00:00:00Z" }] }, "sessions")
    expect(page.items[0]?.usageState).toBe(state)
  }
  expect(() => parseComputerUsage({ sessions: [{ id, usage_state: "closed" }] }, "sessions")).toThrow("state")
  expect(() => parseComputerUsage({ commands: [] }, "sessions")).toThrow("collection")
  expect(() => parseComputerUsage({ members: [] }, "sessions")).toThrow("collection")
})

for (const kind of ["hibernate", "wake"] as const) {
  test(`Computer ${kind} returns acceptance and preserves idempotency`, async () => {
    const signal = new AbortController().signal
    const computer = createClientComputerRef(id, {
      async request(method, path, options) {
        expect(method).toBe("POST")
        expect(path).toBe(`/v1/computers/${id}/${kind}`)
        expect(options?.signal).toBe(signal)
        expect(options?.body).toEqual({ idempotency_key: "same-operation" })
        return { id, computer_id: id, kind, status: "accepted", accepted_at: "2026-10-22T00:00:00Z", settled_at: null }
      },
    })
    const receipt = await computer[kind]({ idempotencyKey: "same-operation" }, { signal })
    expect(receipt.status).toBe("accepted")
    expect(receipt.settledAt).toBeNull()
    expect(receipt.computerId).toBe(id)
    expect(Object.isFrozen(receipt)).toBe(true)
  })
}
