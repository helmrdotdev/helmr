// Isolated transport failure test; real-native probes run in another process.
import { installNativeTestHooks } from "./native-runtime-fixture"
const nativeFixture = installNativeTestHooks()
import { expect, mock, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { join } from "node:path"
import { tmpdir } from "node:os"
import type { Turn } from "../../../sdk/typescript/src/agent"
import { MessageRejected } from "../../../sdk/typescript/src/message-error"
import { Queue } from "../tasks/issue-fixer/queue"

let messages: Queue<any>
let sent: any[] = []
let startRequestGate: Promise<void> | undefined
let onInterrupt: (() => void) | undefined
let finishExit!: () => void
class Rejected extends Error { constructor(readonly code: number, message: string) { super(message) } }
mock.module("../tasks/issue-fixer/codex-stdio", () => ({
  spawnManagedCodex: () => ({ child: {}, ready: Promise.resolve({ scopeId: "fixture", processId: 123 }) }), CodexRequestRejected: Rejected,
  CodexStdio: class {
    readonly messages = messages = new Queue()
    constructor() { sent = [] }
    readonly processId = 123
    readonly exit = new Promise<void>(resolve => { finishExit = resolve })
    async request(method: string) {
      if (method === "turn/steer") throw new Rejected(-32600, "no active turn to steer")
      if (method === "turn/start") { await startRequestGate; return { turn: { id: "native-turn", status: "inProgress" } } }
      if (method === "turn/interrupt") onInterrupt?.()
      return method === "thread/start" ? { thread: { id: "native" } } : {}
    }
    async send(value: any) { sent.push(value) }
    close() { return this.exit }
  },
}))
const { CodexHarness } = await import("../tasks/issue-fixer/codex-harness")

test("every close caller waits for physical exit after reader failure", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-close-"))
  const harness = await CodexHarness.open(directory, "session", {})
  try {
    messages.end(new Error("malformed native frame"))
    await new Promise<void>(resolve => setImmediate(resolve))
    let resolved = 0
    const first = harness.close(), second = harness.close()
    expect(first).toBe(second)
    void first.then(() => resolved++)
    void second.then(() => resolved++)
    await new Promise<void>(resolve => setImmediate(resolve))
    expect(resolved).toBe(0)
    finishExit()
    await Promise.all([first, second])
    expect(resolved).toBe(2)
  } finally { finishExit(); await harness.close(); await rm(directory, { recursive: true, force: true }) }
})


test("native steering rejection can precede the queued completion notification", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-steer-"))
  const harness = await CodexHarness.open(directory, "session", {})
  let handler!: (value: unknown) => Promise<void>, registered!: () => void
  const ready = new Promise<void>(resolve => { registered = resolve })
  const turn = { id: "turn", signal: new AbortController().signal,
    onMessage: async (callback: typeof handler) => { handler = callback; registered() },
    output: { write: async () => ({ sequence: 1 }) },
  } as unknown as Turn
  try {
    const pending = harness.run(turn, "original")
    await ready
    await expect(handler([{ type:"text", text:"too late" }])).rejects.toBeInstanceOf(MessageRejected)
    messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: "completed" } } })
    await expect(pending).resolves.toEqual({ nativeThreadId: "native", nativeTurnId: "native-turn" })
  } finally { finishExit(); messages.end(); await harness.close(); await rm(directory, { recursive: true, force: true }) }
})


test("forgotten failed codex open is observed by setup without an unhandled rejection", async () => {
  void CodexHarness.open("/dev/null/invalid-native-directory", "session", {})
  await expect(nativeFixture.initializations[0]!).rejects.toThrow()
  await new Promise<void>(resolve => setImmediate(resolve))
})


for (const failure of [false, true]) test(`registered native startup drains after handler ${failure ? "failure" : "return"}`, async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-pending-start-"))
  const harness = await CodexHarness.open(directory, "session", {})
  let start!: () => void
  startRequestGate = new Promise<void>(resolve => { start = resolve })
  const abort = new AbortController()
  let processing = true, registered = false
  const turn = { id: "turn", signal: abort.signal,
    onMessage: async () => { if (!processing) throw new Error("Turn processing admission is closed"); registered = true },
    output: { write: async () => ({ sequence: 1 }) },
  } as unknown as Turn
  onInterrupt = () => messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: "interrupted" } } })
  try {
    const pending = harness.run(turn, "original")
    void pending.catch(() => {})
    expect(registered).toBe(true)
    processing = false
    if (failure) abort.abort(new Error("application failed"))
    start()
    await new Promise<void>(resolve => setImmediate(resolve))
    if (failure) { await expect(pending).rejects.toThrow("application failed") }
    else {
      messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: "completed" } } })
      await pending
    }
    expect(harness.reusable).toBe(true)
  } finally { start(); startRequestGate = undefined; onInterrupt = undefined; finishExit(); messages.end(); await harness.close(); await rm(directory, { recursive: true, force: true }) }
})

