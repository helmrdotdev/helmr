import { runProtectedStarters } from "./protected-starters-driver"
import { runEmbeddedStarter } from "./embedded-starters-driver"
import { runClaudeStarter } from "./claude-starter-driver"
import { runCodexStarter } from "./codex-starter-driver"
import { runNativeSDK } from "../native-sdk/driver"
import { fixtureInput } from "../../e2e/support/runtime-mcp"
// Invoked by the integrated fixture after the ordinary deployment is available.
// API credentials stay in its private input file; stdout contains only evidence.
import assert from "node:assert/strict"
import { readFile, writeFile, rename } from "node:fs/promises"
import { setTimeout as delay } from "node:timers/promises"
import { HelmrClient } from "../../../sdk/typescript/src/client"
import type { SessionRef } from "../../../sdk/typescript/src/contract"
import type { SessionEvent, TurnState, TurnRef, TurnOutcome } from "../../../sdk/typescript/src/contract"
import { runHealthyContinuation } from "./healthy-driver"
import type { CommandRef } from "../../../sdk/typescript/src/command"

const options = JSON.parse(await readFile(process.argv[2]!, "utf8")) as {
  protectedModel?: { origin: string; state: string }; url: string; apiKey: string; modelScript: string; timeoutMs: number; nativeProcessLoss?: boolean; cancelSave?: boolean; deadlineSave?: boolean; evidence: string; healthyRAM?: boolean; sessionDiscovery?: boolean; secretBindings?: boolean; longIdle?: boolean; checkpointKeyMismatch?: boolean; checkpointFault?: "corrupt-config" | "corrupt-memory" | "missing-memory"; checkpointObservations: string; performanceKind?: "no-op" | "edit-test" | "dependencies"; backgroundSaves?: boolean; crossHost?: boolean; sourceHostId?: string; targetHostId?: string
}
assert(Number.isSafeInteger(options.timeoutMs) && options.timeoutMs > 0)
if (options.protectedModel) {
  const client = new HelmrClient({ url: options.url, apiKey: options.apiKey })
  const protectedStarters = await runProtectedStarters(client, options.protectedModel, options.evidence, AbortSignal.timeout(options.timeoutMs))
  await new Promise<void>((resolve, reject) => {
    process.stdout.write(JSON.stringify({ protectedStarters }) + "\n", error => error ? reject(error) : resolve())
  })
  process.exit(0)
}
if (options.healthyRAM || options.checkpointFault) { await runHealthyContinuation(options); process.exit(0) }
const signal = AbortSignal.timeout(options.timeoutMs)
const client = new HelmrClient({ url: options.url, apiKey: options.apiKey })
const sessions: SessionRef[] = []
const peer = await client.sessions.create({ agent: "peer", input: fixtureInput("hold" ) }, { signal })
sessions.push(peer.session)
let model: CommandRef | undefined
let activeOutcome: { controller: AbortController; promise: Promise<TurnOutcome> } | undefined

async function progress(after: number, allowQueued = false): Promise<number> {
  for (;;) {
    const page = await peer.session.events.list({ after, limit: 100 }, { signal })
    const output = page.records.find(event => event.turnId === peer.turn.id && event.kind === "turn.output")
    const status = (await peer.turn.get({ signal })).status
    assert(status === "running" || (allowQueued && status === "queued"), "peer stopped before native completion")
    if (output) return output.sequence
    after = page.nextAfter
    if (!page.hasMore) await delay(100, undefined, { signal })
  }
}

async function nativeOutcome(turn: TurnRef, provider: string, count: number, outcomeSignal: AbortSignal = signal): Promise<TurnOutcome> {
  const answered = new Set<string>()
  for (;;) {
    let cursor: string | undefined
    do {
      const page = await turn.asks.list({ cursor, limit: 100 }, { signal: outcomeSignal })
      for (const summary of page.items) {
        if (summary.status !== "pending" || answered.has(summary.id)) continue
        const ask = await turn.asks.get(summary.id, { signal: outcomeSignal })
        assert.equal(provider, "claude", "unexpected native approval")
        assert.equal(ask.turnId, turn.id)
        const action = ask.question?.prompt.find(part => part.type === "json")
        assert(action?.type === "json")
        const value = action.value as Record<string, unknown>
        assert.equal(value["provider"], "claude")
        assert.equal(value["toolName"], "mcp__helmr__enqueue")
        assert.deepEqual(value["input"], {
          sessionId: peer.session.id, input: fixtureInput(`claude-${count * 2 - 1}`), idempotencyKey: `claude-${count * 2 - 1}`,
        })
        await turn.asks.answer(ask.id, {
          answer: { selectedIds: ["allow"] }, answerId: `fixture-allow-${ask.id}`,
        }, { signal: outcomeSignal })
        answered.add(ask.id)
      }
      cursor = page.nextCursor
    } while (cursor)
    const result = await turn.wait({ timeout: "200ms", signal: outcomeSignal })
    if (result.status === "settled") {
      assert.equal(answered.size, provider === "claude" ? 1 : 0, "native approval count")
      return result.outcome
    }
  }
}

