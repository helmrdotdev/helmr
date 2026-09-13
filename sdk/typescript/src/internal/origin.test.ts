import { expect, test } from "bun:test"
import vectors from "../../../../internal/origin/testdata/origins.json"
import { canonicalSecretOrigin } from "./origin"
for (const vector of vectors) test(`Secret origin: ${vector.input}`, () => {
 if (vector.canonical) expect(canonicalSecretOrigin(vector.input)).toBe(vector.canonical)
 else expect(() => canonicalSecretOrigin(vector.input)).toThrow()
})
