import { test, expect } from "bun:test"
import type { HelmrClient } from "@helmr/sdk"
import { cleanupChildren } from "./cleanup"

test("failed parent cleanup discovers paginated children without touching other owners", async () => {
  const retrieved: string[] = [], cancelled: string[] = [], cursors: unknown[] = []
  let discoverySignal: AbortSignal | undefined
  const client = { runs: {
    list: async ({ cursor }: { cursor?: string }, { signal }: { signal: AbortSignal }) => {
      discoverySignal = signal
      signal.throwIfAborted()
      cursors.push(cursor)
      return cursor === undefined
        ? { items: [{ id: "child", computerId: "owned" }, { id: "other-computer", computerId: "unrelated" }, { id: "other-parent", computerId: "owned" }], nextCursor: "second" }
        : { items: [{ id: "finished", computerId: "owned" }] }
    },
    retrieve: async (id: string) => {
      retrieved.push(id)
      return { id, parentRunId: id === "other-parent" ? "other" : "parent", status: id === "child" && !cancelled.includes(id) ? "running" : "succeeded" }
    },
    cancel: async (id: string) => {
      expect(cursors).toEqual([undefined, "second"])
      expect(retrieved).toContain("finished")
      expect(discoverySignal).toBeDefined()
      cancelled.push(id)
    },
  } } as unknown as HelmrClient
  const recorded = ["parent", "finished"]
  await cleanupChildren(client, "parent", ["owned"], recorded)
  expect(cursors).toEqual([undefined, "second"])
  expect(cancelled).toEqual(["child"])
  expect(retrieved).not.toContain("other-computer")
  expect(recorded).toEqual(["parent", "finished", "child"])
})


test("child finishing between discovery and cancellation is proved terminal", async () => {
  let reads = 0
  const client = { runs: {
    list: async () => ({ items: [{ id: "child", computerId: "owned" }] }),
    retrieve: async () => ({ id: "child", parentRunId: "parent", status: ++reads === 1 ? "running" : "succeeded" }),
    cancel: async () => { throw Object.assign(new Error("already terminal"), { code: "run_lifecycle_conflict" }) },
  } } as unknown as HelmrClient
  const recorded: string[] = []
  await cleanupChildren(client, "parent", ["owned"], recorded)
  expect(recorded).toEqual(["child"])
  expect(reads).toBe(2)
})
