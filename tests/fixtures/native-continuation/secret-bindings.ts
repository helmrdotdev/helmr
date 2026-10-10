import assert from "node:assert/strict"
import { createHash } from "node:crypto"
import { readFile } from "node:fs/promises"
import type { Json } from "../../../sdk/typescript/src/agent"

export interface SecretBindingState {
  rawDigest: string
  fileDigest: string
  protectedDigest: string
  marker: string
  checks: number
}

// Keep the admitted values and selector in the actual setup heap. Rotation must
// not rewrite this process or the values retained by a healthy RAM continuation.
export async function checkSecretBindings(input: Json, prior?: SecretBindingState): Promise<SecretBindingState> {
  const digest = (value: string) => createHash("sha256").update(value).digest("hex")
  const raw = process.env.NATIVE_RAW_SECRET
  const marker = process.env.NATIVE_PROTECTED_SECRET
  assert(typeof raw === "string" && typeof marker === "string", "Secret environment bindings are absent")
  assert(/^hlmr_protected_[a-f0-9]{64}$/.test(marker), "protected binding is not a selector")
  let state = prior
  if (!state) {
    assert(input && typeof input === "object" && !Array.isArray(input))
    const { rawDigest, fileDigest, protectedDigest } = input as Readonly<Record<string, Json>>
    assert(typeof rawDigest === "string" && typeof fileDigest === "string" && typeof protectedDigest === "string")
    for (const value of [rawDigest, fileDigest, protectedDigest]) assert(typeof value === "string" && /^[a-f0-9]{64}$/.test(value))
    state = { rawDigest, fileDigest, protectedDigest, marker, checks: 0 }
  }
  assert.equal(digest(raw), state.rawDigest, "raw Secret changed")
  assert.equal(digest(await readFile("/run/native-proof/secret", "utf8")), state.fileDigest, "file Secret changed")
  assert.notEqual(digest(marker), state.protectedDigest, "protected Secret was delivered as plaintext")
  assert(marker === state.marker, "protected selector changed after restore")
  state.checks++
  return state
}
