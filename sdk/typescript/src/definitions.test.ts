import { describe, expect, test } from "bun:test"

import {
  inspectDefinition,
  inspectWorkspaceDefinition,
  installRuntimeOperations,
  isQueueDefinition,
} from "./internal"
import { actor, image, queue, task, workspace, workspaces } from "./index"

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

  test("rejects untyped Workspace resource extensions", () => {
    const builder = workspace("machine")
      .image(image("root").from("debian:bookworm-slim"))
    expect(() =>
      builder.resources({
        cpu: 1,
        memory: "1GiB",
        disk: "64GiB",
      } as never)
    ).toThrow("only cpu and memory")
  })

  test("delegates task.call through a TaskWait facade", async () => {
    const calls: unknown[] = []
    const uninstall = installRuntimeOperations({
      taskCall: async (target, payload, options) => {
        calls.push({ target, payload, options })
        return {
          ok: true,
          output: { resized: true },
          run: { id: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa" },
        }
      },
    } as never)
    try {
      const child = task({
        id: "resize",
        payload: {
          "~standard": {
            version: 1,
            vendor: "test",
            validate: (value: unknown) => ({ value }),
          },
        },
        run: () => ({ resized: true }),
      })
      const options = {
        workspace: workspaces.ref({ key: "images" }),
        idempotencyKey: "resize:image-1",
      }
      const wait = child.call({ imageId: "image-1" }, options)
      expect(await wait).toEqual({
        ok: true,
        output: { resized: true },
        run: { id: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa" },
      })
      await expect(wait.unwrap()).resolves.toEqual({ resized: true })
      expect(calls).toEqual([{
        target: { declaredId: "resize", payloadPresent: true },
        payload: { imageId: "image-1" },
        options,
      }])
    } finally {
      uninstall()
    }
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

  test("delegates Actor lifecycle and finite output pages to the runtime", async () => {
    const calls: unknown[] = []
    let page = 0
    const uninstall = installRuntimeOperations({
      actorStart: async (declaredId, options) => {
        calls.push({ operation: "start", declaredId, options })
        return {
          actorId: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
          runId: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa",
        }
      },
      actorStatus: async (target) => {
        calls.push({ operation: "status", target })
        return {
          id: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
          status: "open",
          createdAt: new Date("2026-07-26T00:00:00Z"),
          updatedAt: new Date("2026-07-26T00:00:01Z"),
        }
      },
      actorClose: async (target, options) => {
        calls.push({ operation: "close", target, options })
        return {
          actorId: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
          acceptedAt: new Date("2026-07-26T00:00:02Z"),
        }
      },
      actorOutputPage: async (target, options) => {
        calls.push({ operation: "output", target, options })
        page++
        return page === 1
          ? {
              records: [{
                id: "acr_aaaaaaaaaaaaaaaaaaaaaaaaaa",
                sequence: 1,
                data: null,
                contentType: "application/json",
                createdAt: "2026-07-26T00:00:00Z",
                provenance: {
                  runId: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa",
                  attemptNumber: 1,
                  deploymentId: "dep_aaaaaaaaaaaaaaaaaaaaaaaaaa",
                },
              }],
              nextAfter: 1,
              hasMore: true,
            }
          : { records: [], nextAfter: 1, hasMore: false }
      },
    } as never)
    try {
      const definition = actor({ id: "mailbox", run() {} })
      const omitted = {
        workspace: workspaces.ref({ key: "actor-workspace" }),
      }
      const started = await definition.start(omitted)
      expect(started.ref.id).toBe("act_aaaaaaaaaaaaaaaaaaaaaaaaaa")
      expect(started.run.id).toBe("run_aaaaaaaaaaaaaaaaaaaaaaaaaa")
      await definition.start({
        workspace: workspaces.ref({ key: "actor-workspace" }),
        input: null,
      })
      const ref = definition.ref({ id: started.ref.id })
      await ref.status()
      await ref.close({ idempotencyKey: "close-1" })
      expect(await ref.output.list({ limit: 1 })).toHaveLength(1)
      page = 0
      const records = []
      for await (const record of ref.output.read({ limit: 1 })) {
        records.push(record)
      }
      expect(records).toHaveLength(1)
      const starts = calls.filter((call) =>
        (call as { operation?: string }).operation === "start"
      ) as Array<{ options: Record<string, unknown> }>
      expect(Object.hasOwn(starts[0]!.options, "input")).toBe(false)
      expect(Object.hasOwn(starts[1]!.options, "input")).toBe(true)
      expect(starts[1]!.options["input"]).toBeNull()
    } finally {
      uninstall()
    }
  })

  test("aborts Actor output reads only at the caller boundary", async () => {
    let requests = 0
    const controller = new AbortController()
    const uninstall = installRuntimeOperations({
      actorOutputPage: async () => {
        requests++
        return { records: [], nextAfter: 0, hasMore: false }
      },
    } as never)
    try {
      const ref = actor({ id: "mailbox", run() {} }).ref({
        key: "primary",
      })
      controller.abort(new Error("stop output"))
      const iterator = ref.output.read({ signal: controller.signal })
      await expect(iterator[Symbol.asyncIterator]().next()).rejects.toThrow(
        "stop output",
      )
      expect(requests).toBe(0)
    } finally {
      uninstall()
    }
  })
})
