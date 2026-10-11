import { test } from "node:test"
import assert from "node:assert/strict"
import { assertSessionTurns } from "./assertions"
import { assertRestored } from "../../support/persistence"

const expected = { marker: "marker", nonce: "memory-only", sessionId: "session", computerId: "computer" }
const first = { ...expected, count: 1 }, second = { ...expected, count: 2 }
const parked = { session_id: "session", checkpoint_id: "checkpoint", prior_runtime_id: "old-runtime",
  source_fenced: true, source_lease_epoch: 1, target_lease_epoch: null, checkpoint_status: "ready" }
const restored = { ...parked, checkpoint_status: "consumed", target_lease_epoch: 2, controls_reconciled: true,
  target_current: true, target_runtime_id: "new-runtime" }
test("two Turns preserve original memory, count and identities", () => {
  assertSessionTurns(expected, first, second)
  for (const patch of [{ nonce: "restarted" }, { count: 1 }, { sessionId: "another" }, { computerId: "other" }])
    assert.throws(() => assertSessionTurns(expected, first, { ...second, ...patch }))
  assert.throws(() => assertSessionTurns(expected, { ...first, nonce: "restarted" }, second))
})
test("restore proof rejects foreign checkpoints, unfenced sources and stale targets", () => {
  assertRestored(parked, restored, "session")
  for (const patch of [{ session_id: "foreign" }, { checkpoint_id: "different" }, { prior_runtime_id: "different" },
    { source_fenced: false }, { source_lease_epoch: 2 }, { checkpoint_status: "pending" },
    { controls_reconciled: false }, { target_current: false }, { target_runtime_id: "old-runtime" }, { target_lease_epoch: 1 }])
    assert.throws(() => assertRestored(parked, { ...restored, ...patch }, "session"))
  assert.throws(() => assertRestored({ ...parked, target_lease_epoch: 2 }, restored, "session"))
})
