import assert from "node:assert/strict"
import { test } from "node:test"
import { performanceTool } from "./performance-model"

test("performance model follows the exact yielded native command until exit", () => {
  let command = "first"
  const next = performanceTool("codex", () => ({ command, kind: "no-op" }))
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
  const next = performanceTool("claude", () => ({ command: "command", kind: "edit-test" }))
  const call = next({}, 1)
  const result = (content: string, is_error = false) => ({ messages: [{ content: [{ type: "tool_result", tool_use_id: call.id, content, is_error }] }] })
  assert.throws(() => next(result("still running"), 2), /completed workload/)
  const receipt = JSON.stringify({ kind: "edit-test", sequence: 1, operationMs: 123 })
  assert.throws(() => next(result(receipt, true), 3), /failed/)
  assert.equal(next(result(receipt), 4), undefined)
})

function observed(provider: "codex" | "claude", kind: "no-op" | "edit-test" | "dependencies") {
  let elapsed = 0
  const records: Record<string, unknown>[] = []
  const next = performanceTool(provider, () => ({ command: "command", kind }), { now: () => elapsed, record: value => records.push(value) })
  return { next, records, at: (value: number) => { elapsed = value } }
}
const codexResult = (call: any, output: string) => ({ input: [{ type: "function_call_output", call_id: call.call_id, output }] })
const bashResult = (call: any, content: string, is_error = false) => ({ messages: [{ content: [{ type: "tool_result", tool_use_id: call.id, content, is_error }] }] })

test("dependency observation retains the target miss and exact eventual completion", () => {
  const { next, records, at } = observed("codex", "dependencies")
  const first = next({}, 1)
  at(120000)
  const waiting = next(codexResult(first, "Process running with session ID 42"), 2)
  assert.equal(records[0]!.targetMiss, true)
  assert.equal(records[0]!.resultState, "running")
  assert.equal(records[0]!.callId, first.call_id)
  assert.equal(records[0]!.processId, 42)
  at(299999)
  assert.equal(next(codexResult(waiting, "Process exited with code 0"), 3), undefined)
  assert.equal(records[1]!.elapsedMS, 299999)
  assert.equal(records[1]!.targetMiss, true)
  assert.equal(records[1]!.exitCode, 0)
})

for (const result of ["Process running with session ID 42", "Process exited with code 0"]) {
  test(`dependency observation rejects the parsed result at its fixed bound: ${result}`, () => {
    const { next, records, at } = observed("codex", "dependencies")
    const first = next({}, 1)
    at(120001)
    const waiting = next(codexResult(first, "Process running with session ID 42"), 2)
    at(300000)
    assert.throws(() => next(codexResult(waiting, result), 3), /observation budget/)
    assert.equal(records.at(-1)!.elapsedMS, 300000)
    assert.equal(records.at(-1)!.observationBudgetExceeded, true)
    assert.equal(records.at(-1)!.resultState, result.includes("running") ? "running" : "exited")
  })
}

for (const kind of ["no-op", "edit-test"] as const) {
  test(`${kind} keeps its original command and observation bounds`, () => {
    const { next, records, at } = observed("codex", kind)
    const first = next({}, 1)
    at(120000)
    assert.throws(() => next(codexResult(first, "Process exited with code 0"), 2), /observation budget/)
    assert.equal(records[0]!.exitCode, 0)
    assert.equal(records[0]!.observationBudgetMS, 120000)
    assert.equal(observed("claude", kind).next({}, 1).input.timeout, 120000)
  })
}

test("late results never hide failures, stale calls or changed process identity", () => {
  for (const invalid of ["nonzero", "stale", "process", "malformed"]) {
    const { next, records, at } = observed("codex", "dependencies")
    const first = next({}, 1)
    const waiting = next(codexResult(first, "Process running with session ID 42"), 2)
    at(130000)
    const result = invalid === "nonzero" ? "Process exited with code 7" : invalid === "process" ? "Process running with session ID 43" : invalid === "malformed" ? "unknown" : "Process exited with code 0"
    assert.throws(() => next(codexResult(invalid === "stale" ? first : waiting, result), 3))
    assert.equal(records.at(-1)!.targetMiss, true)
    assert.equal(records.at(-1)!.resultState, invalid === "nonzero" ? "exited" : "invalid")
  }
})

test("dependency Bash changes only its fixture timeout and requires an exact timely receipt", () => {
  const receipt = JSON.stringify({ kind: "dependencies", sequence: 1, operationMs: 130000 })
  for (const outcome of ["success", "error", "missing", "stale", "bound"]) {
    const { next, records, at } = observed("claude", "dependencies")
    const first = next({}, 1)
    assert.equal(first.input.timeout, 300000)
    at(outcome === "bound" ? 300000 : 130000)
    const body = bashResult(outcome === "stale" ? { id: "wrong" } : first, outcome === "missing" ? "still running" : receipt, outcome === "error")
    if (outcome === "success") assert.equal(next(body, 2), undefined)
    else assert.throws(() => next(body, 2))
    assert.equal(records.at(-1)!.targetMiss, true)
    if (outcome === "bound") assert.equal(records.at(-1)!.observationBudgetExceeded, true)
  }
})
