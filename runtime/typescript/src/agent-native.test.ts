import { EventEmitter } from "node:events"
import { PassThrough } from "node:stream"
import type { spawn } from "node:child_process"
import { test } from "node:test"
import assert from "node:assert/strict"
import { SessionNativeRegistry } from "./agent-native"
import type { Turn } from "../../../sdk/typescript/src/agent"
import { registerNativeResource } from "../../../sdk/typescript/src/internal/native-runtime"
function deferred<T>() {
  let resolve!: (value: T) => void, reject!: (reason: unknown) => void
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}
const lifecycle = () => ({ initialized: Promise.resolve(), state: () => ({ phase: "idle" as const, reusable: true }), stop: async () => {} })

test("setup joins resources registered before an unawaited initialization", async () => {
  const registry = new SessionNativeRegistry(new AbortController().signal)
  const ready = deferred<void>()
  let completed = false
  const setup = registry.setup(() => { registry.resource({ ...lifecycle(), initialized: ready.promise }) }).then(() => { completed = true })
  await Promise.resolve(); await Promise.resolve()
  assert.equal(completed, false)
  ready.resolve()
  await setup
  assert.equal(completed, true)
  assert.throws(() => registry.resource(lifecycle()), /during Session setup/)
})

test("setup failure from a forgotten native open remains visible", async () => {
  const registry = new SessionNativeRegistry(new AbortController().signal)
  const ready = deferred<void>()
  const setup = registry.setup(() => { registry.resource({ ...lifecycle(), initialized: ready.promise }) })
  ready.reject(new Error("native initialization failed"))
  await assert.rejects(setup, /native initialization failed/)
})

test("native operation requires the exact Turn and its owning resource", async () => {
  const registry = new SessionNativeRegistry(new AbortController().signal)
  const resource = await registry.setup(() => registry.resource(lifecycle()))
  const turn = { id: "turn" } as Turn
  let open = true, invoked = 0
  registry.bind(turn, { id: "turn", assertOpen: () => { if (!open) throw new Error("closed") }, producer: () => ({ run: callback => { invoked++; return callback() } }) })
  const operation = { done: Promise.resolve(), state: () => ({ idle: true, reusable: true }), stop: async () => {} }
  assert.throws(() => registry.register({ ...turn }, resource, operation), /exact Session and Turn/)
  assert.throws(() => registry.register(turn, { signal: resource.signal, cooperativeStopMs: resource.cooperativeStopMs }, operation), /exact Session and Turn/)
  const invocation = registry.register(turn, resource, operation)
  assert.equal(invocation.run(() => 42), 42)
  assert.equal(invoked, 1)
  open = false
  assert.throws(() => registry.register(turn, resource, operation), /closed/)
})

test("ordinary failure stops only active work and preserves healthy idle resources", async () => {
  const registry = new SessionNativeRegistry(new AbortController().signal)
  let resourceStops = 0, operationStops = 0
  const resource = await registry.setup(() => registry.resource({ ...lifecycle(), stop: async () => { resourceStops++ } }))
  const turn = { id: "turn" } as Turn
  registry.bind(turn, { id: "turn", assertOpen: () => {}, producer: () => ({ run: callback => callback() }) })
  const done = deferred<void>()
  let idle = false
  registry.register(turn, resource, { done: done.promise, state: () => ({ idle, reusable: true }), stop: async () => { operationStops++; idle = true; done.resolve() } })
  await registry.join("turn", "failed", new Error("application failure"))
  await registry.join("turn", "failed")
  assert.equal(operationStops, 1)
  assert.equal(resourceStops, 0)
  assert.deepEqual(await registry.evidence(), { scopes: [], reusable: true })
})

test("installed private hook rejects resource admission outside setup", async () => {
  const registry = new SessionNativeRegistry(new AbortController().signal)
  const uninstall = registry.install()
  try {
    await registry.setup(() => registerNativeResource(lifecycle()))
    assert.deepEqual(await registry.evidence(), { scopes: [], reusable: true })
  } finally { uninstall() }
  assert.throws(() => registerNativeResource(lifecycle()), /managed Session runtime/)
})


test("proxy identity and process exit determine native convergence evidence", async () => {
  const metadata = new PassThrough()
  const child = Object.assign(new EventEmitter(), { stdio: [null, null, null, metadata], kill: () => true })
  const spawnProcess = ((command: string, args: string[], options: any) => {
    assert.equal(command, "/opt/helmr/native/launch")
    assert.deepEqual(args, ["__helmr-native-proxy", "harness", "--stdio"])
    assert.deepEqual(options.stdio, ["pipe", "pipe", "pipe", "pipe"])
    return child
  }) as unknown as typeof spawn
  const registry = new SessionNativeRegistry(new AbortController().signal, 10_000, spawnProcess)
  let phase: "initializing" | "idle" | "closed" = "initializing"
  await registry.setup(async () => {
    const resource = registry.resource({ ...lifecycle(), state: () => ({ phase, reusable: true }) })
    const native = registry.spawn(resource, "harness", ["--stdio"], {})
    metadata.end(JSON.stringify({ scopeId: "native-scope", processId: 42 }))
    assert.deepEqual(await native.ready, { scopeId: "native-scope", processId: 42 })
    phase = "idle"
  })
  assert.deepEqual(await registry.evidence(), { scopes: [{ scopeId: "native-scope", state: "idle" }], reusable: true })
  phase = "closed"
  await assert.rejects(registry.evidence(), /has not exited/)
  phase = "idle"
  child.emit("close", 1)
  assert.deepEqual(await registry.evidence(), { scopes: [{ scopeId: "native-scope", state: "stopped" }], reusable: false })
})
