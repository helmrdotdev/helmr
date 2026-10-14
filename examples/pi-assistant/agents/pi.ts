import type { Turn } from "@helmr/sdk"
import type { AgentSession, ExtensionUIContext } from "@mariozechner/pi-coding-agent"
import { questionUI } from "./questions"

// One owned prompt on a fresh session; the application configures native tools,
// extensions, authentication, and the remaining UI before opening it.
export function runPi(turn: Turn, prompt: string, open: () => Promise<AgentSession>, baseUI: ExtensionUIContext): Promise<{ response?: string; cancelledDialogs: number }> {
  let session: AgentSession | undefined
  let cleanupFailure: unknown
  let running: Promise<{ response?: string; cancelledDialogs: number }>
  turn.onCancel(async () => {
    await session?.abort()
    await running.catch(() => {})
    if (cleanupFailure) throw cleanupFailure
  })
  running = run()
  return running

  async function run(): Promise<{ response?: string; cancelledDialogs: number }> {
    const native = session = await open()
    const signal = turn.signal
    let receiving = false
    let rejectFailure!: (error: unknown) => void
    const failure = new Promise<never>((_, reject) => { rejectFailure = reject })
    void failure.catch(() => {})
    let ownedPrompt: Promise<void> | undefined
    let output = Promise.resolve(), outputError: unknown
    let bytes = 0, messages = 0, candidate = "", cancelledDialogs = 0, hasOutput = false
    const seen = new WeakSet<object>()
    let ended: unknown, expected: unknown, promptDone = false
    let settleEvents!: () => void
    const eventsDone = new Promise<void>(resolve => { settleEvents = resolve })
    function checkEnd() { if (promptDone && ended === expected) settleEvents() }
    function publish() {
      const text = candidate; candidate = ""
      if (!text || signal.aborted) return
      output = output.then(async () => {
        signal.throwIfAborted()
        await turn.output.write(`${hasOutput ? "\n\n" : ""}${text}`)
        hasOutput = true
      }).catch(error => { outputError ??= error; rejectFailure(error); void native.abort().catch(rejectFailure) })
    }
    const onEvent: Parameters<AgentSession["subscribe"]>[0] = event => {
      if (!receiving || signal.aborted || outputError) return
      if ((event.type === "message_start" && event.message.role === "assistant") || event.type === "tool_execution_start") publish()
      if (event.type === "message_end" && event.message.role === "assistant") {
        if (seen.has(event.message)) return
        seen.add(event.message)
        const text = event.message.content.filter(block => block.type === "text").map(block => block.text).join("")
        bytes += Buffer.byteLength(text)
        if (++messages > 16384 || Buffer.byteLength(text) > 256 * 1024 || bytes > 8 * 1024 * 1024) {
          outputError = new Error("Pi assistant text exceeds the bridge limit")
          rejectFailure(outputError)
          void native.abort().catch(rejectFailure)
          return
        }
        candidate = text
      }
      if (event.type === "agent_end") { ended = [...event.messages].reverse().find(message => message.role === "assistant"); checkEnd() }
    }
    let unsubscribe = () => {}
    let abort!: () => void
    const aborted = new Promise<never>((_, reject) => { abort = () => { void native.abort().catch(rejectFailure); reject(signal.reason) }; signal.addEventListener("abort", abort, { once: true }) })
    void aborted.catch(() => {})
    try {
      signal.throwIfAborted()
      unsubscribe = native.subscribe(onEvent)
      await native.bindExtensions({ uiContext: questionUI(turn, baseUI, () => { cancelledDialogs++ }) })
      signal.throwIfAborted()
      const before = native.state.messages.at(-1)
      receiving = true
      ownedPrompt = native.prompt(prompt, { expandPromptTemplates: false })
      await Promise.race([ownedPrompt, aborted, failure])
      expected = [...native.state.messages].reverse().find(message => message.role === "assistant")
      promptDone = true
      // An input hook may consume a prompt without producing a message.
      if (native.state.messages.at(-1) !== before) { checkEnd(); await Promise.race([eventsDone, aborted, failure]) }
      await output
      if (outputError) throw outputError
      signal.throwIfAborted()
      const final = native.state.messages.at(-1)
      if (final !== before && final?.role === "assistant" && (final.stopReason === "error" || final.stopReason === "aborted")) throw new Error(`Pi prompt ${final.stopReason}`)
      if (final !== before && final?.role !== "assistant") throw new Error("Pi completed without a final assistant message")
      return { ...(candidate ? { response: candidate } : {}), cancelledDialogs }
    } catch (error) {
      if (!signal.aborted && !outputError) { publish(); await output }
      throw error
    } finally {
      signal.removeEventListener("abort", abort)
      try {
        await native.abort()
        await ownedPrompt?.catch(() => {})
        await output
        unsubscribe()
        native.dispose()
        session = undefined
      } catch (error) { cleanupFailure = error; throw error }
    }
  }
}
