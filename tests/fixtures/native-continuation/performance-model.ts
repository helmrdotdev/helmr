import assert from "node:assert/strict"

export type PerformanceCommand = { command: string; kind: "no-op" | "edit-test" | "dependencies" }
export const performanceCommandTimeout = (kind: PerformanceCommand["kind"]) => kind === "dependencies" ? 300000 : 120000

// Follow the real native tool result. A yielded shell handle is still running;
// request count/parity is not evidence that the command finished successfully.
export function performanceTool(provider: "codex" | "claude", command: () => PerformanceCommand,
  observation: { now?: () => number; record?: (value: Record<string, unknown>) => void } = {}) {
  const now = observation.now ?? (() => performance.now())
  const record = observation.record ?? (value => console.error(JSON.stringify(value)))
  let active: { command: string; kind: PerformanceCommand["kind"]; callId: string; started: number; issuedMonotonicNs: string; issuedWallMs: number; complete: boolean; processId?: number } | undefined
  return (body: any, index: number): any => {
    const current = command()
    const callId = `performance_${provider}_${index}`
    if (!active || active.command !== current.command) {
      assert(!active || active.complete, "native command changed before its tool completed")
      active = { ...current, callId, started: now(), issuedWallMs: Date.now(), issuedMonotonicNs: process.hrtime.bigint().toString(), complete: false }
      return provider === "codex"
        ? { type: "function_call", id: callId, call_id: callId, name: "exec_command", arguments: JSON.stringify({ cmd: current.command, yield_time_ms: 30000, max_output_tokens: 2000 }) }
        : { id: callId, name: "Bash", input: { command: current.command, timeout: performanceCommandTimeout(current.kind), description: "Run the isolated performance workload" } }
    }
    assert(!active.complete, "unexpected model request after performance completion")
    const elapsedMS = now() - active.started
    const observationBudgetMS = performanceCommandTimeout(active.kind)
    const observe = (resultState: string, fields: Record<string, unknown> = {}) => record({
      event: "native.performance.command_observation", provider, kind: active!.kind, command: active!.command,
      issuedWallMs:active!.issuedWallMs, observedWallMs:Date.now(), issuedMonotonicNs:active!.issuedMonotonicNs, observedMonotonicNs:process.hrtime.bigint().toString(), callId: active!.callId, processId: active!.processId, elapsedMS, targetMS: 120000, targetMiss: elapsedMS >= 120000,
      observationBudgetMS, observationBudgetExceeded: elapsedMS >= observationBudgetMS, resultState, ...fields,
    })
    const invalid = (message: string): never => { observe("invalid", { reason: message }); assert.fail(message) }
    let running = false
    if (provider === "codex") {
      const result = body.input?.findLast((item: any) => item.type === "function_call_output" && item.call_id === active!.callId)
      if (!result || typeof result.output !== "string") invalid("missing exact native command result")
      const yielded = /Process running with session ID (\d+)/.exec(result.output)
      if (yielded) {
        const processId = Number(yielded[1])
        if (!Number.isSafeInteger(processId)) invalid("invalid native process identity")
        if (active.processId !== undefined && processId !== active.processId) invalid("native process identity changed")
        active.processId = processId
        running = true
        observe("running")
      } else {
        const exit = /Process exited with code (-?\d+)/.exec(result.output)
        if (!exit) invalid("native command result lacks exit status")
        const exitCode = Number(exit![1])
        const nativeWallSeconds = /Wall time: ([0-9.]+) seconds/.exec(result.output)?.[1]
 observe("exited", { exitCode, ...(nativeWallSeconds ? { nativeReportedWallMs: Number(nativeWallSeconds) * 1000 } : {}), ...workloadDiagnostic(result.output) })
        assert.equal(exitCode, 0, result.output)
      }
    } else {
      const parts = (body.messages ?? []).flatMap((message: any) => Array.isArray(message.content) ? message.content : [])
      const result = parts.findLast((part: any) => part.type === "tool_result" && part.tool_use_id === active!.callId)
      if (!result) invalid("missing exact native Bash result")
      if (result.is_error === true) { observe("failed"); assert.fail("native Bash command failed") }
      const text = typeof result.content === "string" ? result.content : result.content?.map((part: any) => part.text ?? "").join("\n")
      if (typeof text !== "string") invalid("native Bash result lacks text")
      // The workload prints a receipt only after all assertions and writes.
      // A timed-out/background Bash response is deliberately not completion.
      const receipt = text.split("\n").some((line: string) => {
        try { const result = JSON.parse(line); return ["no-op", "edit-test", "dependencies"].includes(result.kind) && Number.isSafeInteger(result.sequence) && Number.isFinite(result.operationMs) }
        catch { return false }
      })
      if (!receipt) invalid("Bash did not return the completed workload receipt")
      observe("receipt", workloadDiagnostic(text))
    }
    // This bounds receipt admissibility when the model is called, not a watchdog
    // for a missing response. The driver retains its independent outer deadline.
    assert(elapsedMS < observationBudgetMS, "native performance command exceeded its observation budget")
    if (running) {
      active.callId = callId
      return { type: "function_call", id: callId, call_id: callId, name: "write_stdin", arguments: JSON.stringify({ session_id: active.processId, chars: "", yield_time_ms: 1000, max_output_tokens: 2000 }) }
    }
    active.complete = true
    return undefined
  }
}

function workloadDiagnostic(text: string): Record<string, unknown> {
 for (const line of text.split("\n")) {
  try {
   const receipt = JSON.parse(line)
   if (Number.isSafeInteger(receipt.sequence) && receipt.diagnosticEntry && receipt.diagnosticReceipt) return {workloadDiagnostic:{sequence:receipt.sequence, entry:receipt.diagnosticEntry, receipt:receipt.diagnosticReceipt}}
  } catch {}
 }
 return {}
}
