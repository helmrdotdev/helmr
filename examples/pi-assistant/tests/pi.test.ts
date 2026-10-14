import { expect, test } from "bun:test"
import type { AgentSession, ExtensionUIContext } from "@mariozechner/pi-coding-agent"
import type { Turn } from "@helmr/sdk"
import { runPi } from "../agents/pi"
import { questionUI } from "../agents/questions"
function fixture() {
  const output: string[] = []
  const controller = new AbortController()
  const turn = { signal: controller.signal, onCancel: () => {}, output: { write: async (text: string) => { output.push(text) } } } as unknown as Turn
  return { turn, output, controller }
}
const baseUI = {} as ExtensionUIContext
function assistant(text: string, stopReason = "stop") { return { role: "assistant", content: [{ type: "text", text }, { type: "thinking", thinking: "private" }], stopReason } }
function session(run: (emit: (event: any) => void, messages: any[]) => Promise<void>) {
  let emit: (event: any) => void = () => {}
  const messages: any[] = []
  return { state: { messages }, subscribe: (cb: typeof emit) => { emit = cb; return () => { emit = () => {} } }, bindExtensions: async () => {}, abort: async () => {}, dispose: () => {}, prompt: () => run(emit, messages) } as unknown as AgentSession
}
test("Pi waits through cycle ends and delayed final notification; application owns respond", async () => {
  const { turn, output } = fixture()
  const native = session(async (emit, messages) => {
    const first = assistant("Checking "), final = assistant("Done")
    first.content.push({ type: "text", text: "callers" })
    messages.push(first)
    emit({ type: "message_end", message: first })
    emit({ type: "agent_end", messages: [first] })
    emit({ type: "tool_execution_start" })
    messages.push(final)
    setTimeout(() => { emit({ type: "message_end", message: final }); emit({ type: "agent_end", messages: [final] }) }, 5)
  })
  expect(await runPi(turn, "hello", async () => native, baseUI)).toEqual({ response: "Done", cancelledDialogs: 0 })
  expect(output).toEqual(["Checking callers"])
})
test("failed Pi completion publishes held user text and never returns a response", async () => {
  const { turn, output } = fixture()
  const native = session(async (emit, messages) => {
    const final = assistant("Could not finish", "error")
    messages.push(final); emit({ type: "message_end", message: final }); emit({ type: "agent_end", messages: [final] })
  })
  await expect(runPi(turn, "hello", async () => native, baseUI)).rejects.toThrow("Pi prompt error")
  expect(output).toEqual(["Could not finish"])
})
test("Pi dialog cancellation is distinct from an explicit No", async () => {
  const { turn, controller } = fixture()
  let cancellations = 0
  turn.ask = async () => ({ answer: { selected: [{ id: "no", value: false }] }, answeredBy: { kind: "user", id: "fixture" } }) as never
  const ui = questionUI(turn, baseUI, () => cancellations++)
  expect(await ui.confirm("Proceed?", "Apply edit")).toBe(false)
  expect(cancellations).toBe(0)
  controller.abort()
  expect(await ui.confirm("Proceed?", "Apply edit")).toBe(false)
  expect(await ui.input("Name")).toBeUndefined()
  expect(await ui.select("Pick", ["A"])).toBeUndefined()
  expect(cancellations).toBe(3)
})

test("Pi cancellation joins its prompt and cleanup before rejecting", async () => {
  const { turn, output, controller } = fixture()
  let release!: () => void, started!: () => void
  const ready = new Promise<void>(resolve => { started = resolve })
  const pending = new Promise<void>(resolve => { release = resolve })
  let disposed = false, joined = false
  const native = session(async emit => {
    emit({ type: "message_end", message: assistant("held final candidate") })
    started(); await pending; joined = true
  })
  native.abort = async () => { release(); await pending }
  native.dispose = () => { disposed = true }
  const running = runPi(turn, "hello", async () => native, baseUI)
  await ready
  controller.abort(new Error("cancelled turn"))
  await expect(running).rejects.toThrow("cancelled turn")
  expect(joined && disposed).toBe(true)
  expect(output).toEqual([])
})

test("Pi rejects closed cancellation registration before opening a session", async () => {
  const { turn } = fixture()
  turn.onCancel = () => { throw new Error("registration closed") }
  let disposed = false, aborted = false
  const native = session(async () => { throw new Error("must not prompt") })
  native.abort = async () => { aborted = true }
  native.dispose = () => { disposed = true }
  expect(() => runPi(turn,"hello",async () => native,baseUI)).toThrow("registration closed")
  expect(aborted || disposed).toBe(false)
})


test("Pi cancellation waits for an opening session and disposes it without prompting", async () => {
  const { turn, controller } = fixture()
  let cancel!: (reason: unknown) => void | Promise<void>, release!: (value: AgentSession) => void
  turn.onCancel = callback => { cancel = callback }
  let disposed = false
  const native = session(async () => { throw new Error("must not prompt") })
  native.dispose = () => { disposed = true }
  const opening = new Promise<AgentSession>(resolve => { release = resolve })
  const running = runPi(turn, "hello", () => opening, baseUI)
  const rejected = running.catch(error => error)
  controller.abort(new Error("stopped during open"))
  let joined = false
  const cancellation = Promise.resolve(cancel(controller.signal.reason)).then(() => { joined = true })
  await Promise.resolve()
  expect(joined).toBe(false)
  release(native)
  expect((await rejected).message).toBe("stopped during open")
  await cancellation
  expect(disposed).toBe(true)
})

test("Pi cancellation reports failed native cleanup to the runtime", async () => {
  const { turn } = fixture()
  let cancel!: (reason: unknown) => void | Promise<void>
  turn.onCancel = callback => { cancel = callback }
  const native = session(async () => {})
  native.dispose = () => { throw new Error("native cleanup failed") }
  await expect(runPi(turn, "hello", async () => native, baseUI)).rejects.toThrow("native cleanup failed")
  await expect(Promise.resolve(cancel(new Error("handler failed")))).rejects.toThrow("native cleanup failed")
})
