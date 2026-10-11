import { describe, expect, test } from "bun:test"
import { agent, computer, image, source, triggers } from "@helmr/sdk"
import { analyze, normalizeComputerResources } from "./compile"
import { successfulVerificationResult } from "./analysis"
const machine = () => computer({ id: "machine", image: image("root").from("ubuntu:24.04"), resources: { cpu: 2, memory: "4GiB", disk: "16GiB" } })
const entry = (value: unknown, exportName = "worker", modulePath = "helmr/app/entry-0.mjs") => ({ value, exportName, modulePath })
const compile = (...exports: ReturnType<typeof entry>[]) => analyze({ architecture: "x86_64", exports })

describe("Agent declaration analysis", () => {
  test("bundles inline Computer and all execution functions without running them", () => {
    const prepare = () => { throw new Error("prepare executed during compilation") }
    const worker = agent({ id: "worker", computer: computer({ ...machine(), prepare, refresh: { every: "1h", maxAge: "1d" }, secrets: [{ secretId: "01900000-0000-7000-8000-000000000001", env: { name: "TOKEN", mode: "raw" } }] }), setup: () => { throw new Error("setup executed") }, turn: () => { throw new Error("turn executed") }, maxTurnDuration: "5m", closeAfterIdle: 1000, triggers: [triggers.cron("hourly", "0 * * * *", { timezone: "Asia/Tokyo", input: [{ type: "text", text: "work" }] })] })
    const result = compile(entry(worker))
    expect(result.buildPlan.definitions[1]?.manifest).toMatchObject({ prepare: true })
    expect(result.buildPlan.definitions[0]).toEqual({ kind: "agent", declaredId: "worker", manifest: { computerDefinitionId: "machine", setup: true, maxTurnDurationMs: 300000, closeAfterIdleMs: 1000, triggers: { hourly: { cron: "0 * * * *", timezone: "Asia/Tokyo", input: [{ type: "text", text: "work" }] } } } })
    const declaration = result.buildPlan.definitions[1]!
    if (declaration.kind !== "computer") throw new Error("Computer missing")
    expect(declaration.manifest.resources).toEqual({ milliCpu: 2000, memoryMiB: 4096, diskMiB: 16384 })
    expect(declaration.manifest.refresh).toEqual({ everyMs: 3600000, maxAgeMs: 86400000 })
    expect(declaration.manifest.buildSecrets).toEqual([])
    expect(JSON.parse(new TextDecoder().decode(result.definitionIndexBytes)).computers).toEqual([{ id: "machine", modulePath: "helmr/app/entry-0.mjs", exportName: "worker", throughAgent: true }])
    const frame = successfulVerificationResult(result)
    expect(Object.keys(frame).sort()).toEqual(["files", "formatVersion", "outcome"])
    expect(frame.files.map(file => file.path)).toEqual(["helmr/build-plan.json", "helmr/definition-index.json"])
  })
  test("chooses deterministic re-exports and direct Computer exports", () => {
    const computer = machine(), worker = agent({ id: "worker", computer, turn: () => null })
    const entries = [entry(worker, "z", "helmr/app/entry-2.mjs"), entry(worker, "a"), entry(computer, "machine", "helmr/app/entry-9.mjs")]
    const result = compile(...entries)
    expect(compile(...entries.toReversed()).definitionIndexBytes).toEqual(result.definitionIndexBytes)
    expect(result.definitionIndex.agents).toEqual([{ id: "worker", computerDefinitionId: "machine", modulePath: "helmr/app/entry-0.mjs", exportName: "a" }])
    expect(result.definitionIndex.computers).toEqual([{ id: "machine", modulePath: "helmr/app/entry-9.mjs", exportName: "machine", throughAgent: false }])
  })
  test("does not substitute distinct Computers with the same declaration id", () => {
    const worker = agent({ id: "worker", computer: machine(), turn: () => null })
    expect(() => compile(entry(worker), entry(machine(), "machine"))).toThrow("duplicate computer")
    expect(() => compile(entry(worker), entry(agent({ ...worker }), "other"))).toThrow("duplicate agent")
  })
  test("rejects malformed locators, durations, credentials and application JSON", () => {
    const worker = agent({ id: "worker", computer: machine(), turn: () => null })
    expect(() => compile(entry(worker, "x", "../escape.mjs"))).toThrow()
    for (const maxTurnDuration of [0, -1, NaN, Infinity, 0.5, "0s", "1.5s", "01s"]) expect(() => compile(entry({ ...worker, maxTurnDuration }))).toThrow()
    expect(() => compile(entry({ ...machine(), secrets: [{ secretId: "name", env: { name: "TOKEN", mode: "raw" } }] }))).toThrow("Secret ID")
    expect(() => compile(entry({ ...machine(), buildSecrets: [{ secretId: "01900000-0000-7000-8000-000000000001", env: { name: "TOKEN", mode: "raw" }, value: "secret" }] }))).toThrow("unknown member")
    expect(() => compile(entry({ ...worker, triggers: [{ id: "tick", cron: "* * * * *", timezone: "UTC", input: undefined }] }))).toThrow()
    expect(() => compile(entry({ kind: "task", id: "old" }))).toThrow("definitions")
  })
  test("rejects misspelled policy fields and preserves explicit cron identity", () => {
    const worker = agent({ id: "worker", computer: machine(), turn: () => null })
    for (const value of [{ ...worker, maxTurnDurations: "1h" }, { ...machine(), secret: {} }, { ...machine(), refresh: { every: "1h", maxage: "2h" } }, { ...machine(), resources: { cpu: 1, memory: "1GiB", disks: "1GiB" } }, { ...worker, triggers: [{ id: "tick", cron: "* * * * *", timezone: "UTC", input: [], overlap: true }] }]) expect(() => compile(entry(value))).toThrow("unknown members")
    expect(() => triggers.cron("tick", "* * * * *", { input: [] } as never)).toThrow("timezone")
    const tick = triggers.cron("tick", "* * * * *", { timezone: "Asia/Tokyo", input: [] })
    expect(() => compile(entry({ ...worker, triggers: [tick, tick] }))).toThrow("Duplicate")
    const second = agent({ id: "second", computer: machine(), turn: () => null })
    expect(() => compile(entry(worker), entry(second, "second", "helmr/app/entry-1.mjs"))).toThrow("duplicate computer")
    const sharedId = agent({ id: "machine", computer: machine(), turn: () => null })
    expect(compile(entry(sharedId)).buildPlan.definitions.map(item => item.kind)).toEqual(["agent", "computer"])
  })
  test("rejects nil Secret identities for both runtime and preparation", () => {
    for (const field of ["secrets", "buildSecrets"] as const) {
      expect(() => compile(entry(computer({ ...machine(), [field]: [{ secretId: "00000000-0000-0000-0000-000000000000", env: { name: "TOKEN", mode: "raw" } }] })))).toThrow("Secret")
    }
  })
  test("canonicalizes all Secret placements and rejects ambiguous delivery", () => {
    const secretId = "01900000-0000-7000-8000-000000000003"
    const bindings = [
      { secretId, file: { path: "/etc/service/token" } },
      { secretId, env: { name: "TOKEN", mode: "protected", allowedOrigins: ["https://API.EXAMPLE.COM:443", "https://api.example.com"] } },
      { secretId, env: { name: "RAW_TOKEN", mode: "raw" } },
    ]
    const result = compile(entry({ ...machine(), secrets: bindings, buildSecrets: bindings.toReversed() }))
    const declaration = result.buildPlan.definitions[0]!
    if (declaration.kind !== "computer") throw new Error("Computer missing")
    expect(declaration.manifest.secrets).toEqual([
      { secretId, env: { name: "RAW_TOKEN", mode: "raw" } },
      { secretId, env: { name: "TOKEN", mode: "protected", allowedOrigins: ["https://api.example.com"] } },
      { secretId, file: { path: "/etc/service/token" } },
    ])
    expect(declaration.manifest.buildSecrets).toEqual(declaration.manifest.secrets)
    for (const invalid of [
      {},
      [{ secretId }],
      [{ secretId: "11111111-1111-4111-8111-111111111111", env: { name: "TOKEN", mode: "raw" } }],
      [{ secretId: "01900000-0000-7AAA-8AAA-AAAAAAAAAAAA", env: { name: "TOKEN", mode: "raw" } }],
      [{ secretId, env: { name: "TOKEN", mode: "raw" }, file: { path: "/etc/token" } }],
      [{ secretId, env: { name: "NODE_OPTIONS", mode: "raw" } }],
      [{ secretId, env: { name: "TOKEN", mode: "protected", allowedOrigins: ["http://api.example.com"] } }],
      [{ secretId, env: { name: "TOKEN", mode: "raw", allowedOrigins: ["https://api.example.com"] } }],
      [{ secretId, file: { path: "/workspace/token" } }],
      [{ secretId, file: { path: "/etc/token" } }, { secretId, file: { path: "/etc/token/child" } }],
      [bindings[1], bindings[1]],
    ]) {
      for (const field of ["secrets", "buildSecrets"]) expect(() => compile(entry({ ...machine(), [field]: invalid }))).toThrow()
    }
  })
  test("preserves image source copies and rejects unknown step members", () => {
    const base = image("dependency").from("ubuntu:24.04")
    const disk = image("root").from("ubuntu:24.04").copy(source.file("package.json"), "/app/package.json").copy(source.directory("src"), "/app/src").copyFrom("/usr/local/bin/tool", base, "/opt/tool")
    const result = compile(entry(computer({ ...machine(), image: disk })))
    const declaration = result.buildPlan.definitions[0]!
    if (declaration.kind !== "computer") throw new Error("Computer missing")
    expect(declaration.manifest.imageBuild.images.flatMap(value => value.steps)).toContainEqual({ copyFromImage: { dst: "/usr/local/bin/tool", imageKey: "dependency", srcPath: "/opt/tool" } })
    expect(declaration.manifest.imageBuild.images.flatMap(value => value.steps)).toContainEqual({ copySourceFile: { dst: "/app/package.json", path: "package.json" } })
    const forged = image("forged").from("ubuntu:24.04").run(["true"])
    Object.defineProperty((forged as unknown as { steps: object[] }).steps[1]!, "unknown", { value: true, enumerable: true })
    expect(() => compile(entry(computer({ ...machine(), image: forged })))).toThrow("unknown members")
  })
  test("normalizes resources exactly and rejects rounding or aliases", () => {
    expect(
      normalizeComputerResources({
        cpu: 1e-3,
        memory: "1GiB",
      }),
    ).toEqual({
      milliCpu: 1,
      memoryMiB: 1024,
    })

    for (const cpu of [0, -1, Number.NaN, Number.POSITIVE_INFINITY, 0.0001]) {
      expect(() =>
        normalizeComputerResources({
          cpu,
          memory: "1MiB",
        }),
      ).toThrow()
    }
    for (const memory of ["01MiB", "1GB", "1.5GiB", " 1GiB", "+1MiB"]) {
      expect(() =>
        normalizeComputerResources({
          cpu: 1,
          memory,
        }),
      ).toThrow()
    }
  })


})

