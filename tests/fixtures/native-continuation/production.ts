import { fixtureValue } from "../../e2e/support/runtime-mcp"
// Authored application for the integrated worker qualification. The driver
// starts the local model through an ordinary Computer command before native setup.
import { agent, template, image, source, type Json, type InputContent } from "../../../sdk/typescript/src/index"
import assert from "node:assert/strict"
import { readFile, writeFile, rename, unlink } from "node:fs/promises"
import { setTimeout as delay } from "node:timers/promises"
import { spawnSync } from "node:child_process"
import type { NativeProcess, NativeRuntimeHooks } from "../../../sdk/typescript/src/internal/native-runtime"
import { codex as nativeCodex, claude as nativeClaude } from "./agent"
import { startNativeModels } from "./model-server"
import { checkSessionDiscovery, type DiscoveryState } from "./session-discovery"
import { checkSecretBindings, type SecretBindingState } from "./secret-bindings"
import { conversation } from "../../../examples/issue-fixer/agents/issue-fixer/conversation"

const baseImage = image("native-computer").from("node:24.21.0-bookworm-slim")
// Replaced by the fixture packaging command, before ordinary compilation.
const computerImage = process.env.HELMR_NATIVE_PERFORMANCE === "1"
  ? baseImage.copy(source.directory("performance"), "/opt/native-performance")
      .run(["sh", "/opt/native-performance/prepare-image.sh"])
  : baseImage
export const workspace = template({
  id: "native-continuation",
  image: computerImage
    .run(["mkdir", "-p", "/workspace"])
    .run(["chmod", "777", "/workspace"]),
  resources: { cpu: 2, memory: "4GiB", disk: "32GiB" },
})

