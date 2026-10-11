import assert from "node:assert/strict"

// Follow the real native tool result. A yielded shell handle is still running;
// request count/parity is not evidence that the command finished successfully.
export function performanceTool(provider: "codex" | "claude", command: () => string) {
  let active: { command: string; callId: string; started: number; complete: boolean; processId?: number } | undefined
  return (body: any, index: number): any => {
    const current = command()
    const callId = `performance_${provider}_${index}`
    if (!active || active.command !== current) {
      assert(!active || active.complete, "native command changed before its tool completed")
      active = { command: current, callId, started: performance.now(), complete: false }
      return provider === "codex"
        ? { type: "function_call", id: callId, call_id: callId, name: "exec_command", arguments: JSON.stringify({ cmd: current, yield_time_ms: 30000, max_output_tokens: 2000 }) }
        : { id: callId, name: "Bash", input: { command: current, timeout: 120000, description: "Run the isolated performance workload" } }
    }
    assert(!active.complete, "unexpected model request after performance completion")
    assert(performance.now() - active.started < 120000, "native performance command exceeded its deadline")
    if (provider === "codex") {
      const result = body.input?.findLast((item: any) => item.type === "function_call_output" && item.call_id === active!.callId)
      assert(result && typeof result.output === "string", "missing exact native command result")
      const running = /Process running with session ID (\d+)/.exec(result.output)
      if (running) {
        const processId = Number(running[1])
        assert(Number.isSafeInteger(processId))
        if (active.processId !== undefined) assert.equal(processId, active.processId)
        active.processId = processId
        active.callId = callId
        return { type: "function_call", id: callId, call_id: callId, name: "write_stdin", arguments: JSON.stringify({ session_id: processId, chars: "", yield_time_ms: 1000, max_output_tokens: 2000 }) }
      }
      const exit = /Process exited with code (-?\d+)/.exec(result.output)
      assert(exit, `native command result lacks exit status: ${result.output.slice(0, 2048)}`)
      assert.equal(Number(exit[1]), 0, result.output)
    } else {
      const parts = (body.messages ?? []).flatMap((message: any) => Array.isArray(message.content) ? message.content : [])
      const result = parts.findLast((part: any) => part.type === "tool_result" && part.tool_use_id === active!.callId)
      assert(result && result.is_error !== true, "native Bash command failed or has no matching result")
      const text = typeof result.content === "string" ? result.content : result.content?.map((part: any) => part.text ?? "").join("\n")
      assert(typeof text === "string")
      // The workload prints a receipt only after all assertions and writes.
      // A timed-out/background Bash response is deliberately not completion.
      assert(text.split("\n").some(line => {
        try { const result = JSON.parse(line); return ["no-op", "edit-test", "dependencies"].includes(result.kind) && Number.isSafeInteger(result.sequence) && Number.isFinite(result.operationMs) }
        catch { return false }
      }), "Bash did not return the completed workload receipt")
    }
    active.complete = true
    return undefined
  }
}
