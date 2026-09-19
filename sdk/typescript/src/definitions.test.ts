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
    const file = source.file("./package.json")
    const directory = source.directory("./src")
    const copied = image("source-copy")
      .copy(file, "/app/package.json")
      .copy(directory, "/app/src")
    expect(inspectImage(copied)?.steps).toEqual([
      {
        kind: "copy_source_file",
        destination: "/app/package.json",
        source: file,
      },
      {
        kind: "copy_source_directory",
        destination: "/app/src",
        source: directory,
      },
    ])
    expect(() =>
      image("forged-source").copy(
        { path: "./package.json" } as never,
        "/app/package.json",
      ),
    ).toThrow("source.file() or source.directory()")
  })

  test("rejects copy calls that do not name a source and then a destination", () => {
    const file = source.file("./package.json")
    const copy = image("copy-order").copy as (...args: unknown[]) => unknown
    const call =
      (...args: unknown[]) =>
      () =>
        copy.apply(image("copy-order"), args)
    expect(call("/app/package.json", file)).toThrow("as its first argument")
    expect(call(file)).toThrow("destination path as its second argument")
    expect(call(file, file)).toThrow("destination path as its second argument")
    expect(call(file, "/app/package.json", "/app/extra")).toThrow(
      "only a source and a destination",
    )
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
    expect(() =>
      schedules.task({
        id: "maintenance",
        cron: { pattern: "0 3 * * *", timezone: "UTC" },
        workspace: { sandbox: { id: "machine" } } as never,
        run: () => null,
      }),
    ).toThrow("Sandbox definition")
  })

  test("rejects untyped Workspace resource extensions", () => {
    const builder = sandbox({ id: "machine" }).image(
      image("root").from("debian:bookworm-slim"),
    )
    expect(() =>
      builder.resources({
        cpu: 1,
        memory: "1GiB",
        disk: "64GiB",
      } as never),
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
      expect(calls).toEqual([
        {
          target: { declaredId: "resize", payloadPresent: true },
          payload: { imageId: "image-1" },
          options,
        },
      ])
    } finally {
      uninstall()
    }
  })

})
