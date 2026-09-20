// Run separately: real production Actor + pinned SDK/native child processes.
// Model, Helmr delivery and repository checks are fixtures; this is not VM proof.
import { claudeModel } from "./native-model"
import { mkdtemp, rm, stat } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { expect, mock, test } from "bun:test"
import { z } from "zod"
import { inspectDefinition } from "../../../sdk/typescript/src/internal"
import { conversation } from "../tasks/issue-fixer/conversation"

let directory = ""
let checks = 0
mock.module("../tasks/issue-fixer/checks", () => ({
  issueSchema: z.object({ issue: z.string().min(1) }),
  repository: () => directory,
  checkRepository: async (_cwd: string, signal: AbortSignal) => { signal.throwIfAborted(); checks++ },
}))

for (const interrupted of [false, true]) test(`Claude Actor ${interrupted ? "interruption" : "interaction"} and fresh-process history`, async () => {
  directory = await mkdtemp(join(tmpdir(), "helmr-claude-actor-"))
  checks = 0
  const originalEnv = { ...process.env }
  const requests: any[] = []
  const http = claudeModel(requests, interrupted)
  const cancel = new AbortController()
  const timer = setTimeout(() => cancel.abort(new Error("Native probe timed out")), 60000)
  try {
    await new Promise<void>(resolve => http.listen(0, "127.0.0.1", resolve))
    const port = (http.address() as { port: number }).port
    // The production Actor inherits its host environment. Give this isolated
    // probe only fixture auth and a temporary home; never forward operator keys.
    for (const key of Object.keys(process.env)) delete process.env[key]
    Object.assign(process.env, {
      PATH: originalEnv.PATH, HOME: directory,
      ANTHROPIC_BASE_URL: `http://127.0.0.1:${port}`, ANTHROPIC_API_KEY: "local-fixture",
      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1", DISABLE_TELEMETRY: "1",
    })
    const { claudeIssueFixer } = await import("../tasks/issue-fixer/claude")
    let questions = 0
    let approvals = 0
    let completed = 0
    let nativeId: string | undefined
    let staleReply: any
    const results: { issue: string; nativeId: string }[] = []
    for (const issue of ["first unique input", "second unique input"]) {
      let received = false
      let handler: (value: any) => Promise<void> = async () => { throw new Error("Message handler not registered") }
      const stop = new AbortController()
      const run = inspectDefinition(claudeIssueFixer)!.handler({
        id: "same-session",
        receive: async () => {
          if (received) return null
          received = true
          return {
            id: issue, input: { issue }, signal: AbortSignal.any([cancel.signal, stop.signal]),
            onMessage: async (value: typeof handler) => {
              handler = value
              if (staleReply) await expect(handler(staleReply)).rejects.toThrow()
            },
            output: { write: async (event: any) => {
              if (event.type === "provider_event" && event.event.type === "result") {
                if (!stop.signal.aborted) expect(event.event.subtype).toBe("success")
                results.push({ issue, nativeId: event.event.session_id })
              }
              if (event.type !== "human_requested") return
              expect(event.action.provider).toBe("claude")
              if (event.kind === "answer") {
                questions++
                expect(event.action.toolName).toBe("AskUserQuestion")
                staleReply = { data: { type: "answer", requestId: event.requestId, answers: { "Which option?": ["A"] } } }
                if (interrupted) { stop.abort(new Error("Fixture interruption")); return }
                // These are queued while the human question is still pending.
                await handler({ data: { type: "update_constraints", text: "first followup constraint" } })
                await handler({ data: { issue: "second followup constraint" } })
                await handler(staleReply)
                await expect(handler(staleReply)).rejects.toThrow()
              } else {
                approvals++
                expect(event.action.toolName).toBe("Bash")
                await handler({ data: { type: "approval", requestId: event.requestId, allow: false } })
              }
            } },
            complete: async () => { completed++; expect(checks).toBe(completed) },
            fail: async (error: unknown) => { throw error },
          }
        },
      })
      if (interrupted && issue === "first unique input") {
        await run.then(() => { throw new Error("Expected interruption") }, error => expect(error).toBe(stop.signal.reason))
        await expect(handler(staleReply)).rejects.toThrow()
      } else await run
      const current = await conversation(directory, "same-session", "claude")
      expect(current.id).toBeDefined()
      if (nativeId) expect(current.id).toBe(nativeId)
      nativeId = current.id
    }
    expect(questions).toBe(1)
    expect(approvals).toBe(interrupted ? 0 : 1)
    expect(completed).toBe(interrupted ? 1 : 2)
    expect(checks).toBe(completed)
    // An interrupted native request may still produce a result. Only Helmr
    // settlement and the abort-aware repository check decide completion.
    expect(results.filter(result => result.issue === "second unique input")).toHaveLength(1)
    if (!interrupted) expect(results).toHaveLength(4)
    for (const result of results) expect(result.nativeId).toBe(nativeId!)
    await expect(stat(join(directory, "denied-command-marker"))).rejects.toMatchObject({ code: "ENOENT" })
    const resumed = JSON.stringify(requests.at(-1).messages)
    for (const text of ["first unique input", "second unique input"]) expect(resumed).toContain(text)
    if (!interrupted) {
      const blocks = requests.at(-1).messages.flatMap((message: any) => Array.isArray(message.content) ? message.content : [])
      const answer = blocks.find((block: any) => block.type === "tool_result" && block.tool_use_id === "toolu_question")
      expect(answer.content).toContain('"Which option?"="A"')
      for (const text of ["native fixture response", "User denied this operation", "first followup constraint", "second followup constraint"]) expect(resumed).toContain(text)
      expect(resumed.indexOf("first followup constraint")).toBeLessThan(resumed.indexOf("second followup constraint"))
    }
  } finally {
    clearTimeout(timer); cancel.abort()
    http.closeAllConnections(); await new Promise<void>(resolve => http.close(() => resolve()))
    for (const key of Object.keys(process.env)) delete process.env[key]
    Object.assign(process.env, originalEnv)
    await rm(directory, { recursive: true, force: true })
  }
}, 75000)