function native(definition: typeof nativeCodex) {
  return agent({
    id: definition.id,
    template: workspace,
    async init(context) {
      // Observe the ordinary launcher without replacing its authority or process
      // admission. This fixture kills only the exact proxy handle it receives;
      // guest PID metadata is never used as a signal target.
      const key = Symbol.for("helmr.agent.v1.native_runtime")
      const target = globalThis as typeof globalThis & { [key: symbol]: NativeRuntimeHooks }
      const original = target[key]!
      assert(original, "managed native runtime is required")
      let nativeProcess: NativeProcess | undefined
      const observed: NativeRuntimeHooks = { ...original, spawn(...args) {
        assert.equal(nativeProcess, undefined, "expected one native launcher")
        return nativeProcess = original.spawn(...args)
      } }
      target[key] = observed
      let state: Awaited<ReturnType<typeof definition.init>>
      try { state = await definition.init(context) }
      finally { assert.equal(target[key], observed); target[key] = original }
      assert(nativeProcess)
      const path = `/workspace/${context.session.id}/setup-count.json`
      let setupCount = 0
      try { setupCount = JSON.parse(await readFile(path, "utf8")) }
      catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error }
      assert(Number.isSafeInteger(setupCount) && setupCount >= 0)
      await writeFile(path, JSON.stringify(++setupCount))
      return { ...state, nativeProcess, setupCount }
    },
    async turn(turn, context) {
      const input = fixtureValue(turn.input)
      if (input === "fixture-native-proxy-loss") {
        let release!: () => void
        const requested = new Promise<void>((resolve, reject) => {
          release = resolve
          turn.signal.addEventListener("abort", () => reject(turn.signal.reason), { once: true })
        })
        await turn.onMessage(message => { assert.equal(fixtureValue(message), "lose-native-proxy"); release() })
        await turn.output.write("native proxy loss ready")
        await requested
        const child = context.initResult.nativeProcess.child
        assert.equal(child.exitCode, null)
        assert.equal(child.signalCode, null)
        const exited = new Promise<void>(resolve => child.once("close", () => resolve()))
        assert(child.kill("SIGKILL"), "owned native proxy was not signalled")
        await exited
        throw new Error("Fixture native launcher proxy was lost")
      }
      const performanceKind = typeof input === "string" && input.startsWith("performance:") ? input.slice("performance:".length) : undefined
      if (performanceKind) {
        assert(["no-op", "edit-test", "dependencies"].includes(performanceKind))
        const directory = `/workspace/${context.session.id}`
        assert(/^\/workspace\/[a-zA-Z0-9_-]+$/.test(directory))
        await writeFile(`/workspace/performance-${definition.id}.json`, JSON.stringify({ command: `cd ${directory} && node /opt/native-performance/workload.mjs ${performanceKind} ${turn.sequence}` }))
      }
      const stopping = input === "fixture-native-stop"
      const stopPath = `/workspace/native-stop-${definition.id}.json`
      if (stopping) await writeFile(stopPath, JSON.stringify({ turnId: turn.id, directory: `/workspace/${context.session.id}` }))
      let nativeResult: Awaited<ReturnType<typeof definition.turn>>
      try { nativeResult = await definition.turn(turn, context) }
      catch (error) {
        if (!stopping) throw error
        assert(turn.signal.aborted, "native control Turn failed without interruption")
        const identity = await context.initResult.nativeProcess.ready
        let commandLock: string | undefined
        if (definition.id === "codex") {
          const command = `/workspace/${context.session.id}/owned-command-${turn.id}`
          assert.equal(await readFile(`${command}.started`, "utf8"), turn.id)
          commandLock = `${command}.lock`
          // The shared inode identifies the command across isolated PID namespaces.
          const lock = spawnSync("/usr/bin/flock", ["--nonblock", "--exclusive", commandLock, "/bin/true"], { timeout: 1_000 })
          assert.equal(lock.status, 0, "owned native command retained its lifetime lock")
        }
        await unlink(stopPath)
        const gateStartedAt = Date.now()
        const marker = `/workspace/native-stop-settled-${turn.id}.json`
        await writeFile(`${marker}.tmp`, JSON.stringify({ turnId: turn.id, gateStartedAt, pid: process.pid, nativePid: context.initResult.harness.processId,
          nonce: context.initResult.nonce, setupCount: context.initResult.setupCount, nativeScopeId: identity.scopeId, commandLock }))
        await rename(`${marker}.tmp`, marker)
        // Authored cleanup deliberately waits for the external fixture to observe
        // pending-stop admission. A bounded failure cannot retain the gate forever.
        const deadline = gateStartedAt + 4_000
        for (;;) {
          try { assert.equal(await readFile(`${marker}.release`, "utf8"), turn.id); break }
          catch (missing) { if ((missing as NodeJS.ErrnoException).code !== "ENOENT") throw missing }
          assert(Date.now() < deadline, "fixture cleanup gate exceeded 4s; release observation was too slow")
          await delay(50)
        }
        process.stderr.write(JSON.stringify({ nativeStopGate: { turnId: turn.id, durationMs: Date.now() - gateStartedAt } }) + "\n")
        throw error
      }
      assert(!stopping, "native control Turn completed without interruption")
      const saved = await conversation(`/workspace/${context.session.id}`, context.session.id, definition.id)
      assert(saved.id, "successful native Turn has no established conversation")
      const identity = await context.initResult.nativeProcess.ready
      const workload = performanceKind ? JSON.parse(await readFile(`/workspace/${context.session.id}/performance-result-${turn.sequence}.json`, "utf8")) : undefined
      if (workload) { assert.equal(workload.kind, performanceKind); assert.equal(workload.sequence, turn.sequence) }
      const result = { ...nativeResult, setupCount: context.initResult.setupCount, nativeConversationId: saved.id, nativeScopeId: identity.scopeId, ...(workload ? { workload } : {}) }
      await turn.output.write([{ type: "json", value: result }])
      await writeFile(`/workspace/${context.session.id}/result-${turn.sequence}.json`, JSON.stringify(result))
      return result
    },
  })
}

export const codex = native(nativeCodex)
export const claude = native(nativeClaude)

export const receipt = agent<InputContent, Json, unknown>({
  id: "receipt", template: workspace,
  async turn(turn, { session }) {
    const input = fixtureValue(turn.input)
    const marker = `/workspace/control-${session.id}.json`
    await writeFile(`${marker}.tmp`, JSON.stringify({ turnId: turn.id, processId: process.pid }))
    await rename(`${marker}.tmp`, marker)
    if (input === "native-control-hold") {
      for (;;) await delay(60_000, undefined, { signal: turn.signal })
    }
    return input
  },
})

