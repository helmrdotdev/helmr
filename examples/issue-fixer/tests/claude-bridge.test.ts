// Run separately from real-native probes: the SDK event stream is controlled here.
import { installProcessFixture } from "./process-fixture"
const nativeFixture = installProcessFixture()
import { expect, mock, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { join } from "node:path"
import { tmpdir } from "node:os"
import { Queue } from "../agents/issue-fixer/queue"
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
const { ClaudeBridge } = await import("../agents/issue-fixer/claude-bridge")
const flush = () => new Promise<void>(resolve => setImmediate(resolve))

test("held-back results and old task events cannot complete or enter a later Turn", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-order-"))
  const bridge = await ClaudeBridge.open(directory, "session", {})
  const output: Record<string, any[]> = { first: [], second: [] }
  const turn = (id: string) => ({ id, signal: new AbortController().signal,
    onCancel: () => {}, onMessage: async () => {}, output: { write: async (value: any) => { output[id]!.push(value); return { sequence: 1 } } },
  }) as unknown as Turn
  try {
    let completed = false
    const first = bridge.run(turn("first"), "first").then(value => { completed = true; return value })
    await prompts.next()
    events.push({ type: "result", session_id: nativeSessionId, subtype: "success", is_error: false })
    await flush()
    expect(completed).toBe(false)
    await expect(bridge.run(turn("second"), "second")).rejects.toThrow("not ready")
    events.push({ type: "system", subtype: "task_started", task_id: "old-task" })
    events.push({ type: "system", subtype: "task_notification", task_id: "old-task" })
    events.push({ type: "result", session_id: nativeSessionId, subtype: "success", is_error: false })
    await flush()
    expect(completed).toBe(false)
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    await first
    const second = bridge.run(turn("second"), "second")
    await prompts.next()
    expect(output.second).toHaveLength(0)
    events.push({ type: "result", session_id: nativeSessionId, subtype: "success", is_error: false })
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    await second
    expect(output.first).toHaveLength(0)
    expect(output.second).toHaveLength(0)
  } finally { await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})


test("registration-time steering stays behind the admitted input", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-registration-"))
  const bridge = await ClaudeBridge.open(directory, "session", {})
  const turn = { id: "first", signal: new AbortController().signal,
    onCancel: () => {}, onMessage: async (handler: (value: any) => Promise<void>) => { await handler({ id: "message", content: [{ type:"text", text:"followup" }], source: { kind: "user" } }) },
    output: { write: async () => ({ sequence: 1 }) },
  } as unknown as Turn
  try {
    const pending = bridge.run(turn, "original")
    expect((await prompts.next()).value.message.content).toBe("original")
    events.push({ type: "result", session_id: nativeSessionId, subtype: "success", is_error: false })
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    expect((await prompts.next()).value.message.content).toBe("followup")
    events.push({ type: "result", session_id: nativeSessionId, subtype: "success", is_error: false })
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    await pending
  } finally { await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})


test("failed Claude initialization rejects the awaited open", async () => {
  await expect(ClaudeBridge.open("/dev/null/invalid-native-directory", "session", {})).rejects.toThrow()
  await new Promise<void>(resolve => setImmediate(resolve))
})


test("launch failure rejects setup even while native initialization is pending", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-launch-failure-"))
  nativeFixture.failLaunch(new Error("native identity rejected"))
  initializationPending = true
  try {
    await expect(ClaudeBridge.open(directory, "session", {})).rejects.toThrow("native identity rejected")
    await flush()
  } finally { initializationPending = false; await rm(directory, { recursive: true, force: true }) }
})

test("a caught native result failure permits an ordinary retry in the same Turn", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-retry-"))
  const bridge = await ClaudeBridge.open(directory, "session", {})
  let registrations = 0
  const turn = { id: "turn", signal: new AbortController().signal,
    onCancel: () => {}, onMessage: async () => { if (++registrations > 1) throw new Error("duplicate message handler") },
    output: { write: async () => ({ sequence: 1 }) },
  } as unknown as Turn
  try {
    const first = bridge.run(turn, "first")
    await prompts.next()
    events.push({ type: "result", session_id: nativeSessionId, subtype: "error_during_execution", is_error: true })
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    await expect(first).rejects.toThrow("Native Turn failed")
    expect(bridge.reusable).toBe(true)
    const second = bridge.run(turn, "retry")
    await prompts.next()
    events.push({ type: "result", session_id: nativeSessionId, subtype: "success", is_error: false })
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    await second
    expect(registrations).toBe(1)
  } finally { await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})

test("the bridge returns final content for application-owned publication", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-output-failure-"))
  const bridge = await ClaudeBridge.open(directory, "session", {})
  const turn = { id: "turn", signal: new AbortController().signal,
    onCancel: () => {}, onMessage: async () => {}, respond: async () => { throw new Error("response persistence failed") }, output: { write: async () => ({ sequence: 1 }) },
  } as unknown as Turn
  try {
    const running = bridge.run(turn, "first")
    await prompts.next()
    events.push({ type: "assistant", message: { content: "possible final text" } })
    await flush()
    events.push({ type: "result", session_id: nativeSessionId, subtype: "success", is_error: false, result: "complete" })
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    expect((await running).response).toBe("complete")
    expect(bridge.reusable).toBe(true)
  } finally { await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})


test("registration failure before submission preserves the setup resource", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-registration-failure-"))
  const bridge = await ClaudeBridge.open(directory, "session", {})
  const turn = { id: "turn", signal: new AbortController().signal,
    onCancel: () => {}, onMessage: () => { throw new Error("registration failed") },
    output: { write: async () => ({ sequence: 1 }) },
  } as unknown as Turn
  try {
    await expect(bridge.run(turn, "never submitted")).rejects.toThrow("registration failed")
    expect(bridge.idle).toBe(true)
    expect(bridge.reusable).toBe(true)
  } finally { await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})

