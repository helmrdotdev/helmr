import { fixtureValue } from "../../e2e/support/runtime-mcp"
// Authored application for the integrated worker qualification. The driver
// starts the local model through an ordinary Computer command before native setup.
import { agent, template, image, source, type JsonValue } from "../../../sdk/typescript/src/index"
import assert from "node:assert/strict"
import { readFile, writeFile, rename, unlink, open } from "node:fs/promises"
import { createHash, randomFillSync } from "node:crypto"
import { setTimeout as delay } from "node:timers/promises"
import { spawnSync } from "node:child_process"
import { channel } from "node:diagnostics_channel"
import type { ChildProcess } from "node:child_process"
import { codex as nativeCodex, claude as nativeClaude } from "./agent"
import { startNativeModels } from "./model-server"
import { checkSessionDiscovery, type DiscoveryState } from "./session-discovery"
import { checkSecretBindings, type SecretBindingState } from "./secret-bindings"
import { conversation } from "../../../examples/issue-fixer/agents/issue-fixer/conversation"

const baseImage = image("native-computer").from("node:24.21.0-bookworm-slim")
// Match the Claude starter's native process-inspection dependency.
const claudeStarterImage = (process.env.HELMR_CLAUDE_STARTER_PROOF === "1" || process.env.HELMR_PROTECTED_STARTER_PROOF === "1")
  ? baseImage
      .run(["node", "-e", "const fs = require('node:fs'); fs.mkdirSync('/etc/ssl/certs', { recursive: true }); fs.writeFileSync('/etc/ssl/certs/ca-certificates.crt', require('node:tls').rootCertificates.join(String.fromCharCode(10)) + String.fromCharCode(10))"])
      .run(["sh", "-ceu", "sed -i 's|http://deb.debian.org|https://deb.debian.org|g' /etc/apt/sources.list.d/debian.sources && apt-get update && apt-get install -y --no-install-recommends procps && rm -rf /var/lib/apt/lists/*"])
  : baseImage
// Replaced by the fixture packaging command, before ordinary compilation.
const computerImage = process.env.HELMR_NATIVE_SDK_PROOF === "1"
  ? claudeStarterImage.copy(source.directory("native-sdk-dependencies"), "/workspace/native-sdk")
      .run(["sh", "/workspace/native-sdk/prepare-image.sh"])
      .copy(source.directory("native-sdk"), "/workspace/native-sdk")
  : process.env.HELMR_NATIVE_PERFORMANCE === "1"
  ? claudeStarterImage.copy(source.directory("performance"), "/opt/native-performance")
      .run(["sh", "/opt/native-performance/prepare-image.sh"])
  : claudeStarterImage
export const workspace = template({
  id: "native-continuation",
  image: computerImage
    .run(["mkdir", "-p", "/workspace"])
    .run(["chmod", "777", "/workspace"]),
  resources: { cpu: 2, memory: "4GiB", disk: "32GiB" },
  async prepare(build) {
    await build.exec(["node", "-e", "require('node:fs').writeFileSync('/workspace/prepared-environment', 'prepared once')"])
    await build.exec(["sh", "-ceu", "if [ -d /opt/native-performance ]; then mkdir /workspace/prepared-repository; tar -xf /opt/native-performance/repository.tar -C /workspace/prepared-repository; fi"])
  },
})

// Seed-only admission must qualify its Runtime without an authored Program.
export const seedWorkspace = template({
  id: "native-seed-only",
  image: baseImage.run(["mkdir", "-p", "/workspace"]),
  resources: { cpu: 1, memory: "1GiB", disk: "32GiB" },
})

