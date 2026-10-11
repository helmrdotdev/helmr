import { fixtureInput } from "../../e2e/support/runtime-mcp"
import assert from "node:assert/strict"
import { createHash, randomBytes } from "node:crypto"
import { readFile, writeFile, rename } from "node:fs/promises"
import { setTimeout as delay } from "node:timers/promises"
import { HelmrClient } from "../../../sdk/typescript/src/client"
import type { ClientSessionRef } from "../../../sdk/typescript/src/client-session"
import type { SessionEvent, TurnRef, TurnState } from "../../../sdk/typescript/src/contract"

// PostgreSQL observations are written by the surrounding qualification process.
// They only observe checkpoint readiness and ordinary Worker physical-stop
// receipts; the driver never manufactures an allocation or a source fence.
export async function runHealthyContinuation(options: { url: string; apiKey: string; timeoutMs: number; checkpointObservations: string; sessionDiscovery?: boolean; secretBindings?: boolean; longIdle?: boolean; checkpointKeyMismatch?: boolean; crossHost?: boolean; sourceHostId?: string; targetHostId?: string; checkpointFault?: "corrupt-config" | "missing-memory"; performanceKind?: "no-op" | "edit-test" | "dependencies" }) {
 const signal = AbortSignal.timeout(options.timeoutMs)
 const client = new HelmrClient({ url: options.url, apiKey: options.apiKey })
 const sessions: ClientSessionRef[] = []
 const turns: TurnState[] = []
 const retainedTurns = new Map<string, TurnRef>()
 const nativeSessions: Record<string, string> = {}
 const native: Record<string, { session: ClientSessionRef; previous: Record<string, unknown> }> = {}
 const continuations: Record<string, unknown>[] = []
 const clientTimings = new Map<string, { provider: string; phase: string; sessionId: string; turnId: string; requestStartedAt: string; requestStartedMS: number; ackMS: number; runningObservedMS?: number; firstOutputObservedMS?: number; firstOutputSequence?: number; settlementObservedMS?: number }>()
 const nativeEvents: (SessionEvent & { clientObservedMS: number })[] = []
 const eventCursors = new Map<string, number>()
 function admitted(provider: string, phase: string, turn: TurnRef, started: number, at: string) {
  if (options.performanceKind) clientTimings.set(turn.id, { provider, phase, sessionId: turn.sessionId, turnId: turn.id, requestStartedAt: at, requestStartedMS: started, ackMS: performance.now() - started })
 }
 async function observeEvents(session: ClientSessionRef) {
  if (!options.performanceKind) return
  for (;;) {
   const page = await session.events.list({ after: eventCursors.get(session.id) ?? 0, limit: 100 }, { signal })
   const observed = performance.now()
   eventCursors.set(session.id, page.nextAfter)
   for (const event of page.records) {
    nativeEvents.push({ ...event, clientObservedMS: observed })
    const timing = event.turnId ? clientTimings.get(event.turnId) : undefined
    if (!timing) continue
    if (event.kind === "turn.running") timing.runningObservedMS ??= observed - timing.requestStartedMS
    if (event.kind === "turn.output" && timing.firstOutputObservedMS === undefined) {
     timing.firstOutputObservedMS = observed - timing.requestStartedMS
     timing.firstOutputSequence = event.sequence
    }
   }
   if (!page.hasMore) return
  }
 }
 const secretIds: string[] = []
 let secretChecks = 0
 let secretRevocation: Record<string, unknown> | undefined
 async function startPeer() {
  if (!options.secretBindings) return client.agents.start("peer", { input: fixtureInput(options.performanceKind ? "hold-performance" : options.sessionDiscovery ? "hold-discovery" : "hold" )}, { signal })
  try {
   const digests: string[] = []
   for (const kind of ["raw", "file", "protected"]) {
    const value = randomBytes(32).toString("hex")
    const secret = await client.secrets.create({ name: `native-${kind}`, value, idempotencyKey: `native-${kind}` }, { signal })
    secretIds.push(secret.id); digests.push(createHash("sha256").update(value).digest("hex"))
   }
   const computer = await client.computerDefinitions.createComputer("native-continuation", {
    idempotencyKey: "native-secret-computer", secrets: [
     { secretId: secretIds[0]!, env: { name: "NATIVE_RAW_SECRET", mode: "raw" } },
     { secretId: secretIds[1]!, file: { path: "/run/native-proof/secret" } },
     { secretId: secretIds[2]!, env: { name: "NATIVE_PROTECTED_SECRET", mode: "protected", allowedOrigins: ["https://native-proof.invalid"] } },
    ],
   }, { signal })
   return await client.agents.start("peer", { computer, input: fixtureInput({ kind: "hold-secrets", rawDigest: digests[0]!, fileDigest: digests[1]!, protectedDigest: digests[2]! } )}, { signal })
  } catch (error) {
   const cleanup = AbortSignal.timeout(10_000)
   const results = await Promise.allSettled(secretIds.map(id => client.secrets.ref(id).revoke({ idempotencyKey: `cleanup-${id}` }, { signal: cleanup })))
   throw new AggregateError([error, ...results.filter(result => result.status === "rejected").map(result => result.reason)], "Secret fixture admission failed")
  }
 }
 const initialPeerStartedMS = performance.now()
 const initialPeerStartedAt = new Date().toISOString()
 const peer = await startPeer()
 const initialPeerAckMS = performance.now() - initialPeerStartedMS
 sessions.push(peer.session)
 let holding = peer.turn
 let peerSequence = 0
 const discovery: Record<string, unknown>[] = []
 async function progress() {
  for (;;) {
   const page = await peer.session.events.list({ after: peerSequence, limit: 100 }, { signal })
   peerSequence = page.nextAfter
   for (const output of page.records.filter(event => event.turnId === holding.id && event.kind === "turn.output")) {
    if (options.sessionDiscovery) {
     const parts = output.data as { type: string; text?: string }[]
     assert(Array.isArray(parts))
     const text = parts.find(part => part.type === "text")
     assert(text?.text)
     const value = JSON.parse(text.text)
     if (value.discoveryCreated) {
      assert.equal(discovery.length, 0)
      const target = client.sessions.get(value.discoveryCreated)
      assert.equal((await target.retrieve({ signal })).requesterSessionId, peer.session.id)
      await target.close(undefined, { signal })
      continue
     }
     const observed = value.discovery as Record<string, unknown>
     assert.equal(observed["checks"], discovery.length + 1)
     for (const key of ["owned", "requested", "ownedTurn", "requestedTurn"]) {
      assert.equal(typeof observed[key], "string")
      if (discovery.length) assert.equal(observed[key], discovery[0]![key])
     }
     const controls = observed["controls"] as Record<string, unknown>
     assert(controls && typeof controls === "object")
     for (const key of ["interruptedTurn", "resumedTurn", "holdId", "cancelledSession", "cancelledTurn", "cancelledQueuedTurn"]) {
      assert.equal(typeof controls[key], "string")
      if (discovery.length) assert.equal(controls[key], (discovery[0]!["controls"] as Record<string, unknown>)[key])
     }
     const cancelled = client.sessions.get(controls["cancelledSession"] as string)
     const neverStarted = await cancelled.turn(controls["cancelledQueuedTurn"] as string).retrieve({ signal })
     assert.equal(neverStarted.status, "cancelled")
     assert.equal(neverStarted.startedAt, undefined)
     discovery.push(observed)
    }
    if (options.secretBindings) {
     const parts = output.data as { type: string; text?: string }[]
     const text = parts.find(part => part.type === "text"); assert(text?.text)
     const value = JSON.parse(text.text)
     assert.equal(value.secretChecks, secretChecks + 1)
     secretChecks++
    }
    return output
   }
   const status = (await holding.retrieve({ signal })).status
   assert(status === "running" || status === "queued", "peer did not survive continuation")
   await delay(100, undefined, { signal })
  }
 }
 async function finish(turn: TurnRef, provider: string, count: number) {
  const answered = new Set<string>()
  for (;;) {
   await observeEvents(client.sessions.get(turn.sessionId))
   let cursor: string | undefined
   do {
    const page = await turn.asks.list({ ...(cursor ? { cursor } : {}), limit: 100 }, { signal })
    for (const summary of page.asks) {
     if (summary.status !== "pending" || answered.has(summary.id)) continue
     const ask = await turn.asks.get(summary.id, { signal })
     const part = ask.prompt?.find(item => item.type === "json")
     assert.equal(provider, "claude"); assert(part?.type === "json")
     const value = part.value as Record<string, unknown>
     assert.equal(value["provider"], "claude")
     if (options.performanceKind) {
      assert.equal(value["toolName"], "Bash")
      const command = `node /opt/native-performance/workload.mjs ${options.performanceKind} ${count}`
      const input = value["input"] as Record<string, unknown>
      // Claude may remove a redundant cd to its configured Session cwd before
      // asking. Accept only these two spellings of the same isolated workload.
      assert(input["command"] === command || input["command"] === `cd /workspace/${nativeSessions[provider]} && ${command}`)
      assert.deepEqual(input, { command: input["command"], timeout: 120000, description: "Run the isolated performance workload" })
     } else {
      assert.equal(value["toolName"], "mcp__helmr__enqueue")
      assert.deepEqual(value["input"], { sessionId: peer.session.id, input: fixtureInput(`claude-${count * 2 - 1}`), idempotencyKey: `claude-${count * 2 - 1}` })
     }
     await turn.asks.respond(ask.id, { answer: { selected: [{ id: "allow", value: true }] }, responseId: `healthy-${ask.id}` }, { signal })
     answered.add(ask.id)
    }
    cursor = page.nextCursor
   } while (cursor)
   const waiting = await turn.wait({ timeout: "200ms", signal })
   if (waiting.status !== "settled") continue
   const timing = clientTimings.get(turn.id)
   if (timing) timing.settlementObservedMS = performance.now() - timing.requestStartedMS
   assert.equal(waiting.outcome.status, "completed", JSON.stringify(waiting.outcome))
   assert.equal(answered.size, provider === "claude" ? 1 : 0)
   const state = await turn.retrieve({ signal }); assert(state.completionSaveId)
   await observeEvents(client.sessions.get(turn.sessionId))
   if (options.performanceKind) {
    assert(timing && timing.runningObservedMS !== undefined && timing.firstOutputObservedMS !== undefined, "native client timing is incomplete")
   }
   if (options.performanceKind) {
    const prior = turns.findLast(item => item.sessionId === state.sessionId)
    if (prior) {
     assert(state.startedAt && prior.terminalAt)
     assert(Date.parse(state.startedAt) >= Date.parse(prior.terminalAt), "successor started before predecessor settlement")
    }
   }
   const result = state.result as Record<string, unknown>
   assert.equal(result["count"], count); assert.equal(result["setupCount"], 1)
   const previous = native[provider]?.previous
   if (previous) for (const key of ["pid", "nativePid", "nonce", "nativeConversationId", "nativeScopeId"]) assert.equal(result[key], previous[key], `${provider} lost ${key}`)
   if (native[provider]) native[provider]!.previous = result
   turns.push(state); retainedTurns.set(state.id, turn)
   return result
  }
 }
 let peerOutcome: Awaited<ReturnType<TurnRef["wait"]>> | undefined
 try {
  const initialPeerOutput = await progress()
  const initialPeerTiming = { requestStartedAt: initialPeerStartedAt, ackMS: initialPeerAckMS, firstOutputObservedMS: performance.now() - initialPeerStartedMS, turnId: peer.turn.id, outputSequence: initialPeerOutput.sequence }
  const computer = { id: (await peer.session.retrieve({ signal })).computerId }
  const modelInput = options.performanceKind ? { peerSession: peer.session.id, performance: true } : peer.session.id
  const nativeInput = (text: string) => options.performanceKind ? `performance:${options.performanceKind}` : text
  const model = await client.agents.start("models", { input: fixtureInput(modelInput), computer }, { signal })
  sessions.push(model.session)
  const firstModel = await model.turn.wait({ signal }); assert.equal(firstModel.status, "completed")
  const modelState = (await model.turn.retrieve({ signal })).result as Record<string, unknown>
  // Identical warm-up under both policies lets ordinary background saving occur
  // while the peer is doing real I/O, outside native handler timing intervals.
  if (options.performanceKind) await delay(12000, undefined, { signal })
  for (const provider of ["codex", "claude"]) {
   const startMS = performance.now(), startAt = new Date().toISOString()
   const started = await client.agents.start(provider, { input: fixtureInput(nativeInput(`${provider} before idle one`)), computer }, { signal })
   admitted(provider, "initial", started.turn, startMS, startAt)
   sessions.push(started.session); nativeSessions[provider] = started.session.id
   const enqueueMS = performance.now(), enqueueAt = new Date().toISOString()
   const queued = await started.session.enqueue(fixtureInput(nativeInput(`${provider} before idle two`)), undefined, { signal })
   admitted(provider, "queued-successor", queued, enqueueMS, enqueueAt)
   if (options.performanceKind) {
    const state = await queued.retrieve({ signal })
    assert.equal(state.status, "queued"); assert.equal(state.startedAt, undefined)
   }
   const first = await finish(started.turn, provider, 1)
   native[provider] = { session: started.session, previous: first }
   await finish(queued, provider, 2)
  }
  let priorCheckpoint = ""
  let priorPeer: Record<string, unknown> | undefined
  for (let cycle = 1; cycle <= 2; cycle++) {
   const releaseStartedMS = performance.now()
   const park = await model.session.enqueue(fixtureInput("park"), undefined, { signal })
   assert.equal((await park.wait({ signal })).status, "completed")
   assert.deepEqual((await park.retrieve({ signal })).result, { parked: true, nonce: modelState["nonce"], pid: modelState["pid"], count: cycle })
   await holding.send(fixtureInput("release"), undefined, { signal })
   peerOutcome = await holding.wait({ signal }); assert.equal(peerOutcome.status, "completed")
   const peerState = (await holding.retrieve({ signal })).result as Record<string, unknown>
   if (priorPeer) { for (const key of ["pid", "nonce"]) assert.equal(peerState[key], priorPeer[key]); assert(Number(peerState["count"]) > Number(priorPeer["count"])) }
   priorPeer = peerState
   let checkpoint: { computerId: string; checkpointId: string; sourceEpoch: number; sourceFenced: boolean; observedAt: string; unfencedLeases: number; status: string; runtimeObjects: { role: string; digest: string; sizeBytes: number }[] }
   for (;;) {
    let rows: typeof checkpoint[] = []
    try { rows = JSON.parse(await readFile(options.checkpointObservations, "utf8")) }
    catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error }
    const ready = rows.find(row => row.computerId === computer.id && row.checkpointId !== priorCheckpoint && row.status === "ready" && row.sourceFenced)
    if (ready) { checkpoint = ready; break }
    await delay(250, undefined, { signal })
   }
   assert.deepEqual(checkpoint.runtimeObjects.map(object => object.role).sort(), ["memory", "scratch_disk", "vm_config", "vm_state"])
   priorCheckpoint = checkpoint.checkpointId
   if (options.secretBindings && cycle === 1) {
    for (const id of secretIds) {
     const rotated = await client.secrets.ref(id).rotate({ value: randomBytes(32).toString("hex"), idempotencyKey: `rotate-${id}` }, { signal })
     assert(rotated.rotatedAt)
    }
   }
   process.stderr.write(JSON.stringify({ healthyCheckpointReleased: { cycle, ...checkpoint } }) + "\n")
   if (options.crossHost && cycle === 1) {
    assert(options.sourceHostId && options.targetHostId && options.sourceHostId !== options.targetHostId)
    await writeFile(options.checkpointObservations + ".handoff-request.new", JSON.stringify({ checkpointId: checkpoint.checkpointId }), { flag: "wx", mode: 0o600 })
    await rename(options.checkpointObservations + ".handoff-request.new", options.checkpointObservations + ".handoff-request")
    let handoff: { checkpointId: string; sourceResourceId: string; targetResourceId: string; sourceWorkerJoined: boolean; sourceObservationExpired: boolean }
    for (;;) {
     try { handoff = JSON.parse(await readFile(options.checkpointObservations + ".handoff-receipt", "utf8")); break }
     catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error }
     await delay(100, undefined, { signal })
    }
    assert.equal(handoff.checkpointId, checkpoint.checkpointId)
    assert.equal(handoff.sourceResourceId, options.sourceHostId)
    assert.equal(handoff.targetResourceId, options.targetHostId)
    assert.equal(handoff.sourceWorkerJoined, true)
    assert.equal(handoff.sourceObservationExpired, true)
    process.stderr.write(JSON.stringify({ crossHostHandoff: handoff }) + "\n")
   }
   if (options.checkpointFault) {
    await writeFile(options.checkpointObservations + ".fault-request.new", JSON.stringify({ checkpointId: checkpoint.checkpointId }), { flag: "wx", mode: 0o600 })
    await rename(options.checkpointObservations + ".fault-request.new", options.checkpointObservations + ".fault-request")
    let fault: { checkpointId: string; kind: string; role: string; originalDigest: string; corruptedDigest?: string; deleteMarkerVersionId?: string; observedAbsent?: boolean }
    for (;;) {
     try { fault = JSON.parse(await readFile(options.checkpointObservations + ".fault-receipt", "utf8")); break }
     catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error }
     await delay(100, undefined, { signal })
    }
    assert.equal(fault.checkpointId, checkpoint.checkpointId)
    assert.equal(fault.kind, options.checkpointFault)
    assert.equal(fault.role, options.checkpointFault === "corrupt-config" ? "vm_config" : "memory")
    assert.equal(fault.originalDigest, checkpoint.runtimeObjects.find(object => object.role === fault.role)?.digest)
    if (options.checkpointFault === "corrupt-config") {
     assert(fault.corruptedDigest)
     assert.notEqual(fault.originalDigest, fault.corruptedDigest)
    } else { assert(fault.deleteMarkerVersionId); assert.equal(fault.observedAbsent, true) }
    const queued = await Promise.all(["codex", "claude"].map(provider => native[provider]!.session.enqueue(fixtureInput(`${provider} held after checkpoint loss`), undefined, { signal })))
    const holds: Record<string, string> = {}
    for (const session of sessions) {
     for (;;) {
      const state = await session.retrieve({ signal })
      if (state.holds.length) {
       assert.equal(state.holds.length, 1)
       const hold = state.holds[0]!
       assert.equal(hold.scope, "local")
       assert.equal(hold.reason, "Computer execution authority lost without an eligible continuation")
       holds[session.id] = hold.id
       break
      }
      await delay(100, undefined, { signal })
     }
    }
    // Observe the held state for a bounded 20-second window. Never resume the holds:
    // this case proves no automatic reconstruction or accepted-input replay.
    const heldAt = performance.now()
    for (let sample = 0; sample < 20; sample++) {
     for (const turn of queued) assert.equal((await turn.retrieve({ signal })).status, "queued")
     for (const session of sessions) assert.deepEqual((await session.retrieve({ signal })).holds.map(hold => hold.id), [holds[session.id]])
     for (const state of turns) assert.deepEqual(await retainedTurns.get(state.id)!.retrieve({ signal }), state)
     await delay(1000, undefined, { signal })
    }
    process.stdout.write(JSON.stringify({ computerId: computer.id, peerSessionId: peer.session.id, peerTurnId: holding.id, peerSequence, peerOutcome, nativeSessions, nativeLosses: [], turns,
     checkpointLoss: { checkpoint, fault, holds, queuedTurns: await Promise.all(queued.map(turn => turn.retrieve({ signal }))), heldMs: performance.now() - heldAt } }) + "\n")
    return
   }
   // The long-idle case observes two hours with no unfenced lease before resuming.
   const releaseRequestToCheckpointObservedMS = performance.now() - releaseStartedMS
   const requestedIdleMs = cycle === 1 ? (options.longIdle ? 2 * 60 * 60_000 : 65_000) : 5_000
   const idleStartedAt = new Date().toISOString()
   const idleStarted = performance.now()
   let idleSamples = 0
   do {
    await delay(Math.min(30_000, Math.max(1, requestedIdleMs - (performance.now() - idleStarted))), undefined, { signal })
    if (options.longIdle) {
     const rows: typeof checkpoint[] = JSON.parse(await readFile(options.checkpointObservations, "utf8"))
     const current = rows.find(row => row.checkpointId === checkpoint.checkpointId)
     assert(current && current.status === "ready" && current.sourceFenced && current.unfencedLeases === 0, "idle checkpoint lost its released boundary")
     const age = Date.now() - Date.parse(current.observedAt)
     assert(Number.isFinite(age) && age >= -1000 && age < 5000, "idle checkpoint observation is stale")
     for (const state of turns) assert.deepEqual(await retainedTurns.get(state.id)!.retrieve({ signal }), state)
     idleSamples++
    }
   } while (performance.now() - idleStarted < requestedIdleMs)
   const idleMs = performance.now() - idleStarted
   const idleFinishedAt = new Date().toISOString()
   process.stderr.write(JSON.stringify({ healthyIdle: { cycle, checkpointId: checkpoint.checkpointId, requestedIdleMs, idleMs, idleStartedAt, idleFinishedAt, idleSamples } }) + "\n")
   let keyFault: Record<string, unknown> | undefined
   let resumeTiming: { requestStartedAt: string; requestStartedMS: number; ackMS: number; turnId: string; firstOutputObservedMS?: number; outputSequence?: number } | undefined
   if (options.checkpointKeyMismatch && cycle === 1) {
    async function requestKeyPhase(phase: string) {
     const path = options.checkpointObservations + ".key-" + phase
     await writeFile(path + ".new", JSON.stringify({ checkpointId: checkpoint.checkpointId }), { flag: "wx", mode: 0o600 })
     await rename(path + ".new", path)
    }
    async function keyReceipt(phase: string): Promise<Record<string, unknown>> {
     for (;;) {
      try {
       const receipt = JSON.parse(await readFile(options.checkpointObservations + ".key-" + phase, "utf8")) as Record<string, unknown>
       assert.equal(receipt["checkpointId"], checkpoint.checkpointId)
       return receipt
      } catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error }
      await delay(100, undefined, { signal })
     }
    }
    await requestKeyPhase("request")
    await keyReceipt("wrong-ready")
    holding = await peer.session.enqueue(fixtureInput(options.performanceKind ? "hold-performance" : "hold"), undefined, { signal })
    keyFault = await keyReceipt("fault")
    const pausedAt = performance.now()
    for (let sample = 0; sample < 20; sample++) {
     assert.equal((await holding.retrieve({ signal })).status, "queued")
     for (const session of sessions) assert.deepEqual((await session.retrieve({ signal })).holds, [])
     for (const state of turns) assert.deepEqual(await retainedTurns.get(state.id)!.retrieve({ signal }), state)
     // The pause receipt and checkpoint sampler are independent readers. Require
     // a sample taken after this check starts, then assert its state unchanged.
     const sampleStarted = Date.now()
     let current: typeof checkpoint | undefined
     for (;;) {
      const rows: typeof checkpoint[] = JSON.parse(await readFile(options.checkpointObservations, "utf8"))
      current = rows.find(row => row.checkpointId === checkpoint.checkpointId)
      if (current && Date.parse(current.observedAt) >= sampleStarted) break
      await delay(50, undefined, { signal })
     }
     assert(current.status === "ready" && current.sourceFenced && current.unfencedLeases === 0)
     assert.deepEqual(current.runtimeObjects.toSorted((a, b) => a.role.localeCompare(b.role)), checkpoint.runtimeObjects.toSorted((a, b) => a.role.localeCompare(b.role)))
     await delay(1000, undefined, { signal })
    }
    keyFault["pausedMs"] = performance.now() - pausedAt
    await requestKeyPhase("repair-request")
    keyFault["repaired"] = await keyReceipt("repaired")
   } else {
    const started = performance.now(), at = new Date().toISOString()
    holding = await peer.session.enqueue(fixtureInput(options.performanceKind ? "hold-performance" : "hold"), undefined, { signal })
    resumeTiming = { requestStartedAt: at, requestStartedMS: started, ackMS: performance.now() - started, turnId: holding.id }
   }
   const resumedOutput = await progress()
   if (resumeTiming) {
    resumeTiming.firstOutputObservedMS = performance.now() - resumeTiming.requestStartedMS
    resumeTiming.outputSequence = resumedOutput.sequence
   }
   const modelTurn = await model.session.enqueue(fixtureInput(modelInput), undefined, { signal })
   const modelOutcome = await modelTurn.wait({ signal }); assert.equal(modelOutcome.status, "completed")
   const restoredModel = (await modelTurn.retrieve({ signal })).result as Record<string, unknown>
   for (const key of ["pid", "nonce"]) assert.equal(restoredModel[key], modelState[key], `model ${key} changed`)
   assert.equal(restoredModel["count"], cycle + 1)
   const before = Object.fromEntries(Object.entries(native).map(([provider, value]) => [provider, value.previous]))
   for (const provider of ["codex", "claude"]) {
    const started = performance.now(), at = new Date().toISOString()
    const turn = await native[provider]!.session.enqueue(fixtureInput(nativeInput(`${provider} after idle ${cycle}`)), undefined, { signal })
    admitted(provider, `after-resume-${cycle}`, turn, started, at)
    await finish(turn, provider, cycle + 2)
   }
   const evidence = { cycle, checkpoint, keyFault, releaseRequestToCheckpointObservedMS, resumeTiming, requestedIdleMs, idleMs, idleStartedAt, idleFinishedAt, idleSamples, idleAndResumeMs: performance.now() - idleStarted, before,
    after: Object.fromEntries(Object.entries(native).map(([provider, value]) => [provider, value.previous])), model: restoredModel }
   continuations.push(evidence)
   process.stderr.write(JSON.stringify({ healthyContinuation: evidence }) + "\n")
  }
  if (options.sessionDiscovery) assert.equal(discovery.length, 3)
  if (options.secretBindings) {
   assert.equal(secretChecks, 3)
   const pending = await peer.session.enqueue(fixtureInput("retained-after-revocation"), undefined, { signal })
   assert.equal((await pending.retrieve({ signal })).status, "queued")
   assert.equal((await holding.retrieve({ signal })).status, "running")
   assert.deepEqual((await peer.session.retrieve({ signal })).holds, [])
   assert.equal((await client.secrets.ref(secretIds[0]!).revoke({ idempotencyKey: "revoke-exposed-raw" }, { signal })).status, "revoked")
   peerOutcome = await holding.wait({ signal }); assert.equal(peerOutcome.status, "interrupted")
   const held = await peer.session.retrieve({ signal })
   assert.equal(held.holds.length, 1)
   assert.equal(held.holds[0]!.sessionId, peer.session.id)
   assert.equal(held.holds[0]!.scope, "local")
   assert.equal(held.holds[0]!.reason, "Computer exposed to a revoked Secret")
   for (let sample = 0; sample < 10; sample++) {
    const queued = await pending.retrieve({ signal })
    assert.equal(queued.status, "queued"); assert.equal(queued.startedAt, undefined)
    assert.deepEqual((await peer.session.retrieve({ signal })).holds, held.holds)
    await delay(100, undefined, { signal })
   }
   await peer.session.cancel({ idempotencyKey: "cancel-revoked-queue" }, { signal })
   assert.equal((await pending.wait({ signal })).status, "cancelled")
   secretRevocation = { interruptedTurnId: holding.id, retainedTurnId: pending.id, holds: held.holds, explicitlyCancelled: true }
  } else {
   await holding.send(fixtureInput("release"), undefined, { signal }); peerOutcome = await holding.wait({ signal }); assert.equal(peerOutcome.status, "completed")
  }
  assert.equal(new Set(turns.map(turn => turn.completionSaveId)).size, turns.length)
  for (const state of turns) {
   const retained = retainedTurns.get(state.id)!
   assert.deepEqual(await retained.retrieve({ signal }), state)
  }
  process.stdout.write(JSON.stringify({ computerId: computer.id, peerSessionId: peer.session.id, peerTurnId: holding.id, peerSequence, peerOutcome, nativeSessions, nativeLosses: [], turns, continuations, discovery, ...(options.performanceKind ? { firstOutputSemantics: "first authored workload result before handler return; includes native operation and client observation delay", initialPeerTiming, clientTimings: [...clientTimings.values()], nativeEvents } : {}), ...(options.secretBindings ? { secretBindings: { ids: secretIds, checks: secretChecks, rotatedWhileReleased: true, revocation: secretRevocation } } : {}) }) + "\n")
 } finally {
  const cleanupSignal = AbortSignal.timeout(10_000)
  const results: PromiseSettledResult<unknown>[] = await Promise.allSettled(sessions.map(session => session.cancel(undefined, { signal: cleanupSignal })))
  results.push(...await Promise.allSettled(secretIds.map(async id => { await client.secrets.ref(id).revoke({ idempotencyKey: `cleanup-${id}` }, { signal: cleanupSignal }) })))
  const failures = results.filter(result => result.status === "rejected")
  if (failures.length) throw new AggregateError(failures.map(result => result.reason), "healthy fixture cleanup failed")
 }
}
