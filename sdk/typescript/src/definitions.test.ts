import { describe, expect, test } from "bun:test"

import {
  inspectDefinition,
  inspectWorkspaceDefinition,
  isQueueDefinition,
} from "./internal"
import { queue, task } from "./index"

describe("private definition inspection", () => {
  test("distinguishes helpers from malformed branded values", () => {
    expect(inspectDefinition({ id: "helper" })).toBeUndefined()
    expect(isQueueDefinition({ id: "helper" })).toBe(false)
    expect(inspectWorkspaceDefinition({ id: "helper" })).toBeUndefined()

    expect(() =>
      inspectDefinition({
        [Symbol.for("helmr.sdk.v0.definition")]: { kind: "task" },
      }),
    ).toThrow("private definition")
    expect(() =>
      isQueueDefinition({
        [Symbol.for("helmr.sdk.v0.queue")]: true,
      }),
    ).toThrow("private queue")
    expect(() =>
      inspectWorkspaceDefinition({
        [Symbol.for("helmr.sdk.v0.workspace")]: true,
      }),
    ).toThrow("private workspace")
  })

  test("accepts SDK-created definitions", () => {
    const definition = task({
      id: "resize",
      run: () => null,
    })
    expect(inspectDefinition(definition)).toMatchObject({
      kind: "task",
      id: "resize",
    })
    expect(isQueueDefinition(queue({ id: "images" }))).toBe(true)
  })
})
