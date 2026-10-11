import { expect, test } from "bun:test"
import { createClientComputerRef } from "./client-computer"
import { parseComputerMembers } from "./computer"

test("Computer members preserves its cursor and observation signal", async () => {
  const controller = new AbortController()
  const id = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"
  const requests: string[] = []
  const computer = createClientComputerRef(id, {
    async request(method, path, options) {
      expect(method).toBe("GET")
      expect(options?.signal).toBe(controller.signal)
      requests.push(path)
      return { members: [{ kind: "session", id, state: "admitted", created_at: "2026-09-26T00:00:00Z" }], next_cursor: "next" }
    },
  })
  const page = await computer.members({ cursor: "a+b", limit: 1 }, { signal: controller.signal })
  expect(requests).toEqual([`/v1/computers/${id}/members?cursor=a%2Bb&limit=1`])
  expect(page.items).toEqual([{ kind: "session", id, state: "admitted", createdAt: "2026-09-26T00:00:00Z" }])
  expect(page.nextCursor).toBe("next")
  expect(Object.isFrozen(page.items)).toBe(true)
  await expect(computer.members({ limit: 0 })).rejects.toThrow("limit")
  await expect(computer.members({ cursor: "" })).rejects.toThrow("cursor")
  expect(requests.length).toBe(1)
})

test("Computer members rejects malformed member identity and kinds", () => {
  expect(() => parseComputerMembers({ members: [{ kind: "vm", id: "invalid" }] })).toThrow("kind")
  expect(() => parseComputerMembers({ members: [{ kind: "session", id: "invalid", state: "running" }] })).toThrow("id")
  expect(parseComputerMembers({ members: [] })).toEqual({ items: [] })
})

 test("Computer member states do not expose a process generation", () => {
  const id = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"
  for (const state of ["admitted", "running", "waiting", "parked", "draining", "unreconciled"]) {
    const page = parseComputerMembers({ members: [{kind: "session", id, state, created_at: "2026-09-26T00:00:00Z"}] })
    expect(page.items[0]?.state).toBe(state)
    expect(Object.hasOwn(page.items[0]!, "runId")).toBe(false)
  }
  expect(() => parseComputerMembers({ members: [{kind: "session", id, state: "open"}] })).toThrow("state")
  expect(() => parseComputerMembers({ members: [{kind: "task", id, state: "running"}] })).toThrow("kind")
 })
