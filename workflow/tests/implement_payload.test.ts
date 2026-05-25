import { describe, expect, test } from "bun:test"

import { normalizePayload } from "../tasks/implement/types"
import { normalizeLightPayload } from "../tasks/light-implement"

describe("implementation payload normalization", () => {
  test("implement rejects unknown payload fields", () => {
    expect(() =>
      normalizePayload({
        featureDesign: "Add context metadata",
        repository: "helmrdotdev/helmr",
      } as never),
    ).toThrow("payload.repository is not supported")
  })

  test("light implement rejects fields it ignores", () => {
    expect(() =>
      normalizeLightPayload({
        featureDesign: "Fix small typo",
        operatorInput: true,
      } as never),
    ).toThrow("payload.operatorInput is not supported by light-implement")
  })
})
