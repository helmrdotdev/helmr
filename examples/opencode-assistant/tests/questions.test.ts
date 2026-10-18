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