// Hold a real Turn open until the driver sends a message after observing both
// native completions. Continuing output and disk writes demonstrate peer work
// across each mandatory save; elapsed time alone is not the assertion.
export const peer = agent({
  id: "peer",
  template: workspace,
  init: () => ({ nonce: crypto.randomUUID(), count: 0, discovery: undefined as DiscoveryState | undefined, secrets: undefined as SecretBindingState | undefined }),
  async turn(turn, { initResult: state, session, computer }): Promise<Json> {
    const input = fixtureValue(turn.input)
    const secretInput = input !== null && typeof input === "object" && !Array.isArray(input) && "kind" in input && input["kind"] === "hold-secrets"
    const performanceProbe = input === "hold-performance"
    if (input !== "hold" && input !== "hold-discovery" && !secretInput && !performanceProbe) return { received: input }
    if (secretInput || state.secrets) state.secrets = await checkSecretBindings(input, state.secrets)
    if (input === "hold-discovery" || state.discovery) {
      state.discovery = await checkSessionDiscovery(turn, session.id, computer, receipt, state.discovery)
    }
    let released = false
    await turn.onMessage(message => { if (fixtureValue(message) === "release") released = true })
    while (!released) {
      turn.signal.throwIfAborted()
      let io: { bytes: number; writeReadMs: number; observedAt: string } | undefined
      if (performanceProbe) {
        const data = Buffer.alloc(32 * 1024, state.count % 256)
        const started = performance.now()
        const path = `/workspace/peer-io-${session.id}`
        await writeFile(path, data)
        assert.deepEqual(await readFile(path), data)
        io = { bytes: data.length * 2, writeReadMs: performance.now() - started, observedAt: new Date().toISOString() }
      }
      const progress = { nonce: state.nonce, count: ++state.count, pid: process.pid, ...(io ? { io } : {}),
        ...(state.secrets ? { secretChecks: state.secrets.checks } : {}),
        ...(state.discovery ? { discovery: { owned: state.discovery.owned.id, requested: state.discovery.requested.id,
          ownedTurn: state.discovery.ownedTurn, requestedTurn: state.discovery.requestedTurn,
          controls: state.discovery.controls, checks: state.discovery.checks } } : {}) }
      await writeFile(`/workspace/peer-${session.id}.json`, JSON.stringify(progress))
      await turn.output.write(JSON.stringify(progress))
      await delay(100, undefined, { signal: turn.signal })
    }
    return { nonce: state.nonce, count: state.count, pid: process.pid }
  },
})

// Authored listeners pin compute. Park them before hibernation while retaining
// request history and setup state in RAM; reopen the same ports after restoration.
export const models = agent({
  id: "models", template: workspace,
  async init() {
    const target = { session: "", performance: false }
    const model = await startNativeModels(() => { assert(target.session); return target.session }, undefined, undefined, () => target.performance)
    await writeFile("/workspace/native-models.json", JSON.stringify(model.endpoints))
    return { ...model, target, nonce: crypto.randomUUID(), count: 0 }
  },
  async turn(turn, { initResult: state }): Promise<Json> {
    const input = fixtureValue(turn.input)
    assert(typeof input === "string" || (input && typeof input === "object" && !Array.isArray(input)))
    if (input === "park") {
      await Promise.all(state.servers.map(server => new Promise<void>((resolve, reject) => {
        server.close(error => error ? reject(error) : resolve())
        server.closeIdleConnections()
      })))
      state.servers = []
      return { parked: true, nonce: state.nonce, pid: process.pid, count: state.count }
    }
    if (typeof input === "string") state.target.session = input
    else {
      assert("peerSession" in input && "performance" in input)
      assert(typeof input.peerSession === "string"); assert.equal(input.performance, true)
      state.target.session = input.peerSession; state.target.performance = true
    }
    if (!state.servers.length) {
      const model = await startNativeModels(() => state.target.session, state.endpoints, state.requests, () => state.target.performance, state.controlState)
      state.servers = model.servers
    }
    return { nonce: state.nonce, pid: process.pid, count: ++state.count,
      codexRequests: state.requests.codex.length, claudeRequests: state.requests.claude.length }
  },
})

// The elapsed Turn deadline includes ordinary mandatory Save publication.
export const deadline = agent({
  id: "deadline", template: workspace, maxTurnDuration: "30s",
  turn(turn) { return { input: fixtureValue(turn.input) } },
})
