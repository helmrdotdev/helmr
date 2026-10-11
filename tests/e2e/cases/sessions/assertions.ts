import assert from "node:assert/strict"

export function assertSessionTurns(
  expected: { marker: string; nonce: string; sessionId: string; computerId: string }, first: unknown, second: unknown,
) {
  assert.deepEqual(first, { ...expected, count: 1 }, "first Turn did not resume its original memory")
  assert.deepEqual(second, { ...expected, count: 2 }, "second Turn lost Session memory or identity")
}
