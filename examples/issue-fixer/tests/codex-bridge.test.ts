// Isolated transport failure test; real-native probes run in another process.
import { EventEmitter } from "node:events"
import { expect, mock, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { join } from "node:path"
import { tmpdir } from "node:os"
import type { Turn } from "../../../sdk/typescript/src/agent"
import { MessageRejected } from "../../../sdk/typescript/src/message-error"
import { Queue } from "../agents/issue-fixer/queue"

let messages: Queue<any>
let sent: any[] = []
let requested: string[] = []
let startRequestGate: Promise<void> | undefined
let onInterrupt: (() => void) | undefined
let finishExit!: () => void
class Rejected extends Error { constructor(readonly code: number, message: string) { super(message) } }
mock.module("../agents/issue-fixer/codex-stdio", () => ({
  spawnCodex: () => { const child = Object.assign(new EventEmitter(), { pid: 123 }); queueMicrotask(() => child.emit("spawn")); return child }, CodexRequestRejected: Rejected,
  CodexStdio: class {
    readonly messages = messages = new Queue()
    constructor() { sent = []; requested = [] }
    readonly processId = 123
    readonly exit = new Promise<void>(resolve => { finishExit = resolve })
    async request(method: string) {
      requested.push(method)
      if (method === "turn/steer") throw new Rejected(-32600, "no active turn to steer")
      if (method === "turn/start") { await startRequestGate; return { turn: { id: "native-turn", status: "inProgress" } } }
      if (method === "turn/interrupt") onInterrupt?.()
      return method === "thread/start" ? { thread: { id: "native" } } : {}
    }
    async send(value: any) { sent.push(value) }
    close() { return this.exit }
  },
}))
const { CodexBridge } = await import("../agents/issue-fixer/codex-bridge")

test("every close caller waits for physical exit after reader failure", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-close-"))
  const bridge = await CodexBridge.open(directory, "session", {})
  try {
    messages.end(new Error("malformed native frame"))
    await new Promise<void>(resolve => setImmediate(resolve))
    let resolved = 0
    const first = bridge.close(), second = bridge.close()
    expect(first).toBe(second)
    void first.then(() => resolved++)
    void second.then(() => resolved++)
    await new Promise<void>(resolve => setImmediate(resolve))
    expect(resolved).toBe(0)
    finishExit()
    await Promise.all([first, second])
    expect(resolved).toBe(2)
  } finally { finishExit(); await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})


test("native steering rejection can precede the queued completion notification", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-steer-"))
  const bridge = await CodexBridge.open(directory, "session", {})
  let handler!: (value: unknown) => Promise<void>, registered!: () => void
  const ready = new Promise<void>(resolve => { registered = resolve })
  const turn = { id: "turn", signal: new AbortController().signal,
    onCancel: () => {}, onMessage: async (callback: typeof handler) => { handler = callback; registered() },
    output: { write: async () => ({ sequence: 1 }) },
  } as unknown as Turn
  try {
    const pending = bridge.run(turn, "original")
    await ready
    await expect(handler({ id: "message", content: [{ type:"text", text:"too late" }], source: { kind: "user" } })).rejects.toBeInstanceOf(MessageRejected)
    messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: "completed" } } })
    await expect(pending).resolves.toEqual({ nativeThreadId: "native", nativeTurnId: "native-turn" })
  } finally { finishExit(); messages.end(); await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})


test("failed Codex initialization rejects the awaited open", async () => {
  await expect(CodexBridge.open("/dev/null/invalid-native-directory", "session", {})).rejects.toThrow()
  await new Promise<void>(resolve => setImmediate(resolve))
})


for (const failure of [false, true]) test(`awaited native startup drains after application ${failure ? "failure" : "return"}`, async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-pending-start-"))
  const bridge = await CodexBridge.open(directory, "session", {})
  let start!: () => void
  startRequestGate = new Promise<void>(resolve => { start = resolve })
  const abort = new AbortController()
  let processing = true, registered = false
  const turn = { id: "turn", signal: abort.signal,
    onCancel: () => {}, onMessage: async () => { if (!processing) throw new Error("Turn processing admission is closed"); registered = true },
    output: { write: async () => ({ sequence: 1 }) },
  } as unknown as Turn
  onInterrupt = () => messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: "interrupted" } } })
  try {
    const pending = bridge.run(turn, "original")
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
    expect(bridge.reusable).toBe(true)
  } finally { start(); startRequestGate = undefined; onInterrupt = undefined; finishExit(); messages.end(); await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})

