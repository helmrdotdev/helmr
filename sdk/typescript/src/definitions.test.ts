import { describe, expect, test } from "bun:test"

import {
  inspectDefinition,
  inspectWorkspaceDefinition,
  installRuntimeOperations,
  isQueueDefinition,
} from "./internal"
import { actor, queue, task } from "./index"

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

  test("captures the Actor declared ID and exact address for input send", async () => {
    const calls: unknown[] = []
    const uninstall = installRuntimeOperations({
      waitFor: async () => {},
      waitUntil: async () => {},
      actorInputSend: async (target, input, options) => {
        calls.push({ target, input, options })
        return { sequence: 7 }
      },
    })
    try {
      const config = { id: "mailbox", run() {} }
      const definition = actor(config)
      config.id = "mutated"
      const idAddress = { id: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa" }
      const idRef = definition.ref(idAddress)
      idAddress.id = "act_invalid"
      await expect(idRef.input.send(
        { message: "hello" },
        { idempotencyKey: "send-1" },
      )).resolves.toEqual({ sequence: 7 })
      const keyAddress = { key: "primary" }
      const keyRef = definition.ref(keyAddress)
      keyAddress.key = "mutated"
      await keyRef.input.send(null)
      expect(idRef.id).toBe("act_aaaaaaaaaaaaaaaaaaaaaaaaaa")
      expect(keyRef.key).toBe("primary")
    } finally {
      uninstall()
    }
    expect(calls).toEqual([
      {
        target: {
          declaredId: "mailbox",
          address: { id: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa" },
        },
        input: { message: "hello" },
        options: { idempotencyKey: "send-1" },
      },
      {
        target: { declaredId: "mailbox", address: { key: "primary" } },
        input: null,
        options: undefined,
      },
    ])
  })

  test("rejects malformed Actor ref addresses before runtime dispatch", () => {
    const definition = actor({ id: "mailbox", run() {} })
    expect(() => definition.ref({ id: "act_not-canonical" })).toThrow(
      "canonical Actor public ID",
    )
    for (const key of [
      "",
      " leading",
      "trailing ",
      "\u0085leading",
      "embedded\0nul",
      "a".repeat(513),
    ]) {
      expect(() => definition.ref({ key })).toThrow("actor ref key")
    }
    expect(() => definition.ref({
      id: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
      key: "primary",
    } as never)).toThrow("exactly one")
    expect(definition.ref({ key: "\ufeffprimary" }).key).toBe(
      "\ufeffprimary",
    )
  })
})
