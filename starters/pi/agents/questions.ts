import type { Turn } from "@helmr/sdk"
import type { ExtensionUIContext, ExtensionUIDialogOptions } from "@earendil-works/pi-coding-agent"

export function questions(base: ExtensionUIContext, current: () => { turn: Turn; cancel: AbortController } | undefined): ExtensionUIContext {
  async function ask<T>(opts: ExtensionUIDialogOptions | undefined, run: (turn: Turn, signal: AbortSignal) => Promise<T>): Promise<T | undefined> {
    const operation = current()
    if (!operation) return undefined
    const signals = [operation.cancel.signal, operation.turn.signal]
    if (opts?.signal) signals.push(opts.signal)
    if (opts?.timeout !== undefined) signals.push(AbortSignal.timeout(opts.timeout))
    const signal = AbortSignal.any(signals)
    if (signal.aborted) return undefined
    try {
      const result = await run(operation.turn, signal)
      return signal.aborted ? undefined : result
    } catch (error) { if (signal.aborted) return undefined; throw error }
  }
  const input: ExtensionUIContext["input"] = (title, placeholder, opts) => ask(opts, async (turn, signal) => {
    const { answer } = await turn.ask({ type: "text", prompt: [{ type: "text", text: placeholder ? `${title}\n${placeholder}` : title }] }, { signal })
    return answer
  })
  return {
    ...base,
    select: (title, choices, opts) => ask(opts, async (turn, signal) => {
      const { answer } = await turn.ask({ type: "choice", prompt: [{ type: "text", text: title }], options: choices.map((label, i) => ({ id: `option-${i}`, label, value: label })) }, { signal })
      return answer.selected.length === 1 ? String(answer.selected[0]!.value) : undefined
    }),
    confirm: async (title, message, opts) => (await ask(opts, async (turn, signal) => {
      const { answer } = await turn.ask({ type: "choice", prompt: [{ type: "text", text: `${title}\n${message}` }], options: [{ id: "yes", label: "Yes", value: true }, { id: "no", label: "No", value: false }] }, { signal })
      return answer.selected.length === 1 && answer.selected[0]?.value === true
    })) ?? false,
    input,
    editor: (title, prefill) => input(title, prefill),
    custom: async () => { throw new Error("Pi custom terminal components require its native interactive interface") },
  }
}