test("a caught native result failure permits an ordinary retry in the same Turn", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-retry-"))
  const bridge = await CodexBridge.open(directory, "session", {})
  let registrations = 0
  const turn = { id: "turn", signal: new AbortController().signal,
    onCancel: () => {}, onMessage: async () => { if (++registrations > 1) throw new Error("duplicate message handler") },
    output: { write: async () => ({ sequence: 1 }) },
  } as unknown as Turn
  try {
    const first = bridge.run(turn, "first")
    await new Promise<void>(resolve => setImmediate(resolve))
    messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: "failed" } } })
    await expect(first).rejects.toThrow("Native Turn ended")
    expect(bridge.reusable).toBe(true)
    const second = bridge.run(turn, "retry")
    await new Promise<void>(resolve => setImmediate(resolve))
    messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: "completed" } } })
    await second
    expect(registrations).toBe(1)
  } finally { finishExit(); messages.end(); await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})

test("callback failure interrupts to reusable idle before returning the error", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-callback-failure-"))
  const bridge = await CodexBridge.open(directory, "session", {})
  const turn = { id: "turn", signal: new AbortController().signal,
    onCancel: () => {}, onMessage: async () => {}, ask: async () => { throw new Error("ask registration failed") },
    output: { write: async () => ({ sequence: 1 }) },
  } as unknown as Turn
  onInterrupt = () => messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: "interrupted" } } })
  try {
    const running = bridge.run(turn, "first")
    await new Promise<void>(resolve => setImmediate(resolve))
    messages.push({ id: 1, method: "item/commandExecution/requestApproval", params: { threadId: "native", turnId: "native-turn" } })
    await expect(running).rejects.toThrow("ask registration failed")
    expect(bridge.reusable).toBe(true)
  } finally { onInterrupt = undefined; finishExit(); messages.end(); await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})


test("registration failure before submission preserves the setup resource", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-registration-failure-"))
  const bridge = await CodexBridge.open(directory, "session", {})
  const turn = { id: "turn", signal: new AbortController().signal,
    onCancel: () => {}, onMessage: () => { throw new Error("registration failed") },
    output: { write: async () => ({ sequence: 1 }) },
  } as unknown as Turn
  try {
    await expect(bridge.run(turn, "never submitted")).rejects.toThrow("registration failed")
    expect(bridge.idle).toBe(true)
    expect(bridge.reusable).toBe(true)
  } finally { finishExit(); messages.end(); await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})

test("completed snapshots publish commentary once and commit only an exact successful final", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-content-"))
  const bridge = await CodexBridge.open(directory, "session", {})
  const output: unknown[] = [], responses: unknown[] = []
  const turn = { id: "turn", signal: new AbortController().signal, onCancel: () => {}, onMessage: async () => {},
    output: { write: async (value: unknown) => { output.push(value); return { sequence: 1 } } }, respond: async (value: unknown) => { responses.push(value) },
  } as unknown as Turn
  const item = (id: string, phase?: string) => ({ method: "item/completed", params: { threadId: "native", turnId: "native-turn", item: { type: "agentMessage", id, text: id, phase } } })
  try {
    const pending = bridge.run(turn, [{ type: "text", text: "inspect" }])
    await new Promise<void>(resolve => setImmediate(resolve))
    messages.push({ method: "item/agentMessage/delta", params: { threadId: "native", turnId: "native-turn", delta: "duplicate" } })
    messages.push(item("progress", "commentary")); messages.push(item("progress", "commentary"))
    messages.push(item("second", "commentary")); messages.push(item("unknown")); messages.push(item("answer", "final_answer"))
    messages.push({ method: "turn/completed", params: { threadId: "elsewhere", turn: { id: "native-turn", status: "completed" } } })
    await new Promise<void>(resolve => setImmediate(resolve))
    expect(output.join("")).toBe("progress\n\nsecond\n\nunknown"); expect(responses).toEqual([])
    messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: "completed" } } })
    expect((await pending).response).toBe("answer")
    expect(responses).toEqual([])
  } finally { finishExit(); messages.end(); await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})

test("terminal notification remains live while questions wait and no late native reply is sent", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-questions-"))
  const bridge = await CodexBridge.open(directory, "session", {})
  let asks = 0, withdrawals = 0
  const turn = { id: "turn", signal: new AbortController().signal, onCancel: () => {}, onMessage: async () => {},
    ask: (_: unknown, { signal }: { signal: AbortSignal }) => { asks++; return new Promise((_, reject) => signal.addEventListener("abort", () => { withdrawals++; reject(signal.reason) })) },
  } as unknown as Turn
  try {
    const pending = bridge.run(turn, "inspect")
    await new Promise<void>(resolve => setImmediate(resolve))
    messages.push({ id: 9, method: "item/tool/requestUserInput", params: { threadId: "native", turnId: "native-turn", questions: ["one", "two"].map(id => ({ id, header: id, question: id })) } })
    await new Promise<void>(resolve => setImmediate(resolve))
    expect(asks).toBe(2)
    messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: "completed" } } })
    await pending
    expect(withdrawals).toBe(2)
    expect(sent.some(value => value.id === 9)).toBe(false)
  } finally { finishExit(); messages.end(); await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})

