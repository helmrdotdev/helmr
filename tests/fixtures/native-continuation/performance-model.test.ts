import assert from "node:assert/strict"
import { test } from "node:test"
import { performanceTool } from "./performance-model"

test("performance model follows the exact yielded native command until exit", () => {
  let command = "first"
  const next = performanceTool("codex", () => command)
  const first = next({}, 1)
  const waiting = next({ input: [{ type: "function_call_output", call_id: first.call_id, output: "Process running with session ID 42" }] }, 2)
  assert.equal(waiting.name, "write_stdin")
  assert.equal(JSON.parse(waiting.arguments).session_id, 42)
  assert.throws(() => next({ input: [{ type: "function_call_output", call_id: first.call_id, output: "Process exited with code 0" }] }, 3), /exact native/)
  const again = next({ input: [{ type: "function_call_output", call_id: waiting.call_id, output: "Process running with session ID 42" }] }, 4)
  assert.equal(JSON.parse(again.arguments).session_id, 42)
  assert.equal(next({ input: [{ type: "function_call_output", call_id: again.call_id, output: "Process exited with code 0" }] }, 5), undefined)
  command = "second"
  const second = next({}, 6)
  assert.equal(second.name, "exec_command")
  assert.throws(() => next({ input: [{ type: "function_call_output", call_id: second.call_id, output: "Process exited with code 7" }] }, 7))
})

test("performance model rejects unfinished and failed Bash results", () => {
  const next = performanceTool("claude", () => "command")
  const call = next({}, 1)
  const result = (content: string, is_error = false) => ({ messages: [{ content: [{ type: "tool_result", tool_use_id: call.id, content, is_error }] }] })
  assert.throws(() => next(result("still running"), 2), /completed workload/)
  const receipt = JSON.stringify({ kind: "edit-test", sequence: 1, operationMs: 123 })
  assert.throws(() => next(result(receipt, true), 3), /failed/)
  assert.equal(next(result(receipt), 4), undefined)
})
