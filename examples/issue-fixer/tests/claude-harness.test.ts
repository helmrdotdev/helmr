// Run separately from real-native probes: the SDK event stream is controlled here.
import { installNativeTestHooks } from "./native-runtime-fixture"
const nativeFixture = installNativeTestHooks()
import { expect, mock, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { join } from "node:path"
import { tmpdir } from "node:os"
import { Queue } from "../tasks/issue-fixer/queue"
import type { Turn } from "../../../sdk/typescript/src/agent"

let events: Queue<any>, prompts: AsyncIterator<any>
let initializationPending = false
let permission: any
let nativeSessionId: string
mock.module("@anthropic-ai/claude-agent-sdk", () => ({ query: (configuration: any) => {
  nativeSessionId = configuration.options.sessionId ?? configuration.options.resume
  permission = configuration.options.canUseTool
  events = new Queue()
  prompts = configuration.prompt[Symbol.asyncIterator]()
  const child = configuration.options.spawnClaudeCodeProcess({ command: "fixture", args: [], env: {}, signal: new AbortController().signal })
  return Object.assign(events, { close: () => { child.kill(); events.end() }, interrupt: async () => {}, stopTask: async () => {}, initializationResult: async () => initializationPending ? new Promise(() => {}) : {} })
} }))
const { ClaudeHarness } = await import("../tasks/issue-fixer/claude-harness")
const flush = () => new Promise<void>(resolve => setImmediate(resolve))

test("held-back results and old task events cannot complete or enter a later Turn", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-order-"))
  const harness = await ClaudeHarness.open(directory, "session", {})
  const output: Record<string, any[]> = { first: [], second: [] }
  const turn = (id: string) => ({ id, signal: new AbortController().signal,
    onMessage: async () => {}, output: { write: async (value: any) => { output[id]!.push(value); return { sequence: 1 } } },
  }) as unknown as Turn
  try {
    let completed = false
    const first = harness.run(turn("first"), "first").then(value => { completed = true; return value })
    await prompts.next()
    events.push({ type: "result", session_id: nativeSessionId, subtype: "success", is_error: false })
    await flush()
    expect(completed).toBe(false)
    await expect(harness.run(turn("second"), "second")).rejects.toThrow("not ready")
    events.push({ type: "system", subtype: "task_started", task_id: "old-task" })
    events.push({ type: "system", subtype: "task_notification", task_id: "old-task" })
    events.push({ type: "result", session_id: nativeSessionId, subtype: "success", is_error: false })
    await flush()
    expect(completed).toBe(false)
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    await first
    const second = harness.run(turn("second"), "second")
    await prompts.next()
    expect(output.second).toHaveLength(0)
    events.push({ type: "result", session_id: nativeSessionId, subtype: "success", is_error: false })
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    await second
    expect(output.first).toHaveLength(0)
    expect(output.second).toHaveLength(0)
  } finally { await harness.close(); await rm(directory, { recursive: true, force: true }) }
})


test("registration-time steering stays behind the admitted input", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-registration-"))
  const harness = await ClaudeHarness.open(directory, "session", {})
  const turn = { id: "first", signal: new AbortController().signal,
    onMessage: async (handler: (value: any) => Promise<void>) => { await handler([{ type:"text", text:"followup" }]) },
    output: { write: async () => ({ sequence: 1 }) },
  } as unknown as Turn
  try {
    const pending = harness.run(turn, "original")
    expect((await prompts.next()).value.message.content).toBe("original")
    events.push({ type: "result", session_id: nativeSessionId, subtype: "success", is_error: false })
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    expect((await prompts.next()).value.message.content).toBe("followup")
    events.push({ type: "result", session_id: nativeSessionId, subtype: "success", is_error: false })
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    await pending
  } finally { await harness.close(); await rm(directory, { recursive: true, force: true }) }
})


test("forgotten failed claude open is observed by setup without an unhandled rejection", async () => {
  void ClaudeHarness.open("/dev/null/invalid-native-directory", "session", {})
  await expect(nativeFixture.initializations[0]!).rejects.toThrow()
  await new Promise<void>(resolve => setImmediate(resolve))
})


test("launch failure rejects setup even while native initialization is pending", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-launch-failure-"))
  nativeFixture.failLaunch(new Error("native identity rejected"))
  initializationPending = true
  try {
    await expect(ClaudeHarness.open(directory, "session", {})).rejects.toThrow("native identity rejected")
    await expect(nativeFixture.initializations[0]!).rejects.toThrow("native identity rejected")
    await flush()
  } finally { initializationPending = false; await rm(directory, { recursive: true, force: true }) }
})