for (const status of ["completed", "failed", "declined"]) test(`a yielded ${status} command receipt may arrive after native Turn completion`, async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-late-command-"))
  const bridge = await CodexBridge.open(directory, "session", {})
  const turn = { id: "turn", signal: new AbortController().signal, onCancel: () => {}, onMessage: async () => {}, output: { write: async () => ({ sequence: 1 }) } } as unknown as Turn
  try {
    const pending = bridge.run(turn, "inspect")
    void pending.catch(() => {})
    let settled = false
    void pending.then(() => { settled = true }, () => { settled = true })
    await new Promise<void>(resolve => setImmediate(resolve))
    messages.push({ method: "item/started", params: { threadId: "native", turnId: "native-turn", item: { type: "commandExecution", id: "command", status: "inProgress", exitCode: null } } })
    messages.push({ method: "item/started", params: { threadId: "native", turnId: "native-turn", item: { type: "commandExecution", id: "earlier", status: "inProgress", exitCode: null } } })
    messages.push({ method: "item/completed", params: { threadId: "native", turnId: "native-turn", item: { type: "commandExecution", id: "earlier", status: "completed", exitCode: 0 } } })
    messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: "completed" } } })
    for (const [threadId, turnId, id] of [["foreign", "native-turn", "command"], ["native", "foreign", "command"], ["native", "native-turn", "unknown"]]) {
      messages.push({ method: "item/completed", params: { threadId, turnId, item: { type: "commandExecution", id, status: "completed", exitCode: 0 } } })
    }
    await new Promise<void>(resolve => setImmediate(resolve))
    expect(settled).toBe(false)
    expect(bridge.idle).toBe(false)
    expect(requested).not.toContain("thread/backgroundTerminals/clean")
    messages.push({ method: "item/completed", params: { threadId: "native", turnId: "native-turn", item: { type: "commandExecution", id: "command", status, exitCode: status === "completed" ? 0 : null } } })
    await expect(pending).resolves.toEqual({ nativeThreadId: "native", nativeTurnId: "native-turn" })
    expect(bridge.reusable).toBe(true)
    const next = bridge.run(turn, "next")
    await new Promise<void>(resolve => setImmediate(resolve))
    messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: "completed" } } })
    await next
  } finally { finishExit(); messages.end(); await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})

for (const failure of ["cancel", "cancel-race", "reader-error", "reader-end", "cancel-reader-error"]) test(`outstanding command receipt joins through ${failure}`, async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-command-join-"))
  const bridge = await CodexBridge.open(directory, "session", {})
  const abort = new AbortController()
  const turn = { id: "turn", signal: abort.signal, onCancel: () => {}, onMessage: async () => {}, output: { write: async () => ({ sequence: 1 }) } } as unknown as Turn
  const receipt = { method: "item/completed", params: { threadId: "native", turnId: "native-turn", item: { type: "commandExecution", id: "command", status: "completed", exitCode: 0 } } }
  try {
    const pending = bridge.run(turn, "inspect")
    let settled = false
    void pending.then(() => { settled = true }, () => { settled = true })
    await new Promise<void>(resolve => setImmediate(resolve))
    messages.push({ method: "item/started", params: { threadId: "native", turnId: "native-turn", item: { type: "commandExecution", id: "command", status: "inProgress", exitCode: null } } })
    messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: "completed" } } })
    await new Promise<void>(resolve => setImmediate(resolve))
    expect(settled).toBe(false)
    const cancelled = new Error("owned Turn cancelled")
    if (failure.startsWith("cancel")) {
      if (failure === "cancel-race") messages.push(receipt)
      abort.abort(cancelled)
      await new Promise<void>(resolve => setImmediate(resolve))
      if (failure !== "cancel-race") {
        expect(requested.filter(value => value === "thread/backgroundTerminals/clean")).toHaveLength(1)
        expect(settled).toBe(false)
      }
      if (failure === "cancel") messages.push(receipt)
    }
    if (failure.includes("reader")) {
      messages.end(failure === "reader-end" ? undefined : new Error("lost native reader"))
      finishExit()
    }
    await expect(pending).rejects.toThrow(failure.startsWith("cancel") ? "owned Turn cancelled" : failure === "reader-end" ? "Native message stream ended" : "lost native reader")
    expect(bridge.reusable).toBe(!failure.includes("reader"))
    expect(bridge.idle).toBe(true)
  } finally { finishExit(); messages.end(); await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})

