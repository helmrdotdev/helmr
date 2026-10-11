import { readFileSync } from "node:fs"

export interface NativeStopModelState {
  normal: Record<"codex" | "claude", number>
  controls: Record<string, number>
}

// Retained in the model Session's setup state across both RAM restores.
export function nativeStopModelState(): NativeStopModelState {
  return { normal: { codex: 0, claude: 0 }, controls: {} }
}

export function nativeStopResponse(provider: "codex" | "claude", state: NativeStopModelState) {
  let control: { turnId: string; directory: string }
  try { control = JSON.parse(readFileSync(`/workspace/native-stop-${provider}.json`, "utf8")) }
  catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error
    return { normalIndex: ++state.normal[provider] }
  }
  const index = state.controls[control.turnId] = (state.controls[control.turnId] ?? 0) + 1
  if (provider === "codex") {
    const name = index === 1 ? "exec_command" : "request_user_input"
    const args = index === 1
      ? { cmd: `sh -c 'echo $$ > ${control.directory}/owned-command-${control.turnId}.pid; exec sleep 120'`, yield_time_ms: 100 }
      : { questions: [{ id: "choice", header: "Choice", question: "Continue the control fixture?", options: [{ label: "A", description: "First" }, { label: "B", description: "Second" }] }] }
    return { codex: index <= 2 ? { type: "function_call", id: `fc_stop_${control.turnId}_${index}`, call_id: `call_stop_${control.turnId}_${index}`, name, arguments: JSON.stringify(args) } : undefined }
  }
  return { claude: index === 1 ? { id: `toolu_stop_${control.turnId}`, name: "AskUserQuestion", input: { questions: [{ question: "Continue the control fixture?", header: "Choice", multiSelect: false, options: [{ label: "A", description: "First" }, { label: "B", description: "Second" }] }] } } : undefined }
}
