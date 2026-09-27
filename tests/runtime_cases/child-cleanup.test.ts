import { test, expect } from "bun:test"
import type { HelmrClient } from "@helmr/sdk"
import { cleanupChildren } from "../e2e/cases/child-tasks/cleanup"

test("failed parent cleanup discovers paginated children without touching other owners", async () => {
  const retrieved: string[] = [], cancelled: string[] = [], cursors: unknown[] = []
  const client = { runs: {
    list: async ({ cursor }: { cursor?: string }) => {
      cursors.push(cursor)
      return cursor === undefined
        ? { items: [{ id: "other-workspace", workspaceId: "unrelated" }, { id: "other-parent", workspaceId: "owned" }], nextCursor: "second" }
        : { items: [{ id: "child", workspaceId: "owned" }, { id: "finished", workspaceId: "owned" }] }
    },
    retrieve: async (id: string) => {
      retrieved.push(id)
      return { id, parentRunId: id === "other-parent" ? "other" : "parent", status: id === "child" && !cancelled.includes(id) ? "running" : "succeeded" }
    },
    cancel: async (id: string) => { cancelled.push(id) },
  } } as unknown as HelmrClient
  const recorded = ["parent", "finished"]
  await cleanupChildren(client, "parent", ["owned"], recorded)
  expect(cursors).toEqual([undefined, "second"])
  expect(cancelled).toEqual(["child"])
  expect(retrieved).not.toContain("other-workspace")
  expect(recorded).toEqual(["parent", "finished", "child"])
})
