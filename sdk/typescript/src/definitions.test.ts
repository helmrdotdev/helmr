import { describe, expect, test } from "bun:test"

import {
  inspectDefinition,
  inspectSandboxDefinition,
  inspectWorkspaceAddress,
  installRuntimeOperations,
  isQueueDefinition,
} from "./internal"
import {
  actor,
  image,
  queue,
  sandbox,
  schedules,
  sessions,
  task,
  workspaces,
} from "./index"

describe("private definition inspection", () => {
  test("distinguishes helpers from malformed branded values", () => {
    expect(inspectDefinition({ id: "helper" })).toBeUndefined()
    expect(isQueueDefinition({ name: "helper" })).toBe(false)
    expect(inspectSandboxDefinition({ id: "helper" })).toBeUndefined()

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
      inspectSandboxDefinition({
        [Symbol.for("helmr.sdk.v0.sandbox")]: true,
      }),
    ).toThrow("private Sandbox")
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
    expect(isQueueDefinition(queue({ name: "images" }))).toBe(true)
  })

  test("constructs branded Workspace addresses without operations", () => {
    const id = workspaces.fromId("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32")
    const key = workspaces.fromKey("machine")
    const ref = workspaces.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32")

    expect(inspectWorkspaceAddress(id)).toEqual({
      id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
    })
    expect(inspectWorkspaceAddress(key)).toEqual({ key: "machine" })
    expect(inspectWorkspaceAddress(ref)?.id).toBe(id.id)
    expect(inspectWorkspaceAddress({ key: "machine" })).toBeUndefined()
    expect(Object.isFrozen(id)).toBe(true)
    expect(Object.isFrozen(key)).toBe(true)
    expect(Object.isFrozen(ref)).toBe(true)
  })

  test("rejects a forged Schedule Sandbox definition", () => {
    expect(() => schedules.task({
      id: "maintenance",
      cron: { pattern: "0 3 * * *", timezone: "UTC" },
      workspace: { sandbox: { id: "machine" } } as never,
      run: () => null,
    })).toThrow("Sandbox definition")
  })

  test("rejects untyped Workspace resource extensions", () => {
    const builder = sandbox({ id: "machine" })
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
          run: { id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" },
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
        workspace: workspaces.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"),
        idempotencyKey: "resize:image-1",
      }
      const wait = child.call({ imageId: "image-1" }, options)
      expect(await wait).toEqual({
        ok: true,
        output: { resized: true },
        run: { id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" },
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

  test("addresses Session input only by UUID", async () => {
    const calls: unknown[] = []
    const uninstall = installRuntimeOperations({
      waitFor: async () => {},
      waitUntil: async () => {},
      actorInputSend: async (sessionId, input, options) => {
        calls.push({ sessionId, input, options })
        return { sequence: 7 }
      },
    })
    try {
      const ref = sessions.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33")
      await expect(ref.input.send(
        { message: "hello" },
        { idempotencyKey: "send-1" },
      )).resolves.toEqual({ sequence: 7 })
      expect(ref.id).toBe("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33")
    } finally {
      uninstall()
    }
    expect(calls).toEqual([
      {
        sessionId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
        input: { message: "hello" },
        options: { idempotencyKey: "send-1" },
      },
    ])
  })

  test("rejects malformed Session IDs before runtime dispatch", () => {
    expect(() => sessions.ref("act_not-canonical")).toThrow(
      "Session ID must be a canonical UUIDv7",
    )
  })

  test("delegates Actor lifecycle and finite output pages to the runtime", async () => {
    const calls: unknown[] = []
    let page = 0
    const uninstall = installRuntimeOperations({
      actorStart: async (declaredId, options) => {
        calls.push({ operation: "start", declaredId, options })
        return {
          sessionId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
          runId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
        }
      },
      sessionStatus: async (sessionId) => {
        calls.push({ operation: "status", sessionId })
        return {
          id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
          status: "open",
          createdAt: new Date("2026-07-26T00:00:00Z"),
          updatedAt: new Date("2026-07-26T00:00:01Z"),
        }
      },
      sessionClose: async (sessionId, options) => {
        calls.push({ operation: "close", sessionId, options })
        return {
          sessionId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
          acceptedAt: new Date("2026-07-26T00:00:02Z"),
        }
      },
      sessionOutputPage: async (sessionId, options) => {
        calls.push({ operation: "output", sessionId, options })
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
                  runId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
                  attemptNumber: 1,
                  deploymentId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
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
        workspace: workspaces.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"),
      }
      const started = await definition.start(omitted)
      expect(started.session.id).toBe("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33")
      expect(started.run.id).toBe("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31")
      await definition.start({
        workspace: workspaces.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"),
        input: null,
      })
      const ref = started.session
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
      sessionOutputPage: async () => {
        requests++
        return { records: [], nextAfter: 0, hasMore: false }
      },
    } as never)
    try {
      const ref = sessions.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33")
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
