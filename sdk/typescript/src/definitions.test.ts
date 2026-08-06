import { describe, expect, test } from "bun:test"

import {
  inspectDefinition,
  inspectImage,
  inspectSandboxDefinition,
  inspectWorkspaceAddress,
  installRuntimeOperations,
  isQueue,
} from "./internal"
import {
  actor,
  image,
  queue,
  sandbox,
  schedules,
  sessions,
  source,
  task,
  workspaces,
} from "./index"

describe("private definition inspection", () => {
  test("distinguishes helpers from malformed branded values", () => {
    expect(inspectDefinition({ id: "helper" })).toBeUndefined()
    expect(isQueue({ name: "helper" })).toBe(false)
    expect(inspectSandboxDefinition({ id: "helper" })).toBeUndefined()

    expect(() =>
      inspectDefinition({
        [Symbol.for("helmr.sdk.v0.definition")]: { kind: "task" },
      }),
    ).toThrow("private definition")
    expect(() =>
      isQueue({
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
    expect(isQueue(queue({ name: "images" }))).toBe(true)
  })

  test("accepts only SDK-created source selectors", () => {
    const copied = image("source-copy")
      .copy("/app/package.json", source.file("./package.json"))
      .copy("/app/src", source.directory("./src"))
    expect(inspectImage(copied)?.steps).toHaveLength(2)
    expect(() =>
      image("forged-source").copy(
        "/app/package.json",
        { path: "./package.json" } as never,
      )
    ).toThrow("source.file() or source.directory()")
  })

  test("constructs branded Workspace refs", () => {
    const ref = workspaces.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32")

    expect(inspectWorkspaceAddress(ref)).toEqual({
      id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
    })
    expect(inspectWorkspaceAddress({ key: "machine" })).toBeUndefined()
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
      actorInputSend: async (sessionId, input, request, signal) => {
        calls.push({ sessionId, input, request, signal })
        return {
          id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34",
          sequence: 7,
          data: input,
          source: { type: "external" },
          createdAt: "2026-07-26T00:00:00Z",
        }
      },
    })
    try {
      const ref = sessions.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33")
      await expect(ref.input.send(
        { message: "hello" },
        { idempotencyKey: "send-1" },
      )).resolves.toEqual({
        id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34",
        sequence: 7,
        data: { message: "hello" },
        source: { type: "external" },
        createdAt: "2026-07-26T00:00:00Z",
      })
      expect(ref.id).toBe("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33")
    } finally {
      uninstall()
    }
    expect(calls).toEqual([
      {
        sessionId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
        input: { message: "hello" },
        request: { idempotencyKey: "send-1" },
        signal: undefined,
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
      sessionRetrieve: async (sessionId, signal) => {
        calls.push({ operation: "retrieve", sessionId, signal })
        return {
          id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
          actorId: "mailbox",
          deploymentId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
          status: "open",
          createdAt: "2026-07-26T00:00:00Z",
          updatedAt: "2026-07-26T00:00:01Z",
        }
      },
      sessionClose: async (sessionId, request, signal) => {
        calls.push({ operation: "close", sessionId, request, signal })
        return {
          sessionId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
          acceptedAt: "2026-07-26T00:00:02Z",
        }
      },
      sessionOutputPage: async (sessionId, query, signal) => {
        calls.push({ operation: "output", sessionId, query, signal })
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
      const retrieveController = new AbortController()
      await ref.retrieve({ signal: retrieveController.signal })
      await ref.close({ idempotencyKey: "close-1" })
      const output = await ref.output.list({ limit: 1 })
      expect(output.records).toHaveLength(1)
      expect(output.nextAfter).toBe(1)
      expect(output.hasMore).toBe(true)
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

  test("passes Actor output cancellation to the finite page read", async () => {
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
      await ref.output.list({}, { signal: controller.signal })
      expect(requests).toBe(1)
    } finally {
      uninstall()
    }
  })
})
