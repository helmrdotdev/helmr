import { describe, expect, test } from "bun:test"

import {
  actor,
  image,
  queue,
  schedules,
  secrets,
  source,
  task,
  workspace,
  workspaces,
  type JsonValue,
  type PayloadSchema,
} from "@helmr/sdk"
import {
  PROGRAM_ENTRYPOINT,
  analyze,
  normalizeWorkspaceResources,
} from "./compile"
import {
  encodeVerificationResultFrame,
  failedVerificationResult,
  successfulVerificationResult,
} from "./analysis"

const identitySchema: PayloadSchema<JsonValue> = {
  "~standard": {
    version: 1,
    vendor: "test",
    validate(value) {
      return { value: value as JsonValue }
    },
  },
}

describe("declaration analysis", () => {
  test("emits one deterministic BuildPlan and declaration locator", () => {
    const jobs = queue({ id: "jobs", concurrencyLimit: 3 })
    const payloadTask = task({
      id: "constructor",
      payload: identitySchema,
      queue: jobs,
      maxDuration: "60s",
      ttl: "1500ms",
      retry: { enabled: true, maxAttempts: 2 },
      run: (payload) => payload,
    })
    const noPayloadTask = task({
      id: "toString",
      run: () => ({ ok: true }),
    })
    const service = actor({
      id: "service",
      idleTimeout: "1500ms",
      run: async () => {},
    })
    const machine = workspace("machine")
      .image(image("root").from("debian:bookworm"))
      .resources({ cpu: 0.125, memory: "1024MiB" })
    const exports = [
      { modulePath: "src/machine.ts", exportName: "machine", value: machine },
      { modulePath: "src/tasks.ts", exportName: "toString", value: noPayloadTask },
      { modulePath: "src/actor.ts", exportName: "service", value: service },
      { modulePath: "src/tasks.ts", exportName: "constructor", value: payloadTask },
      { modulePath: "src/queues.ts", exportName: "jobs", value: jobs },
    ] as const

    const result = analyze({ architecture: "x86_64", exports })
    const reversed = analyze({
      architecture: "x86_64",
      exports: [...exports].reverse(),
    })

    expect(result.buildPlanBytes).toEqual(reversed.buildPlanBytes)
    expect(result.declarationLocatorBytes).toEqual(
      reversed.declarationLocatorBytes,
    )
    expect(result.buildPlan.definitions.map((item) => item.kind)).toEqual([
      "task",
      "task",
      "actor",
      "workspace",
    ])
    expect(result.buildPlan.queues).toEqual([
      { name: "actor/service" },
      { name: "jobs", concurrencyLimit: 3 },
      { name: "task/toString" },
    ])
    expect(result.declarationLocator.declarations).toEqual([
      {
        declaredId: "constructor",
        exportName: "constructor",
        kind: "task",
        modulePath: "src/tasks.ts",
        slot: "handler",
      },
      {
        declaredId: "toString",
        exportName: "toString",
        kind: "task",
        modulePath: "src/tasks.ts",
        slot: "handler",
      },
      {
        declaredId: "service",
        exportName: "service",
        kind: "actor",
        modulePath: "src/actor.ts",
        slot: "handler",
      },
    ])
    const workspaceDefinition = result.buildPlan.definitions[3]
    expect(workspaceDefinition?.kind).toBe("workspace")
    if (workspaceDefinition?.kind !== "workspace") throw new Error("workspace missing")
    expect(workspaceDefinition.manifest.resources).toEqual({
      milliCpu: 125,
      memoryMiB: 1024,
    })
    expect(new TextDecoder().decode(result.entrypointBytes)).toBe(
      PROGRAM_ENTRYPOINT,
    )
    expect(JSON.stringify(result.buildPlan)).not.toContain("registry")
  })

  test("accepts the same declared ID in distinct kind namespaces", () => {
    const sharedTask = task({ id: "shared", run: () => null })
    const sharedActor = actor({ id: "shared", run: async () => {} })
    expect(
      analyze({
        architecture: "x86_64",
        exports: [
          { modulePath: "src/task.ts", exportName: "sharedTask", value: sharedTask },
          { modulePath: "src/actor.ts", exportName: "sharedActor", value: sharedActor },
        ],
      }).buildPlan.definitions,
    ).toHaveLength(2)
  })

  test("rejects duplicate same-kind declarations", () => {
    const first = task({ id: "duplicate", run: () => null })
    const second = task({ id: "duplicate", run: () => null })
    expect(() =>
      analyze({
        architecture: "x86_64",
        exports: [
          { modulePath: "src/a.ts", exportName: "first", value: first },
          { modulePath: "src/b.ts", exportName: "second", value: second },
        ],
      }),
    ).toThrow("duplicate task declaration")
  })

  test("deduplicates one Queue object and rejects distinct objects with the same ID", () => {
    const shared = queue({ id: "shared", concurrencyLimit: 2 })
    expect(() =>
      analyze({
        architecture: "x86_64",
        exports: [
          { modulePath: "src/queue.ts", exportName: "shared", value: shared },
          {
            modulePath: "src/task.ts",
            exportName: "usesShared",
            value: task({
              id: "uses-shared",
              queue: shared,
              run: () => null,
            }),
          },
        ],
      }),
    ).not.toThrow()

    const first = queue({ id: "duplicate", concurrencyLimit: 2 })
    const second = queue({ id: "duplicate", concurrencyLimit: 2 })
    expect(() =>
      analyze({
        architecture: "x86_64",
        exports: [
          { modulePath: "src/a.ts", exportName: "first", value: first },
          { modulePath: "src/b.ts", exportName: "second", value: second },
          {
            modulePath: "src/task.ts",
            exportName: "task",
            value: task({ id: "task", queue: first, run: () => null }),
          },
        ],
      }),
    ).toThrow("duplicate queue declaration")
  })

  test("deduplicates re-exports and selects the smallest locator", () => {
    const definition = task({
      id: "shared",
      payload: identitySchema,
      run: (payload) => payload,
    })
    const result = analyze({
      architecture: "x86_64",
      exports: [
        {
          modulePath: "src/z-barrel.ts",
          exportName: "shared",
          value: definition,
        },
        {
          modulePath: "src/a-direct.ts",
          exportName: "renamed",
          value: definition,
        },
      ],
    })

    expect(result.buildPlan.definitions).toHaveLength(1)
    expect(result.declarationLocator.declarations).toEqual([
      {
        declaredId: "shared",
        exportName: "renamed",
        kind: "task",
        modulePath: "src/a-direct.ts",
        slot: "handler",
      },
    ])
    expect(result.programDeclarations[0]?.slots).toEqual([
      "handler",
      "payloadSchema",
    ])
  })

  test("rejects invalid locator text before canonicalization", () => {
    const definition = task({ id: "located", run: () => null })
    for (const item of [
      { modulePath: "src/\ud800.ts", exportName: "located" },
      { modulePath: "src/located.ts", exportName: "\udc00" },
      { modulePath: "node_modules/pkg/task.ts", exportName: "located" },
      { modulePath: "src/task.d.ts", exportName: "located" },
    ]) {
      expect(() =>
        analyze({
          architecture: "x86_64",
          exports: [{ ...item, value: definition }],
        }),
      ).toThrow()
    }
  })

  test("normalizes resources exactly and rejects rounding or aliases", () => {
    expect(
      normalizeWorkspaceResources({
        cpu: 1e-3,
        memory: "1GiB",
      }),
    ).toEqual({
      milliCpu: 1,
      memoryMiB: 1024,
    })

    for (const cpu of [0, -1, Number.NaN, Number.POSITIVE_INFINITY, 0.0001]) {
      expect(() =>
        normalizeWorkspaceResources({
          cpu,
          memory: "1MiB",
        }),
      ).toThrow()
    }
    for (const memory of ["01MiB", "1GB", "1.5GiB", " 1GiB", "+1MiB"]) {
      expect(() =>
        normalizeWorkspaceResources({
          cpu: 1,
          memory,
        }),
      ).toThrow()
    }
  })

  test("emits source copies without caller-provided integrity fields", () => {
    const machine = workspace("source-copy")
      .image(
        image("root")
          .from("debian:bookworm")
          .copy("/app/package.json", source.file("package.json"))
          .copy("/app/src", source.directory("src")),
      )
      .resources({ cpu: 1, memory: "1GiB" })
    const result = analyze({
      architecture: "x86_64",
      exports: [{
        modulePath: "src/workspace.ts",
        exportName: "machine",
        value: machine,
      }],
    })
    const definition = result.buildPlan.definitions[0]
    expect(definition?.kind).toBe("workspace")
    if (definition?.kind !== "workspace") throw new Error("workspace missing")
    expect(definition.manifest.imageBuild.images[0]?.steps.slice(1)).toEqual([
      {
        copySourceFile: {
          dst: "/app/package.json",
          path: "package.json",
        },
      },
      {
        copySourceDir: {
          dst: "/app/src",
          path: "src",
        },
      },
    ])
  })

  test("emits closed registry authentication without credential bytes", () => {
    const machine = workspace("private-base")
      .image(image("root").from("ghcr.io/acme/base:1", {
        auth: {
          username: "aktky",
          password: secrets.fromName("GHCR_TOKEN"),
        },
      }))
      .resources({ cpu: 1, memory: "1GiB" })
    const result = analyze({
      architecture: "x86_64",
      exports: [{
        modulePath: "src/workspace.ts",
        exportName: "machine",
        value: machine,
      }],
    })
    const definition = result.buildPlan.definitions[0]
    expect(definition?.kind).toBe("workspace")
    if (definition?.kind !== "workspace") throw new Error("workspace missing")
    expect(definition.manifest.imageBuild.images[0]?.steps[0]).toEqual({
      from: {
        ref: "ghcr.io/acme/base:1",
        auth: {
          username: "aktky",
          passwordSecret: "GHCR_TOKEN",
        },
      },
    })
    expect(new TextDecoder().decode(result.buildPlanBytes)).not.toContain(
      "credential-bytes",
    )
  })

  test("rejects image steps with unknown members", () => {
    const root = image("root").from("debian:bookworm")
    const run = root.run as (...args: readonly unknown[]) => unknown
    expect(() =>
      run.call(root, ["true"], { ignored: true }),
    ).toThrow("image.run() accepts only argv")

    const forged = root.run(["true"]) as unknown as {
      readonly steps: readonly Record<string, unknown>[]
    }
    Object.defineProperty(
      forged.steps[1] as Record<string, unknown>,
      "unknown",
      { value: true, enumerable: true },
    )
    expect(() =>
      analyze({
        architecture: "x86_64",
        exports: [{
          modulePath: "src/workspace.ts",
          exportName: "machine",
          value: workspace("machine")
            .image(forged as never)
            .resources({ cpu: 1, memory: "1GiB" }),
        }],
      }),
    ).toThrow("image run step has unknown members")
  })

  test("normalizes scheduler-owned payload and declarative workspace", () => {
    const scheduled = schedules.task({
      id: "nightly",
      cron: { pattern: "0 3 * * *", timezone: "UTC" },
      workspace: workspaces.ref({ key: "maintenance" }),
      run: () => null,
    })
    const result = analyze({
      architecture: "x86_64",
      exports: [
        {
          modulePath: "src/schedules.ts",
          exportName: "nightly",
          value: scheduled,
        },
      ],
    })
    const definition = result.buildPlan.definitions[0]
    expect(definition?.kind).toBe("task")
    if (definition?.kind !== "task") throw new Error("task missing")
    expect(definition.manifest.schedule).toEqual({
      cron: "0 3 * * *",
      timezone: "UTC",
      workspace: { key: "maintenance" },
    })
    expect(definition.manifest.payload).toEqual({
      kind: "standard_schema",
    })
  })

  test("enforces the closed Duration grammar and bounds", () => {
    for (const maxDuration of ["0s", "01s", "1.5s", "1h30m", " 5s", "25h"]) {
      const definition = task({
        id: "duration",
        maxDuration,
        run: () => null,
      })
      expect(() =>
        analyze({
          architecture: "x86_64",
          exports: [{
            modulePath: "src/task.ts",
            exportName: "task",
            value: definition,
          }],
        }),
      ).toThrow()
    }
    const definition = task({
      id: "retry",
      retry: {
        maxAttempts: 2,
        backoff: { minDelay: "2s", maxDelay: "1s", factor: 1.5 },
      },
      run: () => null,
    })
    expect(() =>
      analyze({
        architecture: "x86_64",
        exports: [{
          modulePath: "src/task.ts",
          exportName: "task",
          value: definition,
        }],
      }),
    ).toThrow()
  })

  test("leaves cron grammar authority to Control", () => {
    for (const [index, pattern] of [
      "*/15 0-23/2 1,15 * 0-7",
      "0  3 * * *",
      "00 3 * JAN MON",
      "@daily",
    ].entries()) {
      expect(() =>
        schedules.task({
          id: `valid-${index}`,
          cron: { pattern, timezone: "UTC" },
          workspace: workspaces.ref({ key: "maintenance" }),
          run: () => null,
        }),
      ).not.toThrow()
    }
    expect(() =>
      schedules.task({
        id: "timezone",
        cron: { pattern: "0 3 * * *", timezone: "utc" },
        workspace: workspaces.ref({ key: "maintenance" }),
        run: () => null,
      }),
    ).not.toThrow()
    expect(() =>
      schedules.task({
        id: "empty",
        cron: { pattern: "", timezone: "UTC" },
        workspace: workspaces.ref({ key: "maintenance" }),
        run: () => null,
      }),
    ).toThrow()
  })

  test("encodes the closed analysis result frame", () => {
    const program = analyze({
      architecture: "x86_64",
      exports: [{
        modulePath: "src/task.ts",
        exportName: "build",
        value: task({ id: "build", run: () => null }),
      }],
    })
    const succeeded = decodeAnalysisFrame(
      encodeVerificationResultFrame(successfulVerificationResult(program)),
    )
    expect(succeeded).toEqual({
      formatVersion: 0,
      outcome: "succeeded",
      declarations: program.programDeclarations,
      files: [
        {
          path: "helmr/build-plan.json",
          content: new TextDecoder().decode(program.buildPlanBytes),
        },
        {
          path: "helmr/analysis-locators.json",
          content: new TextDecoder().decode(program.declarationLocatorBytes),
        },
        {
          path: "helmr/entry.mjs",
          content: PROGRAM_ENTRYPOINT,
        },
      ],
    })

    const machine = workspace("machine")
      .image(image("root").from("debian:bookworm"))
      .resources({ cpu: 1, memory: "1GiB" })
    const workspaceOnly = analyze({
      architecture: "x86_64",
      exports: [{
        modulePath: "src/workspace.ts",
        exportName: "machine",
        value: machine,
      }],
    })
    const workspaceResult = decodeAnalysisFrame(
      encodeVerificationResultFrame(successfulVerificationResult(workspaceOnly)),
    ) as { declarations: unknown[]; files: unknown[] }
    expect(workspaceResult.declarations).toEqual([])
    expect(workspaceResult.files).toHaveLength(1)

    expect(decodeAnalysisFrame(
      encodeVerificationResultFrame(failedVerificationResult("module import failed")),
    )).toEqual({
      formatVersion: 0,
      outcome: "failed",
      error: {
        reason: "verification_failed",
        message: "module import failed",
      },
    })
  })
})

function decodeAnalysisFrame(frame: Uint8Array): unknown {
  const size = new DataView(
    frame.buffer,
    frame.byteOffset,
    frame.byteLength,
  ).getUint32(0, false)
  expect(size).toBe(frame.byteLength - 4)
  return JSON.parse(new TextDecoder().decode(frame.subarray(4)))
}