test("Go admission fixtures are generated by the authored compiler contract", async () => {
  const { agentContractFixture } = await import("./agent-contract.fixture")
  const result = agentContractFixture()
  const { readFile } = await import("node:fs/promises")
  expect(new Uint8Array(await readFile(new URL("../../../internal/definition/testdata/agent-build-plan.json", import.meta.url)))).toEqual(result.buildPlanBytes)
  expect(new Uint8Array(await readFile(new URL("../../../internal/artifact/testdata/definition-index.json", import.meta.url)))).toEqual(result.definitionIndexBytes)
  const { encodeVerificationResultFrame } = await import("./analysis")
  expect(new Uint8Array(await readFile(new URL("../../../internal/builder/testdata/verification.frame", import.meta.url)))).toEqual(encodeVerificationResultFrame(successfulVerificationResult(result)))
})

test("validates one text-input contract for every scheduled Agent", () => {
  const worker = agent({ id: "worker", computer: machine(), turn: () => null })
  expect(() => compile(entry({ ...worker, messages: { content: ["text"] } }))).toThrow()
  const trigger = (input: unknown) => [{ id: "tick", cron: "* * * * *", timezone: "UTC", input }]
  for (const input of [null, "hello", { task: "job" }, { type: "message", content: [] }, [{ type: "json", value: null }]]) {
    expect(() => compile(entry({ ...worker, triggers: trigger(input) }))).toThrow()
  }
  expect(() => compile(entry({ ...worker, triggers: trigger([{ type: "text", text: "a\0雪" }]) }))).not.toThrow()
})

test("pins a literal cron Slack configuration and rejects malformed destinations", () => {
  const channelId = "C123"
  const trigger = triggers.cron("tick", "* * * * *", { timezone: "UTC", input: [], slack: { channelId } })
  const worker = agent({ id: "worker", computer: machine(), turn: () => null, triggers: [trigger] })
  expect(compile(entry(worker)).buildPlan.definitions[0]?.manifest).toMatchObject({ triggers: { tick: { slack: { channelId } } } })
  for (const slack of [null, {}, { channelId: "" }, { channelId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35" }, { channelId, name: "mutable" }]) {
    expect(() => triggers.cron("tick", "* * * * *", { timezone: "UTC", input: [], slack } as never)).toThrow()
    expect(() => compile(entry({ ...worker, triggers: [{ ...trigger, slack }] }))).toThrow()
  }
})