async function waitPublished(states: readonly TurnState[]):Promise<void>{
 for(const turn of states){for(;;){
  try{const receipt=JSON.parse(await readFile(`${options.evidence}/turn-published-${turn.id}.json`,"utf8"));assert.equal(receipt.turnId,turn.id);break}
  catch(error){if((error as NodeJS.ErrnoException).code!=="ENOENT")throw error}
  await delay(50,undefined,{signal})
 }}
}

try {
  let peerSequence = await progress(0, true)
  const computerID = (await peer.session.get({ signal })).computerId
  assert(computerID, "running peer must be placed on a Computer")
  const computer = { id: computerID }
  const commands = client.computers.ref(computer.id)
  if (options.backgroundSaves) {
    const waiting = performance.now()
    for (;;) {
      let saves: { saveId: string; computerId: string; peerSessionId: string; peerTurnId: string; leaseEpoch: number; sequence: number; peerCounter: number; requestedAt: string; capturedAt: string; observedAt: string; pendingSaves: number; retainedSaves: number; retainedRoots: number; pinnedBytes: number }[] = []
      try { saves = JSON.parse(await readFile(`${options.evidence}/background-publications.json`, "utf8")) }
      catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error }
      if (saves.length === 3) {
        assert.equal(new Set(saves.map(save => save.saveId)).size, 3)
        for (const [index, save] of saves.entries()) {
          assert.equal(save.computerId, computer.id)
          assert.equal(save.peerSessionId, peer.session.id)
          assert.equal(save.peerTurnId, peer.turn.id)
          assert.equal(save.pendingSaves, 0)
          assert(save.peerCounter > 0 && save.retainedSaves >= index + 1 && save.retainedRoots > 0 && save.pinnedBytes > 0)
          assert(Date.parse(save.requestedAt) <= Date.parse(save.capturedAt))
          assert(Date.parse(save.capturedAt) <= Date.parse(save.observedAt))
          if (index > 0) {
            const previous = saves[index - 1]!
            assert.equal(save.leaseEpoch, previous.leaseEpoch)
            assert.equal(save.sequence, previous.sequence + 1)
            assert(save.peerCounter > previous.peerCounter, "peer stopped progressing between periodic publications")
          }
        }
        assert.equal((await peer.turn.get({ signal })).status, "running")
        await writeFile(`${options.evidence}/background-before-native.json`, JSON.stringify({ saves, waitedMs: performance.now() - waiting, peerStatus: "running" }) + "\n", { mode: 0o600 })
        peerSequence = await progress(peerSequence)
        break
      }
      assert(saves.length < 3)
      peerSequence = await progress(peerSequence)
      await delay(200, undefined, { signal })
    }
  }
  const modelStarted = performance.now()
  model = await commands.exec({
    command: ["/usr/bin/env", `HELMR_NATIVE_TARGET_SESSION=${peer.session.id}`, "/usr/local/bin/node", "--input-type=module", "-", "/workspace/native-models.json"],
    stdin: await readFile(options.modelScript),
    timeout: `${(process.env.HELMR_NATIVE_SDK_PROOF === "1" || process.env.HELMR_CODEX_STARTER_PROOF === "1" || process.env.HELMR_CLAUDE_STARTER_PROOF === "1" || process.env.HELMR_EMBEDDED_STARTER_PROOF === "1") ? 600000 : options.timeoutMs}ms`,
    idempotencyKey: "fixture-model",
  }, { signal })
  const ready = await commands.exec({
    command: ["/usr/local/bin/node", "--input-type=module", "-e", `
      import {readFile} from 'node:fs/promises';
      import {setTimeout as delay} from 'node:timers/promises';
      const reported=new Set();
      process.stderr.write('model readiness probe started\\n');
      for (;;) {
        try {
          const p=JSON.parse(await readFile('/workspace/native-models.json','utf8'));
          if (!['codex','claude','proof'].every(k=>Number.isInteger(p[k])&&p[k]>0&&p[k]<65536)) throw Error('ports');
          if (!(await fetch('http://127.0.0.1:'+p.proof)).ok) throw Error('model');
          break;
        } catch (error) {
          const diagnostic=JSON.stringify({message:String(error),cause:String(error?.cause ?? '')});
          if (reported.size<8 && !reported.has(diagnostic)) {
            reported.add(diagnostic);
            process.stderr.write(diagnostic+'\\n');
          }
          await delay(100);
        }
      }
    `],
    timeout: "180s", idempotencyKey: "fixture-model-ready",
  }, { signal })
  const readiness = await ready.wait({ signal })
  process.stderr.write(JSON.stringify({ modelStartupMs: performance.now() - modelStarted, readiness }) + "\n")
  if (readiness.kind !== "exited" || readiness.exitCode !== 0) {
    const diagnosticSignal = AbortSignal.timeout(5_000)
    const diagnostics = await Promise.allSettled([
      model.get({ signal: diagnosticSignal }),
      ready.get({ signal: diagnosticSignal }),
    ])
    process.stderr.write(JSON.stringify({ modelReadiness: diagnostics.map(result =>
      result.status === "rejected" ? { status: result.status, reason: String(result.reason) } : result,
    ) }) + "\n")
  }
  if (readiness.kind !== "exited" || readiness.exitCode !== 0) {
    const probeSignal = AbortSignal.timeout(10_000)
    try {
      const probe = await commands.exec({
        command: ["/bin/sh", "-c", `
          echo 'node startup probe: shell running' >&2
          printf 'entropy bits: ' >&2
          cat /proc/sys/kernel/random/entropy_avail >&2
          /usr/local/bin/node -e 'console.error("probe CJS start",process.version); const fs=require("node:fs"); fs.statSync("/workspace"); console.error("probe sync fs ready"); fs.promises.stat("/workspace").then(()=>console.error("probe async fs ready")); import("node:fs/promises").then(()=>console.error("probe dynamic import ready"));' &
          common=$!
          /usr/local/bin/node --input-type=module -e 'console.error("probe bare ESM ready")' &
          module=$!
          /usr/local/bin/node --input-type=module -e 'import {readFile} from "node:fs/promises"; console.error("probe imported ESM ready")' &
          imported=$!
          sleep 1
          for child in "$common" "$module" "$imported"; do
            if [ -d /proc/$child ]; then
              state=$(count=0; for thread in /proc/$child/task/*; do
                count=$((count+1)); [ "$count" -le 8 ] || break
                printf '%s wait: ' "$thread"; cat "$thread/wchan"
                printf ' syscall: '; cat "$thread/syscall" || true
                printf '\\n'
              done)
              printf '%s\\n' "$state" >&2
            fi
          done
          wait "$common"; common_status=$?
          wait "$module"; module_status=$?
          wait "$imported"; imported_status=$?
          echo "probe statuses: $common_status $module_status $imported_status" >&2
                `], timeout: "5s", idempotencyKey: "fixture-node-startup-probe",
      }, { signal: probeSignal })
      process.stderr.write(JSON.stringify({ nodeStartupProbe: await probe.wait({ signal: probeSignal }) }) + "\n")
    } catch (error) {
      process.stderr.write(JSON.stringify({ nodeStartupProbeError: String(error) }) + "\n")
    }
  }
  assert.equal(readiness.kind, "exited")
  assert.equal(readiness.exitCode, 0)
  const turns: TurnState[] = []
  const timings: Record<string, unknown>[] = []
  const nativeSessions: Record<string, string> = {}
  const nativeLosses: Record<string, unknown>[] = []
  const cancellations: Record<string, unknown>[] = []
  let deadline: Record<string, string> | undefined
  for (const provider of ["codex", "claude"]) {
    const firstInput = options.nativeProcessLoss ? `${provider} unique pre-loss input one` : "first"
    const secondInput = options.nativeProcessLoss ? `${provider} unique pre-loss input two` : "second"
    const startAt = performance.now()
    const started = await client.sessions.create({ agent: provider, input: fixtureInput(firstInput), computer }, { signal })
    const startAckMs = performance.now() - startAt
    sessions.push(started.session)
    nativeSessions[provider] = started.session.id
    // Approval handling must run while the client observes output and saving.
    const outcomeController = new AbortController()
    const observingSignal = AbortSignal.any([signal, outcomeController.signal])
    const firstOutcome = nativeOutcome(started.turn, provider, 1, observingSignal)
    activeOutcome = { controller: outcomeController, promise: firstOutcome }
    const failObservation = (reason: unknown) => {
      process.stderr.write(JSON.stringify({ nativeOutcomeFailure: reason instanceof Error ? reason.stack : String(reason) }) + "\n")
      outcomeController.abort(reason)
    }
    void firstOutcome.then(outcome => {
      if (outcome.status !== "completed") failObservation(new Error(JSON.stringify(outcome)))
    }, failObservation)
    const disconnected = new AbortController()
    const stream = started.session.events.stream({}, { signal: AbortSignal.any([observingSignal, disconnected.signal]) })
    let lastConsumedSequence = 0
    let observedOutput: SessionEvent | undefined
    for await (const event of stream) {
      assert(event.sequence > lastConsumedSequence)
      lastConsumedSequence = event.sequence
      if (event.turnId === started.turn.id && event.kind === "turn.output") {
        observedOutput = event
        disconnected.abort()
        await assert.rejects(stream.next(), { name: "AbortError" })
        break
      }
    }
    assert(observedOutput, "SDK stream did not expose output before settlement")
    let held: { sessionId: string; turnId: string; saveId: string }
    for (;;) {
      try { held = JSON.parse(await readFile(`${options.evidence}/client-held-${started.turn.id}.json`, "utf8")); break }
      catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error }
      await delay(50, undefined, { signal: observingSignal })
    }
    assert.equal(held.sessionId, started.session.id)
    assert.equal(held.turnId, started.turn.id)
    const reconnected = new HelmrClient({ url: options.url, apiKey: options.apiKey }).sessions.ref(started.session.id)
    const restoredState = await reconnected.turns.ref(started.turn.id).get({ signal: observingSignal })
    assert.equal(restoredState.status, "completed")
    const retained = await reconnected.events.list({ after: observedOutput.sequence - 1, limit: 1 }, { signal: observingSignal })
    assert.deepEqual(retained.records[0], observedOutput, "reconnect changed retained output")
    const enqueueAt = performance.now()
    const following = await reconnected.enqueue(fixtureInput(secondInput), undefined, { signal: observingSignal })
    const enqueueAckMs = performance.now() - enqueueAt
    const predecessorAfterFollowupAck = (await reconnected.turns.ref(started.turn.id).get({ signal: observingSignal })).status
    assert.equal(predecessorAfterFollowupAck, "completed", "follow-up ACK was not observed during saving")
    assert(["queued","running","completed"].includes((await following.get({signal:observingSignal})).status),"accepted follow-up state")
    const resumedSequences: number[] = []
    for await (const event of reconnected.events.stream({ after: lastConsumedSequence }, { signal: observingSignal })) {
      assert(event.sequence > (resumedSequences.at(-1) ?? lastConsumedSequence), "reconnect replayed a consumed event")
      resumedSequences.push(event.sequence)
      if (event.turnId === started.turn.id && event.kind === "turn.completed") break
    }
    const reconnectProof = { ...held, followingId: following.id, lastConsumedSequence, resumedSequences, enqueueAckMs, predecessorAfterFollowupAck }
    const reconnectPath = `${options.evidence}/client-resumed-${started.turn.id}.json`
    await writeFile(`${reconnectPath}.tmp`, JSON.stringify(reconnectProof) + "\n", { mode: 0o600 })
    await rename(`${reconnectPath}.tmp`, reconnectPath)
    let previous: Record<string, unknown> | undefined
    let previousTerminal: string | undefined
    for (let count = 1; count <= 2; count++) {
      const turn = count === 1 ? started.turn : following
      const outcome = await (count === 1 ? firstOutcome : nativeOutcome(turn, provider, count))
      assert.equal(outcome.status, "completed", JSON.stringify(outcome))
      const state = await turn.get({ signal })
      const result = state.result as Record<string, unknown>
      assert.equal(result["count"], count)
      assert.equal(result["setupCount"], 1)
      assert.equal(typeof result["nonce"], "string")
      assert(Number.isSafeInteger(result["pid"]) && Number.isSafeInteger(result["nativePid"]))
      if (previous) {
        for (const key of ["nonce", "nativePid"]) assert.equal(result[key], previous[key], `native ${key} changed`)
      }
      previous = result
      const timeline: SessionEvent[] = []
      let after = 0
      for (;;) {
        const page = await started.session.events.list({ after, limit: 100 }, { signal })
        timeline.push(...page.records.filter(event => event.turnId === turn.id))
        if (!page.hasMore) break
        after = page.nextAfter
      }
      const time = (kind: SessionEvent["kind"]): number => {
        const event = timeline.find(event => event.kind === kind)
        assert(event, `missing native ${kind} event`)
        return Date.parse(event.createdAt)
      }
      assert(state.startedAt && state.terminalAt)
      const nextDispatchAfterPreviousTerminalMs = previousTerminal === undefined ? undefined : Date.parse(state.startedAt) - Date.parse(previousTerminal)
      if (nextDispatchAfterPreviousTerminalMs !== undefined) assert(nextDispatchAfterPreviousTerminalMs >= 0, "follow-up dispatched before predecessor settlement")
      previousTerminal = state.terminalAt
      const timing = { provider, turnId: turn.id, sequence: count,
        admissionAckMs: count === 1 ? startAckMs : enqueueAckMs,
        ...(count === 1 ? { predecessorAfterFollowupAck } : {}),
        queuedToRunningMs: time("turn.running") - time("turn.queued"),
        runningToFirstOutputMs: time("turn.output") - time("turn.running"),
        firstOutputToCompletedMs: time("turn.completed") - time("turn.output"),
        nextDispatchAfterPreviousTerminalMs }
      timings.push(timing)
      process.stderr.write(JSON.stringify({ nativeTurnCompleted: timing }) + "\n")
      // Advance past all output observed at completion, then require new output
      // while the same peer Turn is still running.
      for (;;) {
        const page = await peer.session.events.list({ after: peerSequence, limit: 100 }, { signal })
        peerSequence = page.nextAfter
        if (!page.hasMore) break
      }
      peerSequence = await progress(peerSequence)
      turns.push(state)
    }
    await waitPublished(turns.filter(turn=>turn.sessionId===started.session.id))
    if (options.cancelSave) {
      const cancelled = await started.session.enqueue(fixtureInput("cancel during required save"), undefined, { signal })
      const proofPath = `${options.evidence}/cancel-held-${cancelled.id}.json`
      const readProof = async (path: string): Promise<Record<string, string>> => {
        for (;;) {
          try { return JSON.parse(await readFile(path, "utf8")) }
          catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error }
          await delay(50, undefined, { signal })
        }
      }
      const cancelHeld = async () => {
        const held = await readProof(proofPath)
        assert.equal(held["turnId"], cancelled.id)
        assert.equal(held["sessionId"], started.session.id)
        assert(held["saveId"])
        assert.equal((await cancelled.get({ signal })).status, "completed")
        await started.session.interrupt(undefined,{signal})
        const queued = await started.session.enqueue(fixtureInput("must not dispatch after cancellation"), undefined, { signal })
        assert.equal((await queued.get({ signal })).status, "queued")
        await started.session.cancel(undefined, { signal })
        const state = await cancelled.get({ signal })
        assert.equal(state.status, "completed")
        assert(state.result !== undefined)
        const next = await queued.get({ signal })
        assert.equal(next.status, "cancelled")
        assert.equal(next.startedAt, undefined)
        for (const completed of turns.filter(turn => turn.sessionId === started.session.id)) {
          const current = await started.session.turns.ref(completed.id).get({ signal })
          assert.equal(current.status, "completed")
          assert.deepEqual(current.result, completed.result)
        }
        // Observe fresh peer work while publication is still held and cancellation
        // is already authoritative, then allow the original Save request through.
        for (;;) {
          const page = await peer.session.events.list({ after: peerSequence, limit: 100 }, { signal })
          peerSequence = page.nextAfter
          if (!page.hasMore) break
        }
        peerSequence = await progress(peerSequence)
        const proof = { ...held, queuedTurnId: queued.id }
        const release = `${options.evidence}/cancel-release-${cancelled.id}.json`
        await writeFile(release + ".new", JSON.stringify(proof), { mode: 0o600 })
        await rename(release + ".new", release)
        assert.deepEqual(await readProof(`${options.evidence}/cancel-published-${cancelled.id}.json`), proof)
        const after = await cancelled.get({ signal })
        assert.equal(after.status, "completed")
        assert.deepEqual(after.result,state.result)
        return { ...proof, provider }
      }
      const [outcome, proof] = await Promise.all([nativeOutcome(cancelled, provider, 3), cancelHeld()])
      assert.equal(outcome.status, "completed")
      cancellations.push(proof)
      process.stderr.write(JSON.stringify({ nativeFinalizationCancelled: proof }) + "\n")
    }
    if (options.nativeProcessLoss) {
      assert(previous)
      const completed = turns.filter(turn => turn.sessionId === started.session.id)
      const lost = await started.session.enqueue(fixtureInput("fixture-native-launcher-loss"), undefined, { signal })
      const recoveryInput = `${provider} unique after-loss input`
      const recovered = await started.session.enqueue(fixtureInput(recoveryInput), undefined, { signal })
      let after = 0
      for (;;) {
        const page = await started.session.events.list({ after, limit: 100 }, { signal })
        if (page.records.some(event => event.turnId === lost.id && event.kind === "turn.output" && JSON.stringify(event.data).includes("native launcher loss ready"))) break
        after = page.nextAfter
        if (!page.hasMore) await delay(100, undefined, { signal })
      }
      assert.equal((await recovered.get({ signal })).status, "queued")
      process.stderr.write(JSON.stringify({ nativeProcessLossRequested: { provider, sessionId: started.session.id, lostTurnId: lost.id, recoveredTurnId: recovered.id, before: previous } }) + "\n")
      await lost.send(fixtureInput("lose-native-launcher"), undefined, { signal })
      assert.equal((await lost.wait({ signal })).status, "interrupted")
      let holdId: string | undefined
      let holdReason: string | undefined
      for (;;) {
        const session = await started.session.get({ signal })
        if (session.holds.length) {
          assert.equal(session.holds.length, 1, "native loss added multiple holds")
          const hold = session.holds[0]!
          assert.equal(hold.scope, "local")
          assert(["session_continuation_lost", "turn_drain_failed"].includes(hold.reason), `unexpected native loss hold: ${hold.reason}`)
          holdId = hold.id; holdReason = hold.reason; break
        }
        await delay(100, undefined, { signal })
      }
      // Actual work on the shared Computer must progress while this Session is
      // held, with its successor still queued and setup not silently repeated.
      for (;;) {
        const page = await peer.session.events.list({ after: peerSequence, limit: 100 }, { signal })
        peerSequence = page.nextAfter
        if (!page.hasMore) break
      }
      const heldPeerStart = peerSequence
      for (let sample = 0; sample < 5; sample++) peerSequence = await progress(peerSequence)
      assert(peerSequence > heldPeerStart)
      assert.equal((await recovered.get({ signal })).status, "queued")
      const inspectSetup = await commands.exec({
        command: ["/usr/local/bin/node", "--input-type=module", "-e", `
          import assert from 'node:assert/strict';
          import {readFile} from 'node:fs/promises';
          assert.equal(JSON.parse(await readFile('/workspace/${started.session.id}/setup-count.json','utf8')),1);
        `], timeout: "10s", idempotencyKey: `fixture-held-setup-${provider}`,
      }, { signal })
      const inspected = await inspectSetup.wait({ signal })
      assert.equal(inspected.kind, "exited"); assert.equal(inspected.exitCode, 0)
      process.stderr.write(JSON.stringify({ nativeProcessLossHeld: { provider, holdId, holdReason, heldPeerStart, heldPeerEnd: peerSequence } }) + "\n")
      await started.session.resume({ holdId, idempotencyKey: `fixture-native-resume-${provider}` }, { signal })
      const resumed = await nativeOutcome(recovered, provider, 3)
      assert.equal(resumed.status, "completed", JSON.stringify(resumed))
      const state = await recovered.get({ signal })
      const result = state.result as Record<string, unknown>
      assert.equal(result["count"], 1, "exceptional resume did not reconstruct setup state")
      assert.equal(result["setupCount"], 2)
      assert.notEqual(result["nonce"], previous["nonce"])
      assert.notEqual(result["nativePid"], previous["nativePid"])
      assert.equal(result["nativeConversationId"], previous["nativeConversationId"])
      assert.equal((await lost.get({ signal })).status, "interrupted", "resume replayed the lost Turn")
      for (const old of completed) {
        const current = await started.session.turns.ref(old.id).get({ signal })
        assert.equal(current.status, "completed")
        assert.deepEqual(current.result, old.result)
      }
      const history = await commands.exec({
        command: ["/usr/local/bin/node", "--input-type=module", "-e", `
          import assert from 'node:assert/strict';
          import {readFile} from 'node:fs/promises';
          const ports=JSON.parse(await readFile('/workspace/native-models.json','utf8'));
          const requests=(await (await fetch('http://127.0.0.1:'+ports.proof)).json())[${JSON.stringify(provider)}];
          assert.equal(requests.length,6,'native work was replayed or omitted');
          const last=JSON.stringify(requests.at(-1));
          for (const marker of ${JSON.stringify([firstInput, secondInput, recoveryInput])}) assert(last.includes(marker),'missing native history: '+marker);
        `], timeout: "10s", idempotencyKey: `fixture-native-history-${provider}`,
      }, { signal })
      const historyResult = await history.wait({ signal })
      assert.equal(historyResult.kind, "exited"); assert.equal(historyResult.exitCode, 0)
      const evidence = { provider, sessionId: started.session.id, lostTurnId: lost.id, recoveredTurnId: recovered.id,
        holdId, holdReason, heldPeerStart, heldPeerEnd: peerSequence, before: previous, after: result }
      nativeLosses.push(evidence)
      process.stderr.write(JSON.stringify({ nativeProcessLossRecovered: evidence }) + "\n")
      turns.push(state)
    }
    await waitPublished(turns.filter(turn=>turn.sessionId===started.session.id))
  }
  if (options.deadlineSave) {
    const expiring = await client.sessions.create({ agent: "deadline", input: fixtureInput("expire during required save"), computer }, { signal })
    sessions.push(expiring.session)
    const readProof = async (path: string): Promise<Record<string, string>> => {
      for (;;) {
        try { return JSON.parse(await readFile(path, "utf8")) }
        catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error }
        await delay(50, undefined, { signal })
      }
    }
    const held = await readProof(`${options.evidence}/deadline-held.json`)
    assert.equal(held["turnId"], expiring.turn.id)
    assert.equal(held["sessionId"], expiring.session.id)
    assert(held["saveId"] && held["deadlineAt"])
    assert.equal((await expiring.turn.get({ signal })).status, "completed")
    for (;;) {
      const page = await peer.session.events.list({ after: peerSequence, limit: 100 }, { signal })
      peerSequence = page.nextAfter
      if (!page.hasMore) break
    }
    // Completion clears the running deadline. Hold only the successor so the
    // test can observe natural wall time crossing the original budget.
    const hold=await expiring.session.interrupt(undefined,{signal})
    const queued=await expiring.session.enqueue(fixtureInput("retained successor"),undefined,{signal})
    assert.equal((await queued.get({signal})).status,"queued")
    let peerAdvances=0
    while(Date.now()<=Date.parse(held["deadlineAt"]!)) {peerSequence=await progress(peerSequence);peerAdvances++}
    assert(peerAdvances>=2,"publication gate did not retain live peer work")
    const stopped=await expiring.turn.get({signal})
    assert.equal(stopped.status,"completed")
    assert(stopped.result!==undefined)
    const session=await expiring.session.get({signal})
    assert.equal(session.holds.length,1)
    assert.equal(session.holds[0]!.id,hold.holdId)
    const next=await queued.get({signal});assert.equal(next.status,"queued");assert.equal(next.startedAt,undefined)
    for (const completed of turns) {
      const current = await client.sessions.ref(completed.sessionId).turns.ref(completed.id).get({ signal })
      assert.equal(current.status, "completed")
      assert.deepEqual(current.result, completed.result)
    }
    for (;;) {
      const page = await peer.session.events.list({ after: peerSequence, limit: 100 }, { signal })
      peerSequence = page.nextAfter
      if (!page.hasMore) break
    }
    for (let sample = 0; sample < 5; sample++) {
      peerSequence = await progress(peerSequence)
      const pending = await queued.get({ signal })
      assert.equal(pending.status, "queued")
      assert.equal(pending.startedAt, undefined)
      assert.deepEqual((await expiring.session.get({ signal })).holds, session.holds)
    }
    deadline = { ...held, queuedTurnId: queued.id, holdId: hold.holdId }
    const release = `${options.evidence}/deadline-release.json`
    await writeFile(release + ".new", JSON.stringify(deadline), { mode: 0o600 })
    await rename(release + ".new", release)
    assert.deepEqual(await readProof(`${options.evidence}/deadline-published.json`), deadline)
    const after = await expiring.turn.get({ signal })
    assert.equal(after.status, "completed")
    assert.deepEqual(after.result,stopped.result)
    assert.equal((await queued.get({ signal })).status, "queued")
    assert.deepEqual((await expiring.session.get({ signal })).holds, session.holds)
    process.stderr.write(JSON.stringify({ configuredFinalizationDeadline: deadline, peerAdvances }) + "\n")
  }
  await waitPublished(turns)
  await peer.turn.send(fixtureInput("release"), undefined, { signal })
  const peerOutcome = await peer.turn.wait({ signal })
  assert.equal(peerOutcome.status, "completed")
  assert.equal((await model.get({ signal })).status, "running")
  const files = await commands.exec({
    command: ["/usr/local/bin/node", "--input-type=module", "-e", `
      import assert from 'node:assert/strict';
      import {readFile} from 'node:fs/promises';
      const turns=${JSON.stringify(turns)};
      for (const turn of turns) {
        const saved=JSON.parse(await readFile('/workspace/'+turn.sessionId+'/result-'+turn.sequence+'.json','utf8'));
        assert.deepEqual(saved,turn.result);
      }
      const peer=JSON.parse(await readFile('/workspace/peer-${peer.session.id}.json','utf8'));
      assert.deepEqual(peer,${JSON.stringify(peerOutcome.result)});
    `], timeout: "10s", idempotencyKey: "fixture-files",
  }, { signal })
  const fileResult = await files.wait({ signal })
  assert.equal(fileResult.kind, "exited")
  assert.equal(fileResult.exitCode, 0, "authored disk writes differ from native/peer results")
  let nativeSDK
  if (process.env.HELMR_NATIVE_SDK_PROOF === "1") {
    await model.cancel({ signal })
    await model.wait({ signal })
    nativeSDK = await runNativeSDK(client, computer.id, options.evidence, signal)
  }
  const codexStarter = process.env.HELMR_CODEX_STARTER_PROOF === "1" ? await runCodexStarter(client, computer.id, options.evidence, signal) : undefined
  const claudeStarter = process.env.HELMR_CLAUDE_STARTER_PROOF === "1" ? await runClaudeStarter(client, computer.id, options.evidence, signal) : undefined
  const embeddedStarters = process.env.HELMR_EMBEDDED_STARTER_PROOF === "1" ? [await runEmbeddedStarter("opencode", client, computer.id, options.evidence, signal), await runEmbeddedStarter("pi", client, computer.id, options.evidence, signal)] : undefined
  process.stdout.write(JSON.stringify({ embeddedStarters, claudeStarter, codexStarter, nativeSDK, computerId: computer.id, peerSessionId: peer.session.id, peerTurnId: peer.turn.id, peerSequence, peerOutcome, nativeSessions, nativeLosses, cancellations, deadline, turns, timings }) + "\n")
} finally {
  activeOutcome?.controller.abort()
  await activeOutcome?.promise.catch(() => {})
  // Closing preserves evidence and requests ordinary Session cleanup. A fixture
  // failure does not acquire authority to rewrite terminal state in the DB.
  const cleanupSignal = AbortSignal.timeout(10_000)
  const results = await Promise.allSettled([
    ...sessions.map(session => session.cancel(undefined, { signal: cleanupSignal })),
    ...(model ? [model.cancel({ signal: cleanupSignal }).then(() => model!.wait({ signal: cleanupSignal }))] : []),
  ])
  const failures = results.filter(result => result.status === "rejected")
  if (failures.length) throw new AggregateError(failures.map(result => result.reason), "fixture Session cleanup failed")
}