for (const status of ["completed", "failed", "declined"]) test(`terminal command ${status} without exit code drains before Turn completion`, async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-terminal-command-"))
  const bridge = await CodexBridge.open(directory, "session", {})
  const turn = { id: "turn", signal: new AbortController().signal, onCancel: () => {}, onMessage: async () => {}, output: { write: async () => ({ sequence: 1 }) } } as unknown as Turn
  try {
    const pending = bridge.run(turn, "inspect")
    await new Promise<void>(resolve => setImmediate(resolve))
    messages.push({ method: "item/started", params: { threadId: "native", turnId: "native-turn", item: { type: "commandExecution", id: "command", status: "inProgress", exitCode: null } } })
    messages.push({ method: "item/completed", params: { threadId: "native", turnId: "native-turn", item: { type: "commandExecution", id: "command", status, exitCode: null } } })
    messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: "completed" } } })
    await pending
    expect(bridge.reusable).toBe(true)
  } finally { finishExit(); messages.end(); await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})

test("a retained protocol failure rejects the next prompt and its cancellation hook", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-broken-"))
  const bridge = await CodexBridge.open(directory, "session", {})
  let cancel!: (reason: unknown) => void | Promise<void>
  try {
    const failure = new Error("malformed native frame")
    messages.end(failure)
    await new Promise<void>(resolve => setImmediate(resolve))
    const before = sent.length
    const turn = { id: "next", signal: new AbortController().signal, onCancel: (hook: typeof cancel) => { cancel = hook } } as unknown as Turn
    await expect(bridge.run(turn, "must not be submitted")).rejects.toBe(failure)
    await expect(cancel(new Error("handler failed"))).rejects.toBe(failure)
    expect(sent.length).toBe(before)
  } finally { finishExit(); await bridge.close(); await rm(directory, { recursive: true, force: true }) }
})

for (const scenario of ["phase-less", "continued", "unknown", "unknown-with-final", "async-question", "native-failure"] as const) {
  test(`native text classification: ${scenario}`, async () => {
    const directory = await mkdtemp(join(tmpdir(), "helmr-native-phases-"))
    const bridge = await CodexBridge.open(directory, "session", {})
    const output: string[] = []
    const turn = { id: "turn", signal: new AbortController().signal, onCancel: () => {}, onMessage: async () => {},
      output: { write: async (text: string) => { output.push(text); return { sequence: output.length } } },
      respond: async () => { throw new Error("Bridge must not own final response") },
    } as unknown as Turn
    const item = (id: string, extra = {}) => ({ method: "item/completed", params: { threadId: "native", turnId: "native-turn", item: { type: "agentMessage", id, text: id, ...extra } } })
    try {
      const pending = bridge.run(turn, "inspect")
      await new Promise<void>(resolve => setImmediate(resolve))
      messages.push(item("candidate"))
      if (scenario === "continued") {
        messages.push({ method: "item/started", params: { threadId: "native", turnId: "native-turn", item: { type: "webSearch", id: "tool" } } })
        messages.push(item("final"))
      }
      if (scenario.startsWith("unknown")) messages.push(item("excluded", { phase: "unsupported_phase" }))
      if (scenario === "unknown-with-final") messages.push(item("final", { phase: "final_answer" }))
      if (scenario === "async-question") messages.push(item("detached", { phase: "final_answer", delivery: "async", questions: [{ question: "Unowned?" }] }))
      messages.push({ method: "turn/completed", params: { threadId: "native", turn: { id: "native-turn", status: scenario === "native-failure" ? "failed" : "completed" } } })
      if (scenario === "unknown") await expect(pending).rejects.toThrow("unsupported message phase")
      else if (scenario === "async-question") await expect(pending).rejects.toThrow("asynchronous questions")
      else if (scenario === "native-failure") await expect(pending).rejects.toThrow("Native Turn ended")
      else expect((await pending).response).toBe(scenario === "phase-less" ? "candidate" : "final")
      expect(output.join("")).toBe(["continued", "unknown-with-final", "native-failure"].includes(scenario) ? "candidate" : "")
    } finally { finishExit(); messages.end(); await bridge.close(); await rm(directory, { recursive: true, force: true }) }
  })
}
