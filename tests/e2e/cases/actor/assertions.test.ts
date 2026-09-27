import { test } from "node:test"
import assert from "node:assert/strict"
import { assertActorRestore, assertActorTurns } from "./assertions"

const expected = { marker: "marker", nonce: "memory-only", sessionId: "session", runId: "run", computerId: "computer" }
const first = { ...expected, count: 1 }, second = { ...expected, count: 2 }
const parked = { entrypoint_kind: "actor", session_id: "session", attempt_number: 1,
  checkpoint_id: "checkpoint", prior_runtime_id: "old-runtime", prior_runtime_reclaimed: true,
  prior_runtime_state: "closed", checkpoint_status: "ready", restored_runtime_ids: ["new-runtime"] }
test("two Turns preserve original memory, count and identities", () => {
  assertActorTurns(expected, first, second)
  for (const patch of [{nonce:"restarted"}, {count:1}, {sessionId:"another"}, {runId:"new-run"}, {computerId:"other"}])
    assert.throws(() => assertActorTurns(expected, first, {...second,...patch}))
  assert.throws(() => assertActorTurns(expected, {...first,nonce:"restarted"}, second))
})
test("Actor proof rejects Task, hot VM, replay, wrong checkpoint and foreign session", () => {
  assertActorRestore(parked, parked, "session")
  for (const patch of [{entrypoint_kind:"task"}, {attempt_number:2}, {session_id:"foreign"},
    {checkpoint_id:"different"}, {prior_runtime_id:"different"}, {prior_runtime_reclaimed:false},
    {prior_runtime_state:"ready"}, {checkpoint_status:"pending"}, {restored_runtime_ids:[]}, {restored_runtime_ids:["old-runtime"]}])
    assert.throws(() => assertActorRestore(parked, {...parked,...patch}, "session"))
})
