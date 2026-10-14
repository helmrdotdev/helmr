import type { Turn } from "@helmr/sdk"
import type { ExtensionUIContext, ExtensionUIDialogOptions } from "@mariozechner/pi-coding-agent"

// Other UI methods retain the application's native implementation. This is not
// a permission engine: extensions still own their exact authorization requests.
export function questionUI(turn: Turn, base: ExtensionUIContext, cancelled: () => void): ExtensionUIContext {
  async function ask<T>(options: ExtensionUIDialogOptions | undefined, run: (signal: AbortSignal) => Promise<T>): Promise<T | undefined> {
    const signal = AbortSignal.any([turn.signal, ...(options?.signal ? [options.signal] : []), ...(options?.timeout === undefined ? [] : [AbortSignal.timeout(options.timeout)])])
    try { signal.throwIfAborted(); return await run(signal) }
    catch (error) { if (!signal.aborted) throw error; cancelled(); return undefined }
  }
  return {
    ...base,
    select: (title, options, opts) => ask(opts, async signal => {
      const result = await turn.ask({ type: "choice", prompt: title, options: options.map((label, i) => ({ id: `option-${i}`, label, value: label })) }, { signal })
      return result.answer.selected[0]?.value
    }),
    input: (title, placeholder, opts) => ask(opts, async signal => {
      const result = await turn.ask({ type: "text", prompt: placeholder ? `${title}\n\n${placeholder}` : title }, { signal })
      return result.answer
    }),
    confirm: async (title, message, opts) => (await ask(opts, async signal => {
      const result = await turn.ask({ type: "choice", title, prompt: message, options: [{ id: "yes", label: "Yes", value: true }, { id: "no", label: "No", value: false }] }, { signal })
      return result.answer.selected[0]?.value ?? false
    })) ?? false,
  }
}
