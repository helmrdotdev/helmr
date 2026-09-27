import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { z } from "zod"
import { expect, mock, test } from "bun:test"
import { spawn } from "node:child_process"
import { inspectDefinition } from "../../../sdk/typescript/src/internal"
import { CodexStdio } from "../tasks/issue-fixer/codex-stdio"
import { HumanRequests } from "../tasks/issue-fixer/human"

const trace: string[] = []
let fixtureDirectory = ""
const resumes: (string | undefined)[] = []
const nativeIds: string[] = []
// Boundary fixture: real delayed child exit, no model or provider authentication.
mock.module("@anthropic-ai/claude-agent-sdk", () => ({
  query({ prompt, options }: any) {
    resumes.push(options.resume)
    nativeIds.push(options.resume ?? options.sessionId)
    const child = options.spawnClaudeCodeProcess({
      command: process.execPath,
      args: ["-e", 'process.stdin.resume(); process.stdin.on("end",()=>setTimeout(()=>process.exit(0),80)); setInterval(()=>{},1000)'],
      env: process.env,
    })
    child.on("close", () => trace.push("native_exited"))
    return Object.assign((async function* () {
      yield { type: "system", subtype: "init", session_id: options.resume ?? options.sessionId }
      for await (const input of prompt) {
        trace.push(`input:${input.message.content}`)
        yield { type: "result", subtype: "success", is_error: false }
      }
    })(), { close() { trace.push("close_requested"); child.stdin.end() } })
  },
}))
mock.module("../tasks/issue-fixer/checks", () => ({
  issueSchema: z.object({ issue: z.string() }),
  repository: () => fixtureDirectory,
  checkRepository: async () => { trace.push("checks_started"); throw new Error("deterministic check failed") },
}))

test("Claude serializes rapid followups and waits for native exit before checks/failure", async () => {
  fixtureDirectory = await mkdtemp(join(tmpdir(), "helmr-native-continuity-"))
  const { claudeIssueFixer } = await import("../tasks/issue-fixer/claude")
  let handler: (message: any) => Promise<void>
  let first = true
  let received = false
  const turn = {
    id: "A", input: { issue: "fix" }, signal: new AbortController().signal,
    onMessage: async (value: typeof handler) => { handler = value },
    output: { write: async () => {
      if (first) {
        first = false
        await handler({ data: { type: "update_constraints", text: "first followup" } })
        await handler({ data: { type: "update_constraints", text: "second followup" } })
      }
    } },
    fail: async (error: Error) => { trace.push(`failed:${error.message}`) },
    complete: async () => { trace.push("completed") },
  }
  await inspectDefinition(claudeIssueFixer)!.handler({ id: "fixture-session", receive: async () => { if (received) return null; received = true; return turn } })
  expect(trace.filter(value => value.startsWith("input:"))).toHaveLength(3)
  expect(trace.indexOf("native_exited")).toBeGreaterThan(trace.indexOf("close_requested"))
  expect(trace.indexOf("checks_started")).toBeGreaterThan(trace.indexOf("native_exited"))
  expect(trace.at(-1)).toBe("failed:deterministic check failed")
  expect(trace).not.toContain("completed")
  await rm(fixtureDirectory, { recursive: true, force: true })
})

test("Codex wire cancellation invalidates a request while projection is blocked", async () => {
  const native = new AbortController()
  const cancelled = new Promise<void>(resolve => native.signal.addEventListener("abort", () => resolve(), { once: true }))
  const child = spawn(process.execPath, ["-e", 'process.stdin.on("data",()=>console.log(JSON.stringify({method:"serverRequest/resolved",params:{requestId:1,threadId:"thread"}})))'])
  const server = new CodexStdio(message => { if (message.method === "serverRequest/resolved") native.abort() }, child)
  const records: any[] = []
  const human = new HumanRequests(async value => { records.push(value) }, new AbortController().signal)
  const answer = human.ask("approval", { nativeId: 1 }, native.signal).catch(error => error)
  // Do not consume server.messages: output projection can be indefinitely blocked.
  try {
    await server.send({ trigger: "native cancellation" })
    await cancelled
    await expect(human.reply({ type: "approval", requestId: records[0].requestId, allow: true })).rejects.toThrow("stale")
    expect(await answer).toBeInstanceOf(Error)
    expect(records).toHaveLength(1)
  } finally { await server.close() }
})


test("a new Claude Actor Run resumes the Session conversation from Workspace state", async () => {
  fixtureDirectory = await mkdtemp(join(tmpdir(), "helmr-native-resume-"))
  const { claudeIssueFixer } = await import("../tasks/issue-fixer/claude")
  const before = resumes.length
  try {
    for (const id of ["turn-a", "turn-b"]) {
      let received = false
      // Separate handler invocations represent separate Run heaps; only files survive.
      await inspectDefinition(claudeIssueFixer)!.handler({
        id: "same-session",
        receive: async () => {
          if (received) return null
          received = true
          return {
            id, input: { issue: "continue the earlier work" }, signal: new AbortController().signal,
            onMessage: async () => {}, output: { write: async () => {} },
            fail: async () => {}, complete: async () => {},
          }
        },
      })
    }
    expect(resumes.slice(before)).toEqual([undefined, nativeIds[before]])
  } finally { await rm(fixtureDirectory, { recursive: true, force: true }) }
})
