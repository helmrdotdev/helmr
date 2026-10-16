import { expect, mock, test } from "bun:test"
import type { HumanContent, Turn } from "@helmr/sdk"

const trace: string[] = []
const prompts: HumanContent[] = []
const connections: unknown[] = []
const connection = { url: "http://127.0.0.1:12345/mcp", headers: { Authorization: "Bearer fixture" } }
let checksFail = false
const open = async (cwd: string, sessionId: string, _env: unknown, mcp: unknown) => {
  expect(cwd).toBe("/fixture/repository")
  expect(sessionId).toBe("fixture-session")
  connections.push(mcp)
  trace.push("setup")
  return { run: async (_turn: unknown, prompt: HumanContent) => { prompts.push(prompt); trace.push("native"); return { native: "completed" } } }
}
mock.module("../agents/issue-fixer/codex-bridge", () => ({ CodexBridge: { open } }))
mock.module("../agents/issue-fixer/claude-bridge", () => ({ ClaudeBridge: { open } }))
const { issuePrompt } = await import("../agents/issue-fixer/checks")
mock.module("../agents/issue-fixer/checks", () => ({
  issuePrompt,
  repository: () => "/fixture/repository",
  checkRepository: async (_cwd: string, signal: AbortSignal) => {
    signal.throwIfAborted(); trace.push("checks")
    if (checksFail) throw new Error("Repository checks failed")
  },
}))
const { codexIssueFixer } = await import("../agents/issue-fixer/codex")
const { claudeIssueFixer } = await import("../agents/issue-fixer/claude")

for (const definition of [codexIssueFixer, claudeIssueFixer]) {
  test(`${definition.id} reuses setup, accepts Content and settles only after checks`, async () => {
    trace.length = 0; prompts.length = 0; connections.length = 0; checksFail = false
    const setup = { mcp: { getConnectionOptions: async () => connection }, session: { id: "fixture-session" }, computer: { id: "fixture-computer" }, deployment: { id: "fixture-deployment" }, recovery: { kind: "initial" as const }, signal: new AbortController().signal }
    const initResult = await definition.init!(setup)
    expect(connections).toEqual([definition === codexIssueFixer ? connection : { helmr: { type: "http", url: connection.url, headers: connection.headers } }])
    const context = { ...setup, initResult }
    const turn = { id: "turn", input: [{type:"text",text:"repair"}], signal: new AbortController().signal } as Turn
    expect(await definition.turn(turn, context)).toEqual({ native: "completed" })
    expect(await definition.turn({ ...turn, input: [{ type: "text", text: "followup" }] }, context)).toEqual({ native: "completed" })
    expect(trace).toEqual(["setup", "native", "checks", "native", "checks"])
    expect(prompts[1]).toEqual([{ type: "text", text: "Fix this issue and explain the change: " }, { type: "text", text: "followup" }])
    checksFail = true
    await expect(definition.turn(turn, context)).rejects.toThrow("Repository checks failed")
    const calls = trace.length
    await expect(definition.turn({ ...turn, input: { unexpected: true } as never }, context)).rejects.toThrow()
    expect(trace.length).toBe(calls)
  })
}
