import { expect, mock, test } from "bun:test"
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { tmpdir } from "node:os"
import type { Turn } from "../../../sdk/typescript/src/agent"
import { validateQuestion } from "../../../sdk/typescript/src/question"

let options: any
let results: any[] = []
let running: Promise<void> = Promise.resolve()
mock.module("@anthropic-ai/claude-agent-sdk", () => ({ query: (configuration: any) => {
  options = configuration.options
  return (async function* () {
    yield* results
    await running
    configuration.options.abortController.signal.throwIfAborted()
  })()
} }))
const { runClaude } = await import("../agents/claude-bridge")

const sessionId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33"
const turn = (ask: (question: any) => unknown = () => { throw new Error("unexpected question") }) => ({
  input: [{ type: "text", text: "hello" }], signal: new AbortController().signal,
  ask: async (question: any) => ({ answer: ask(question) }),
}) as unknown as Turn

async function withWorkspace(run: (cwd: string) => Promise<void>) {
  const cwd = await mkdtemp(join(tmpdir(), "helmr-claude-"))
  try { await run(cwd) } finally { await rm(cwd, { recursive: true, force: true }) }
}

test("the first Turn starts the Session's conversation with the model and introduction", () => withWorkspace(async cwd => {
  results = [{ type: "result", subtype: "success", is_error: false, result: "Hi, I am noether." }]
  expect(await runClaude(turn(), { cwd, sessionId, model: "claude-sonnet-5-5", appendSystemPrompt: "You are noether." })).toBe("Hi, I am noether.")
  expect(options.sessionId).toBe(sessionId)
  expect(options.resume).toBeUndefined()
  expect(options.model).toBe("claude-sonnet-5-5")
  expect(options.systemPrompt).toEqual({ type: "preset", preset: "claude_code", append: "You are noether." })
  expect(options.env.CLAUDE_CONFIG_DIR).toBe(join(cwd, ".helmr", "claude"))
  expect(options.env.CLAUDE_CODE_DISABLE_BACKGROUND_TASKS).toBe("1")
}))

test("later Turns resume the conversation saved on disk", () => withWorkspace(async cwd => {
  const project = join(cwd, ".helmr", "claude", "projects", "-workspace")
  await mkdir(project, { recursive: true })
  const transcript = join(project, `${sessionId}.jsonl`)
  results = [{ type: "result", subtype: "success", is_error: false, result: "Again." }]
  await writeFile(transcript, '{"type":"queue-operation"}\n')
  await runClaude(turn(), { cwd, sessionId, model: "m", appendSystemPrompt: "" })
  expect(options.sessionId).toBe(sessionId)
  await writeFile(transcript, '{"type":"queue-operation"}\n{"type":"user","message":{}}\n')
  await runClaude(turn(), { cwd, sessionId, model: "m", appendSystemPrompt: "" })
  expect(options.resume).toBe(sessionId)
  expect(options.sessionId).toBeUndefined()
}))

test("a failed Claude Code result fails the Turn", () => withWorkspace(async cwd => {
  results = [{ type: "result", subtype: "error_during_execution", is_error: true }]
  await expect(runClaude(turn(), { cwd, sessionId, model: "m", appendSystemPrompt: "" })).rejects.toThrow("could not finish")
}))

test("tool use waits for a person's approval", () => withWorkspace(async cwd => {
  results = []
  const asked: any[] = []
  await runClaude(turn(question => { asked.push(question); return { selected: [{ id: "allow", value: true }] } }), { cwd, sessionId, model: "m", appendSystemPrompt: "" })
  const signal = new AbortController().signal
  expect(await options.canUseTool("Bash", { command: "ls" }, { signal, title: "Claude wants to run ls" })).toEqual({ behavior: "allow", updatedInput: { command: "ls" } })
  expect(asked[0].prompt[0].text).toBe("Claude wants to run ls")
  validateQuestion(asked[0])
}))

test("cancelling a running Turn aborts Claude Code and fails the Turn", () => withWorkspace(async cwd => {
  results = []
  let finish!: () => void
  running = new Promise(resolve => { finish = resolve })
  const controller = new AbortController()
  const pending = runClaude({ ...turn(), signal: controller.signal } as unknown as Turn, { cwd, sessionId, model: "m", appendSystemPrompt: "" })
  await new Promise(resolve => setImmediate(resolve))
  controller.abort(new Error("cancelled"))
  expect(options.abortController.signal.aborted).toBe(true)
  finish()
  running = Promise.resolve()
  await expect(pending).rejects.toThrow("cancelled")
}))

test("a Turn cancelled before Claude Code starts does not start it", () => withWorkspace(async cwd => {
  options = undefined
  const controller = new AbortController()
  controller.abort(new Error("cancelled"))
  await expect(runClaude({ ...turn(), signal: controller.signal } as unknown as Turn, { cwd, sessionId, model: "m", appendSystemPrompt: "" })).rejects.toThrow("cancelled")
  expect(options).toBeUndefined()
}))