function native(definition: typeof nativeCodex) {
  return agent({
    id: definition.id,
    template: workspace,
    suspension: "after_turn",
    async init(context) {
      assert.equal(await readFile("/workspace/prepared-environment", "utf8"), "prepared once")
      // Observe ordinary Node launches without replacing spawn or runtime state.
      const children: ChildProcess[] = []
      const launches = channel("child_process")
      const observe = (message: unknown) => { children.push((message as { process: ChildProcess }).process) }
      launches.subscribe(observe)
      let state: Awaited<ReturnType<typeof definition.init>>
      try { state = await definition.init(context) }
      finally { launches.unsubscribe(observe) }
      const nativeProcess = children.find(child => child.pid === state.bridge.processId)
      assert(nativeProcess, "expected the native launcher")
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
      if (input === "fixture-native-launcher-loss") {
        let release!: () => void
        const requested = new Promise<void>((resolve, reject) => {
          release = resolve
          turn.signal.addEventListener("abort", () => reject(turn.signal.reason), { once: true })
        })
        await turn.onMessage(message => { assert.equal(fixtureValue(message.content), "lose-native-launcher"); release() })
        await turn.output.write("native launcher loss ready")
        await requested
        const child = context.initResult.nativeProcess
        assert.equal(child.exitCode, null)
        assert.equal(child.signalCode, null)
        const exited = new Promise<void>(resolve => child.once("exit", () => resolve()))
        assert(child.kill("SIGKILL"), "owned native launcher was not signalled")
        await exited
        await definition.turn(turn, context)
        throw new Error("Lost launcher unexpectedly accepted native work")
      }
      const handlerMonotonicNs = process.hrtime.bigint().toString()
      const performanceKind = typeof input === "string" && input.startsWith("performance:") ? input.slice("performance:".length) : undefined
      const diagnosticPhase = (phase: string) => { if (performanceKind) console.error(JSON.stringify({event:"native.performance.handler_phase",provider:definition.id,turnId:turn.id,sequence:turn.sequence,phase,wallMs:Date.now(),monotonicNs:process.hrtime.bigint().toString(),handlerMonotonicNs})) }
      diagnosticPhase("entry")
      if (performanceKind) {
        assert(["no-op", "edit-test", "dependencies"].includes(performanceKind))
        const directory = `/workspace/${context.session.id}`
        assert(/^\/workspace\/[a-zA-Z0-9_-]+$/.test(directory))
        await writeFile(`/workspace/performance-${definition.id}.json`, JSON.stringify({ kind: performanceKind, command: `cd ${directory} && node /opt/native-performance/workload.mjs ${performanceKind} ${turn.sequence}` }))
      }
      const stopping = input === "fixture-native-stop"
      const stopPath = `/workspace/native-stop-${definition.id}.json`
      if (stopping) await writeFile(stopPath, JSON.stringify({ turnId: turn.id, directory: `/workspace/${context.session.id}` }))
      let nativeResult: Awaited<ReturnType<typeof definition.turn>>
      try { diagnosticPhase("native_start"); nativeResult = await definition.turn(turn, context); diagnosticPhase("native_end") }
      catch (error) {
        if (!stopping) throw error
        assert(turn.signal.aborted, "native control Turn failed without interruption")
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
        await writeFile(`${marker}.tmp`, JSON.stringify({ turnId: turn.id, gateStartedAt, pid: process.pid, nativePid: context.initResult.bridge.processId,
          nonce: context.initResult.nonce, setupCount: context.initResult.setupCount, commandLock }))
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
      const workload = performanceKind ? JSON.parse(await readFile(`/workspace/${context.session.id}/performance-result-${turn.sequence}.json`, "utf8")) : undefined
      if (workload) { assert.equal(workload.kind, performanceKind); assert.equal(workload.sequence, turn.sequence) }
      const result = { ...nativeResult, setupCount: context.initResult.setupCount, nativeConversationId: saved.id, ...(workload ? { workload } : {}) }
      diagnosticPhase("output_start")
      await turn.output.write([{ type: "json", value: result }])
      diagnosticPhase("output_end")
      await writeFile(`/workspace/${context.session.id}/result-${turn.sequence}.json`, JSON.stringify(result))
      return result
    },
  })
}

export const codex = native(nativeCodex)
export const claude = native(nativeClaude)

export const receipt = agent({
  id: "receipt", template: workspace,
  suspension: "after_turn",
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
  suspension: "after_turn",
  init: () => ({ nonce: crypto.randomUUID(), count: 0, discovery: undefined as DiscoveryState | undefined, secrets: undefined as SecretBindingState | undefined }),
  async turn(turn, { initResult: state, session, computer, mcp }): Promise<JsonValue> {
    const input = fixtureValue(turn.input)
    if (input === "recover-check") {
      let retained: JsonValue = null
      try { retained = JSON.parse(await readFile(`/workspace/peer-${session.id}.json`, "utf8")) }
      catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error }
      return { nonce: state.nonce, count: state.count, pid: process.pid, retained }
    }
    if (input === "verify-pressure") {
      const file = await open(`/workspace/pressure-${session.id}`, "r")
      try {
        // O_DIRECT forces this proof through the disk backend, bypassing guest page cache.
        const direct = spawnSync("dd", [`if=/workspace/pressure-${session.id}`, "iflag=direct", "bs=4M", "count=1", "status=none"], {
          maxBuffer: 4 * 1024 * 1024 + 65536, timeout: 30000,
        })
        assert.ifError(direct.error)
        assert.equal(direct.status, 0, direct.stderr.toString())
        assert.equal(direct.stdout.length, 4 * 1024 * 1024)
        return { firstChunkDigest: createHash("sha256").update(direct.stdout).digest("hex"), bytes: (await file.stat()).size }
      } finally { await file.close() }
    }
    if (input === "hold-pressure") {
      let released = false
      await turn.onMessage(message => { if (fixtureValue(message.content) === "release") released = true })
      const file = await open(`/workspace/pressure-${session.id}`, "wx+")
      const chunk = Buffer.alloc(4 * 1024 * 1024)
      const hashes: string[] = []
      try {
        while (!released) {
          turn.signal.throwIfAborted()
          assert(hashes.length < 128, "fixture did not reach pressure within 512 MiB")
          randomFillSync(chunk)
          const expected = createHash("sha256").update(chunk).digest("hex")
          const position = hashes.length * chunk.length
          let written = 0
          while (written < chunk.length) {
            const result = await file.write(chunk, written, chunk.length - written, position + written)
            assert(result.bytesWritten > 0)
            written += result.bytesWritten
          }
          await file.sync()
          hashes.push(expected)
          await turn.output.write(JSON.stringify({ nonce: state.nonce, count: ++state.count, pid: process.pid, bytes: hashes.length * chunk.length, firstChunkDigest: hashes[0] }))
        }
        for (let index = 0; index < hashes.length; index++) {
          let read = 0
          while (read < chunk.length) {
            const result = await file.read(chunk, read, chunk.length - read, index * chunk.length + read)
            assert(result.bytesRead > 0)
            read += result.bytesRead
          }
          assert.equal(createHash("sha256").update(chunk).digest("hex"), hashes[index])
        }
        return { nonce: state.nonce, count: state.count, pid: process.pid, bytes: hashes.length * chunk.length, verified: true }
      } finally { await file.close() }
    }
    const secretInput = input !== null && typeof input === "object" && !Array.isArray(input) && "kind" in input && input["kind"] === "hold-secrets"
    const performanceProbe = input === "hold-performance"
    let preparedWork: JsonValue | undefined
    if (performanceProbe && state.count === 0) {
      // Useful edit/build/test runs against the repository prepared before this
      // Computer existed. The workload must not extract a repository on demand.
      const result = spawnSync("node", ["/opt/native-performance/workload.mjs", "edit-test", "1", "/opt/native-performance", "prepared"], {
        cwd: "/workspace", encoding: "utf8", timeout: 30_000, maxBuffer: 1024 * 1024,
      })
      assert.ifError(result.error)
      assert.equal(result.status, 0, result.stderr)
      preparedWork = JSON.parse(result.stdout)
      assert.equal((preparedWork as { detail: { assertions: number } }).detail.assertions, 5)
    }
    if (input !== "hold" && input !== "hold-discovery" && !secretInput && !performanceProbe) return { received: input }
    if (secretInput || state.secrets) state.secrets = await checkSecretBindings(input, state.secrets)
    if (input === "hold-discovery" || state.discovery) {
      state.discovery = await checkSessionDiscovery(turn, session.id, computer, receipt, mcp, state.discovery)
    }
    let released = false
    await turn.onMessage(message => { if (fixtureValue(message.content) === "release") released = true })
    let previousProgressDelivery: { startedNs: string; stateWrittenNs: string; completedNs: string } | undefined
    while (!released) {
      turn.signal.throwIfAborted()
      let io: { bytes: number; writeReadMs: number; observedAt: string; startedNs: string; completedNs: string } | undefined
      if (performanceProbe) {
        const data = Buffer.alloc(32 * 1024, state.count % 256)
        const started = performance.now()
        const startedNs = process.hrtime.bigint().toString()
        const path = `/workspace/peer-io-${session.id}`
        await writeFile(path, data)
        assert.deepEqual(await readFile(path), data)
        io = { startedNs, completedNs: process.hrtime.bigint().toString(), bytes: data.length * 2, writeReadMs: performance.now() - started, observedAt: new Date().toISOString() }
      }
      const progress = { nonce: state.nonce, count: ++state.count, pid: process.pid, ...(io ? { io, previousProgressDelivery } : {}), ...(preparedWork ? { preparedWork } : {}),
        ...(state.secrets ? { secretChecks: state.secrets.checks } : {}),
        ...(state.discovery ? { discovery: { owned: state.discovery.owned.id, requested: state.discovery.requested.id,
          ownedTurn: state.discovery.ownedTurn, requestedTurn: state.discovery.requestedTurn,
          controls: state.discovery.controls, checks: state.discovery.checks } } : {}) }
      const deliveryStartedNs = process.hrtime.bigint().toString()
      await writeFile(`/workspace/peer-${session.id}.json`, JSON.stringify(progress))
      const stateWrittenNs = process.hrtime.bigint().toString()
      await turn.output.write(JSON.stringify(progress))
      previousProgressDelivery = { startedNs: deliveryStartedNs, stateWrittenNs, completedNs: process.hrtime.bigint().toString() }
      preparedWork = undefined
      await delay(100, undefined, { signal: turn.signal })
    }
    return { nonce: state.nonce, count: state.count, pid: process.pid }
  },
})

// Authored listeners pin compute. Park them before hibernation while retaining
// request history and setup state in RAM; reopen the same ports after restoration.
export const models = agent({
  id: "models", template: workspace,
  suspension: "after_turn",
  async init() {
    const target = { session: "", performance: false }
    const model = await startNativeModels(() => { assert(target.session); return target.session }, undefined, undefined, () => target.performance)
    await writeFile("/workspace/native-models.json", JSON.stringify(model.endpoints))
    return { ...model, target, nonce: crypto.randomUUID(), count: 0 }
  },
  async turn(turn, { initResult: state }): Promise<JsonValue> {
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

import { createCodexStarter } from "./codex-starter"
export const codexStarter = createCodexStarter(workspace)

import { createClaudeStarter } from "./claude-starter"
export const claudeStarter = createClaudeStarter(workspace)
