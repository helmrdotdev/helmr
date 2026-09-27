import { expect, test } from "bun:test"
import { deadline } from "./deadline"
import { abortableDelay } from "../../../sdk/typescript/src/internal/abort"

test("deadline survives listener removal between polling requests", async () => {
  const signal = deadline(30)
  await abortableDelay(5, signal)
  await expect(abortableDelay(1000, signal)).rejects.toThrow("Verification deadline exceeded")
  expect(signal.aborted).toBe(true)
})