test("terminal idle withdraws owned question callbacks before returning", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-question-drain-"))
  const bridge = await ClaudeBridge.open(directory, "session", {})
  let asks = 0, withdrawals = 0
  const responses: unknown[] = [], output: unknown[] = []
  const turn = { id: "turn", signal: new AbortController().signal, onCancel: () => {}, onMessage: async () => {},
    output: { write: async (value: unknown) => { output.push(value); return { sequence: 1 } } }, respond: async (value: unknown) => { responses.push(value) },
    ask: (_: unknown, { signal }: { signal: AbortSignal }) => { asks++; return new Promise((_, reject) => signal.addEventListener("abort", () => { withdrawals++; reject(signal.reason) })) },
  } as unknown as Turn
  try {
    const pending = bridge.run(turn, "inspect")
    await prompts.next()
    const reply = permission("AskUserQuestion", { questions: [{ header: "Pick", question: "Which?", multiSelect: false, options: [{ label: "A", description: "First" }, { label: "B", description: "Second" }] }] }, { toolUseID: "request", signal: new AbortController().signal })
    await flush(); expect(asks).toBe(1)
    events.push({ type: "assistant", message: { content: "possible final" } })
    events.push({ type: "result", session_id: nativeSessionId, subtype: "success", is_error: false, result: "final" })
    await flush(); expect(responses).toEqual([])
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    expect((await pending).response).toBe("final")
    expect((await reply).behavior).toBe("deny")
    expect(withdrawals).toBe(1); expect(output).toEqual([]); expect(responses).toEqual([])
  } finally { await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})

const assistant = (uuid: string, content: unknown[], extra = {}) => ({ type: "assistant", session_id: nativeSessionId, parent_tool_use_id: null, uuid, message: { id: "native-message", content }, ...extra })

test("completed blocks preserve order without duplicating deltas, snapshots or final result", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-claude-blocks-"))
  const bridge = await ClaudeBridge.open(directory, "session", {})
  const output: string[] = []
  const turn = { id: "turn", signal: new AbortController().signal, onCancel: () => {}, onMessage: async () => {}, output: { write: async (text: string) => { output.push(text); return { sequence: output.length } } } } as unknown as Turn
  try {
    const pending = bridge.run(turn, "inspect")
    await prompts.next()
    events.push({ type: "stream_event", event: { type: "content_block_delta", delta: { type: "text_delta", text: "duplicate" } } })
    const first = assistant("one", [{ type: "text", text: "First block" }, { type: "text", text: "Second block" }])
    events.push(first); events.push(first)
    events.push(assistant("child", [{ type: "text", text: "private child" }], { parent_tool_use_id: "child-tool" }))
    events.push(assistant("other", [{ type: "text", text: "other session" }], { session_id: "other" }))
    events.push(assistant("tool", [{ type: "tool_use", id: "tool", name: "Read", input: { private: "argument" } }]))
    events.push(assistant("final", [{ type: "text", text: "Held assistant candidate" }]))
    events.push({ type: "result", session_id: nativeSessionId, subtype: "success", is_error: false, result: "Selected result" })
    await flush()
    expect(output.join("")).toBe("First block\n\nSecond block")
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    expect((await pending).response).toBe("Selected result")
    expect(output.join("")).toBe("First block\n\nSecond block")
  } finally { await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})

test("owned follow-up publishes only the preceding selected result as intermediate", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-claude-followup-"))
  const bridge = await ClaudeBridge.open(directory, "session", {})
  const output: string[] = []
  let send!: (message: unknown) => Promise<void>
  const turn = { id: "turn", signal: new AbortController().signal, onCancel: () => {}, onMessage: async (handler: typeof send) => { send = handler }, output: { write: async (text: string) => { output.push(text); return { sequence: output.length } } } } as unknown as Turn
  try {
    const pending = bridge.run(turn, "first")
    await prompts.next()
    await send({ id: "message", content: [{ type: "text", text: "follow-up" }], source: { kind: "user" } })
    events.push(assistant("candidate", [{ type: "text", text: "Do not duplicate this candidate" }]))
    events.push({ type: "result", session_id: nativeSessionId, subtype: "success", is_error: false, result: "First result" })
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    expect((await prompts.next()).value.message.content).toBe("follow-up")
    events.push({ type: "result", session_id: nativeSessionId, subtype: "success", is_error: false, result: "Second result" })
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    expect((await pending).response).toBe("Second result")
    expect(output).toEqual(["First result"])
  } finally { await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})

test("an established native failure keeps verified explanation as output", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-claude-failure-text-"))
  const bridge = await ClaudeBridge.open(directory, "session", {})
  const output: string[] = []
  const turn = { id: "turn", signal: new AbortController().signal, onCancel: () => {}, onMessage: async () => {}, output: { write: async (text: string) => { output.push(text); return { sequence: output.length } } } } as unknown as Turn
  try {
    const pending = bridge.run(turn, "inspect")
    await prompts.next()
    events.push(assistant("explanation", [{ type: "text", text: "I could not finish the check." }]))
    events.push({ type: "result", session_id: nativeSessionId, subtype: "error_during_execution", is_error: true })
    events.push({ type: "system", subtype: "session_state_changed", state: "idle" })
    await expect(pending).rejects.toThrow("Native Turn failed")
    expect(output).toEqual(["I could not finish the check."])
  } finally { await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})
