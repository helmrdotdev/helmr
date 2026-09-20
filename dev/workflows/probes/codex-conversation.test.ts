// Run separately: uses the real pinned app-server with a loopback model fixture.
// The Helmr handler boundary and repository check are fixtures, not a deployed VM.
import { codexModel } from "./native-model"
import { mkdtemp, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { expect, mock, test } from "bun:test"
import { z } from "zod"
import { inspectDefinition } from "../../../sdk/typescript/src/internal"
import { conversation } from "../tasks/issue-fixer/conversation"

let directory = ""
mock.module("../tasks/issue-fixer/checks", () => ({
  issueSchema: z.object({ issue: z.string().min(1) }),
  repository: () => directory,
  checkRepository: async () => {},
}))

for (const scenario of ["answer", "interrupt", "approval"]) test(`native ${scenario} and history survive fresh app-server processes`, async () => {
  const interrupt = scenario === "interrupt"
  const approval = scenario === "approval"
  directory = await mkdtemp(join(tmpdir(), "helmr-codex-conversation-"))
  const requests: any[] = []
  const http = codexModel(requests, approval)
  const cancel = new AbortController()
  const timeout = setTimeout(() => cancel.abort(new Error("Native probe timed out")), 25000)
  try {
    await new Promise<void>(resolve => http.listen(0, "127.0.0.1", resolve))
    const port = (http.address() as { port: number }).port
    const saved = await conversation(directory, "same-session", "codex")
    await writeFile(join(saved.directory, "config.toml"), `model_provider = "fixture"\nmodel = "gpt-5.1-codex"\n[model_providers.fixture]\nname = "local fixture"\nbase_url = "http://127.0.0.1:${port}/v1"\nwire_api = "responses"\nrequires_openai_auth = false\n`)
    const { codexIssueFixer } = await import("../tasks/issue-fixer/codex")
    let questions = 0
    let completed = 0
    let nativeId: string | undefined
    for (const issue of ["first unique input", "second unique input"]) {
      let received = false
      let handler: (value: any) => Promise<void> = async () => { throw new Error("Message handler not registered") }
      const turnCancel = new AbortController()
      let pendingReply: any
      const run = inspectDefinition(codexIssueFixer)!.handler({
        id: "same-session",
        receive: async () => {
          if (received) return null
          received = true
          return {
            id: issue, input: { issue }, signal: AbortSignal.any([cancel.signal, turnCancel.signal]),
            onMessage: async (value: typeof handler) => { handler = value },
            output: { write: async (event: any) => {
              if (event.type === "human_requested") {
                questions++
                expect(event.kind).toBe(approval ? "approval" : "answer")
                pendingReply = { data: approval ? { type: "approval", requestId: event.requestId, allow: false } : { type: "answer", requestId: event.requestId, answers: { choice: ["A"] } } }
                if (interrupt) turnCancel.abort(new Error("Fixture interruption"))
                else await handler(pendingReply)
              }
            } },
            complete: async () => { completed++ },
            fail: async (error: unknown) => { throw error },
          }
        },
      })
      if (interrupt && issue === "first unique input") {
        await expect(run).rejects.toThrow()
        await expect(handler(pendingReply)).rejects.toThrow()
      } else await run
      const current = await conversation(directory, "same-session", "codex")
      expect(current.id).toBeDefined()
      if (nativeId) expect(current.id).toBe(nativeId)
      nativeId = current.id
    }
    expect(questions).toBe(1)
    expect(completed).toBe(interrupt ? 1 : 2)
    expect(requests).toHaveLength(interrupt ? 2 : 3)
    if (!interrupt) {
      const response = requests[1].input.find((item: any) => item.type === "function_call_output")
      if (approval) expect(response.output).toMatch(/reject|declin/i)
      else {
        const expected = { answers: { choice: { answers: ["A"] } } }
        expect(JSON.parse(response.output)).toEqual(expected)
        const restored = requests.at(-1).input.find((item: any) => item.type === "function_call_output" && item.call_id === "call_question")
        expect(JSON.parse(restored.output)).toEqual(expected)
      }
    }
    const resumed = JSON.stringify(requests.at(-1).input)
    for (const text of ["first unique input", "second unique input"]) expect(resumed).toContain(text)
    if (!interrupt) for (const text of ["fixture response", approval ? "call_command" : "call_question"]) expect(resumed).toContain(text)
  } finally {
    clearTimeout(timeout)
    cancel.abort()
    http.closeAllConnections()
    await new Promise<void>(resolve => http.close(() => resolve()))
    await rm(directory, { recursive: true, force: true })
  }
}, 30000)
