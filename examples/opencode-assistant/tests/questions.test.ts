import assert from "node:assert/strict"
import { test } from "node:test"
import type { Turn } from "@helmr/sdk"
import { askPermission } from "../agents/questions"

test("the website example never offers or accepts a persistent native permission grant", async () => {
  const request = { id: "per_example", sessionID: "ses_example", action: "shell", resources: ["node analysis.mjs"], save: ["node *"] } as unknown as Parameters<typeof askPermission>[1]
  for (const value of ["once", "always", "reject"]) {
    const turn = { ask: async (question: any) => {
      assert.deepEqual(question.options.map((option: any) => option.id), ["once", "reject"])
      return { answer: { selected: [{ id: value, value }] } }
    } } as unknown as Turn
    assert.equal(await askPermission(turn, request, new AbortController().signal), value === "once" ? "once" : "reject")
  }
})


test("permission details remain complete and oversized previews are rejected", async () => {
  const request = { id: "per_edit", sessionID: "ses_example", action: "edit", resources: ["index.html"], save: ["*"], metadata: { files: [{ file: "index.html", patch: "+<h1>Event</h1>" }] } } as unknown as Parameters<typeof askPermission>[1]
  let asks = 0
  const turn = { ask: async (question: any) => {
    asks++
    assert.match(JSON.stringify(question.prompt), /Event/)
    return { answer: { selected: [{ id: "once", value: "once" }] } }
  } } as unknown as Turn
  assert.equal(await askPermission(turn, request, new AbortController().signal), "once")
  assert.equal(await askPermission(turn, { ...request, metadata: { patch: "x".repeat(48 * 1024) } }, new AbortController().signal), "reject")
  assert.equal(asks, 1, "oversized details never offer approval")
})