test("a caught native result failure permits an ordinary retry in the same Turn", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-retry-"))
  const harness = await CodexHarness.open(directory, "session", {})
  let registrations = 0
  const turn = { id: "turn", signal: new AbortController().signal,
    onMessage: async () => { if (++registrations > 1) throw new Error("duplicate message handler") },
    output: { write: async () => ({ sequence: 1 }) },
  } as unknown as Turn
  try {
    const first = harness.run(turn, "first")
    await new Promise<void>(resolve => setImmediate(resolve))
    messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: "failed" } } })
    await expect(first).rejects.toThrow("Native Turn ended")
    await nativeFixture.operations[0]
    expect(harness.reusable).toBe(true)
    const second = harness.run(turn, "retry")
    await new Promise<void>(resolve => setImmediate(resolve))
    messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: "completed" } } })
    await second
    await nativeFixture.operations[1]
    expect(registrations).toBe(1)
  } finally { finishExit(); messages.end(); await harness.close(); await rm(directory, { recursive: true, force: true }) }
})

test("callback failure interrupts to reusable idle before returning the error", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-callback-failure-"))
  const harness = await CodexHarness.open(directory, "session", {})
  const turn = { id: "turn", signal: new AbortController().signal,
    onMessage: async () => {}, ask: async () => { throw new Error("ask registration failed") },
    output: { write: async () => ({ sequence: 1 }) },
  } as unknown as Turn
  onInterrupt = () => messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: "interrupted" } } })
  try {
    const running = harness.run(turn, "first")
    await new Promise<void>(resolve => setImmediate(resolve))
    messages.push({ id: 1, method: "item/commandExecution/requestApproval", params: { threadId: "native", turnId: "native-turn" } })
    await expect(running).rejects.toThrow("ask registration failed")
    await nativeFixture.operations[0]
    expect(harness.reusable).toBe(true)
  } finally { onInterrupt = undefined; finishExit(); messages.end(); await harness.close(); await rm(directory, { recursive: true, force: true }) }
})


test("registration failure before submission preserves the setup resource", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-registration-failure-"))
  const harness = await CodexHarness.open(directory, "session", {})
  const turn = { id: "turn", signal: new AbortController().signal,
    onMessage: () => { throw new Error("registration failed") },
    output: { write: async () => ({ sequence: 1 }) },
  } as unknown as Turn
  try {
    await expect(harness.run(turn, "never submitted")).rejects.toThrow("registration failed")
    expect(harness.idle).toBe(true)
    expect(harness.reusable).toBe(true)
  } finally { finishExit(); messages.end(); await harness.close(); await rm(directory, { recursive: true, force: true }) }
})

test("completed snapshots publish commentary once and commit only an exact successful final", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-content-"))
  const harness = await CodexHarness.open(directory, "session", {})
  const output: unknown[] = [], responses: unknown[] = []
  const turn = { id: "turn", signal: new AbortController().signal, onMessage: async () => {},
    output: { write: async (value: unknown) => { output.push(value); return { sequence: 1 } } }, respond: async (value: unknown) => { responses.push(value) },
  } as unknown as Turn
  const item = (id: string, phase?: string) => ({ method: "item/completed", params: { threadId: "native", turnId: "native-turn", item: { type: "agentMessage", id, text: id, phase } } })
  try {
    const pending = harness.run(turn, [{ type: "text", text: "inspect" }])
    await new Promise<void>(resolve => setImmediate(resolve))
    messages.push({ method: "item/agentMessage/delta", params: { threadId: "native", turnId: "native-turn", delta: "duplicate" } })
    messages.push(item("progress", "commentary")); messages.push(item("progress", "commentary"))
    messages.push(item("second", "commentary")); messages.push(item("unknown")); messages.push(item("answer", "final_answer"))
    messages.push({ method: "turn/completed", params: { threadId: "elsewhere", turn: { id: "native-turn", status: "completed" } } })
    await new Promise<void>(resolve => setImmediate(resolve))
    expect(output.join("")).toBe("progress\n\nsecond"); expect(responses).toEqual([])
    messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: "completed" } } })
    await pending
    expect(responses).toEqual(["answer"])
  } finally { finishExit(); messages.end(); await harness.close(); await rm(directory, { recursive: true, force: true }) }
})

test("terminal notification remains live while questions wait and no late native reply is sent", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-questions-"))
  const harness = await CodexHarness.open(directory, "session", {})
  let asks = 0, withdrawals = 0
  const turn = { id: "turn", signal: new AbortController().signal, onMessage: async () => {},
    ask: (_: unknown, { signal }: { signal: AbortSignal }) => { asks++; return new Promise((_, reject) => signal.addEventListener("abort", () => { withdrawals++; reject(signal.reason) })) },
  } as unknown as Turn
  try {
    const pending = harness.run(turn, "inspect")
    await new Promise<void>(resolve => setImmediate(resolve))
    messages.push({ id: 9, method: "item/tool/requestUserInput", params: { threadId: "native", turnId: "native-turn", questions: ["one", "two"].map(id => ({ id, header: id, question: id })) } })
    await new Promise<void>(resolve => setImmediate(resolve))
    expect(asks).toBe(2)
    messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: "completed" } } })
    await pending
    expect(withdrawals).toBe(2)
    expect(sent.some(value => value.id === 9)).toBe(false)
  } finally { finishExit(); messages.end(); await harness.close(); await rm(directory, { recursive: true, force: true }) }
})
