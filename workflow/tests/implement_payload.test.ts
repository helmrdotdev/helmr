import { describe, expect, test } from "bun:test"

import { lightPayload, payload } from "../tasks/integrations/types"

describe("implementation payload schema", () => {
  test("implement payload rejects unknown fields", () => {
    expect(
      payload.safeParse({
        featureDesign: "Add context metadata",
        repository: "helmrdotdev/helmr",
      }).success,
    ).toBe(false)
  })

  test("light implement payload rejects unsupported fields", () => {
    expect(
      lightPayload.safeParse({
        featureDesign: "Fix small typo",
        operatorInput: true,
      }).success,
    ).toBe(false)
  })
})
