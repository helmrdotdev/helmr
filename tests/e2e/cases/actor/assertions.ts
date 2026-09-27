import assert from "node:assert/strict"

export function assertActorTurns(
  expected: { marker: string; nonce: string; sessionId: string; runId: string; workspaceId: string },
  first: unknown, second: unknown,
) {
  assert.deepEqual(first, { ...expected, count: 1 }, "first Actor Turn did not resume its original memory")
  assert.deepEqual(second, { ...expected, count: 2 }, "second Actor Turn lost session memory or identity")
}

export function assertActorRestore(parked: Record<string, unknown>, restored: Record<string, unknown>, sessionId: string) {
  for (const value of [parked, restored]) {
    assert.equal(value.entrypoint_kind, "actor")
    assert.equal(value.session_id, sessionId)
    assert.equal(value.attempt_number, 1, "Actor retried rather than resumed")
  }
  assert.equal(restored.checkpoint_id, parked.checkpoint_id)
  assert.equal(restored.prior_runtime_id, parked.prior_runtime_id)
  assert.equal(restored.prior_runtime_reclaimed, true)
  assert.equal(restored.prior_runtime_state, "closed")
  assert.equal(restored.checkpoint_status, "ready")
  assert(Array.isArray(restored.restored_runtime_ids) && restored.restored_runtime_ids.length > 0)
  assert(restored.restored_runtime_ids.every(id => typeof id === "string" && id !== parked.prior_runtime_id))
}