test("a caught native result failure permits an ordinary retry in the same Turn", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-retry-"))
  const harness = await ClaudeHarness.open(directory, "session", {})
  let registrations = 0
  const turn = { id: "turn", signal: new AbortController().signal,
    onMessage: async () => { if (++registrations > 1) throw new Error("duplicate message handler") },
    output: { write: async () => ({ sequence: 1 }) },
  } as unknown as Turn
  try {
    const first = harness.run(turn, "first")
    await prompts.next()
    events.push({ type: "result", session_id: nativeSessionId, subtype: "error_during_execution", is_error: true })
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    await expect(first).rejects.toThrow("Native Turn failed")
    await nativeFixture.operations[0]
    expect(harness.reusable).toBe(true)
    const second = harness.run(turn, "retry")
    await prompts.next()
    events.push({ type: "result", session_id: nativeSessionId, subtype: "success", is_error: false })
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    await second
    await nativeFixture.operations[1]
    expect(registrations).toBe(1)
  } finally { await harness.close(); await rm(directory, { recursive: true, force: true }) }
})

test("final response failure preserves reusable idle before returning the error", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-output-failure-"))
  const harness = await ClaudeHarness.open(directory, "session", {})
  const turn = { id: "turn", signal: new AbortController().signal,
    onMessage: async () => {}, respond: async () => { throw new Error("response persistence failed") }, output: { write: async () => ({ sequence: 1 }) },
  } as unknown as Turn
  try {
    const running = harness.run(turn, "first")
    await prompts.next()
    events.push({ type: "assistant", message: { content: "possible final text" } })
    await flush()
    events.push({ type: "result", session_id: nativeSessionId, subtype: "success", is_error: false, result: "complete" })
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    await expect(running).rejects.toThrow("response persistence failed")
    await nativeFixture.operations[0]
    expect(harness.reusable).toBe(true)
  } finally { await harness.close(); await rm(directory, { recursive: true, force: true }) }
})


test("registration failure before submission preserves the setup resource", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-registration-failure-"))
  const harness = await ClaudeHarness.open(directory, "session", {})
  const turn = { id: "turn", signal: new AbortController().signal,
    onMessage: () => { throw new Error("registration failed") },
    output: { write: async () => ({ sequence: 1 }) },
  } as unknown as Turn
  try {
    await expect(harness.run(turn, "never submitted")).rejects.toThrow("registration failed")
    expect(harness.idle).toBe(true)
    expect(harness.reusable).toBe(true)
  } finally { await harness.close(); await rm(directory, { recursive: true, force: true }) }
})

test("terminal idle withdraws owned question callbacks before returning", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-question-drain-"))
  const harness = await ClaudeHarness.open(directory, "session", {})
  let asks = 0, withdrawals = 0
  const responses: unknown[] = [], output: unknown[] = []
  const turn = { id: "turn", signal: new AbortController().signal, onMessage: async () => {},
    output: { write: async (value: unknown) => { output.push(value); return { sequence: 1 } } }, respond: async (value: unknown) => { responses.push(value) },
    ask: (_: unknown, { signal }: { signal: AbortSignal }) => { asks++; return new Promise((_, reject) => signal.addEventListener("abort", () => { withdrawals++; reject(signal.reason) })) },
  } as unknown as Turn
  try {
    const pending = harness.run(turn, "inspect")
    await prompts.next()
    const reply = permission("AskUserQuestion", { questions: [{ header: "Pick", question: "Which?", multiSelect: false, options: [{ label: "A", description: "First" }, { label: "B", description: "Second" }] }] }, { toolUseID: "request", signal: new AbortController().signal })
    await flush(); expect(asks).toBe(1)
    events.push({ type: "assistant", message: { content: "possible final" } })
    events.push({ type: "result", session_id: nativeSessionId, subtype: "success", is_error: false, result: "final" })
    await flush(); expect(responses).toEqual([])
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    await pending
    expect((await reply).behavior).toBe("deny")
    expect(withdrawals).toBe(1); expect(output).toEqual([]); expect(responses).toEqual(["final"])
  } finally { await harness.close(); await rm(directory, { recursive: true, force: true }) }
})
