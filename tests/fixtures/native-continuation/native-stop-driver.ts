import assert from "node:assert/strict"
import { readFile } from "node:fs/promises"
import { setTimeout as delay } from "node:timers/promises"
import type { HelmrClient } from "../../../sdk/typescript/src/client"
import type { SessionRef } from "../../../sdk/typescript/src/contract"
import type { TurnRef } from "../../../sdk/typescript/src/contract"
import { fixtureInput } from "../../e2e/support/runtime-mcp"

interface StopObservation {
  sessionId: string; processEpoch: number; stopSequence: number; stopTurnId: string | null
  stopDeadlineAt: string | null; observedAt: string; status: string; fencedAt: string | null
}

export async function interruptNativeTurn(client: HelmrClient, provider: string, session: SessionRef,
  computerID: string, previous: Record<string, unknown>, nextInput: string, observationPath: string,
  signal: AbortSignal, records: Record<string, unknown>[], admitted?: TurnRef) {
  const interrupted = admitted ?? await session.enqueue(fixtureInput("fixture-native-stop"), undefined, { signal })
  let askID: string
  for (;;) {
    const page = await interrupted.asks.list({ limit: 100 }, { signal })
    const pending = page.items.filter(ask => ask.status === "pending")
    assert(pending.length <= 1)
    if (pending.length) { askID = pending[0]!.id; break }
    assert(["queued", "running"].includes((await interrupted.get({ signal })).status))
    await delay(100, undefined, { signal })
  }
  const successor = await session.enqueue(fixtureInput(nextInput), undefined, { signal })
  const request = { idempotencyKey: `native-stop:${interrupted.id}` }
  const computer = client.computers.ref(computerID)
  const marker = `/workspace/native-stop-settled-${interrupted.id}.json`
  const ownedCommand = `/workspace/${session.id}/owned-command-${interrupted.id}`
  async function command(script: string, phase: string) {
    const requestedAt = Date.now()
    const report = (stage: string) => `process.stderr.write(JSON.stringify({nativeStopCommand:${JSON.stringify(phase)},stage:${JSON.stringify(stage)},at:Date.now()})+'\\n');`
    const command = await computer.exec({ command: ["/bin/sh", "-c", "printf '%s\\n' \"$1\" >&2; shift; exec \"$@\"", "native-stop-command",
      JSON.stringify({ nativeStopCommand: phase, stage: "launcher-entered" }), "/usr/local/bin/node", "--input-type=module", "-e", report("node-entered") + script + report("script-completed")],
      timeout: phase === "inspect-running" ? "60s" : "15s", idempotencyKey: `native-stop:${interrupted.id}:${phase}` }, { signal })
    process.stderr.write(JSON.stringify({ nativeStopCommand: phase, commandId: command.id, requestedAt }) + "\n")
    const outcome = await command.wait({ signal })
    process.stderr.write(JSON.stringify({ nativeStopCommand: phase, commandId: command.id, durationMs: Date.now() - requestedAt, outcome }) + "\n")
    assert.equal(outcome.kind, "exited", `native stop ${phase}: ${JSON.stringify(outcome)}`); assert.equal(outcome.exitCode, 0, `native stop ${phase} failed`)
    return command.id
  }
  const runningCommandID = provider === "codex" ? await command(`
    import assert from 'node:assert/strict'; import {readFile} from 'node:fs/promises'; import {spawnSync} from 'node:child_process';
    import {setTimeout as delay} from 'node:timers/promises';
    const deadline=Date.now()+8000;
    for (;;) { try { if(await readFile(${JSON.stringify(ownedCommand + ".started")},'utf8')===${JSON.stringify(interrupted.id)}) break }
      catch(error) { if(error.code!=='ENOENT') throw error }
      assert(Date.now()<deadline,'owned native command did not enter'); await delay(50) }
    process.stderr.write(JSON.stringify({nativeStopCommand:'inspect-running',stage:'marker-ready',at:Date.now()})+'\\n');
    const lock=spawnSync('/usr/bin/flock',['--nonblock','--exclusive',${JSON.stringify(ownedCommand + ".lock")},'/bin/true'],{timeout:1000});
    process.stderr.write(JSON.stringify({nativeStopCommand:'inspect-running',stage:'lock-checked',at:Date.now(),status:lock.status,error:lock.error?.message})+'\\n');
    assert.equal(lock.status,1,'owned command was not running before interruption');
  `, "inspect-running") : undefined
  const receipt = await interrupted.interrupt(request, { signal })
  assert.equal(receipt.turnId, interrupted.id)
  const inspectedCommandID = await command(`
    import assert from 'node:assert/strict'; import {readFile} from 'node:fs/promises';
    import {spawnSync} from 'node:child_process';
    import {setTimeout as delay} from 'node:timers/promises';
    let actual; const deadline=Date.now()+8000;
    for (;;) { try { actual=JSON.parse(await readFile(${JSON.stringify(marker)},'utf8')); break }
      catch(error) { if(error.code!=='ENOENT') throw error; assert(Date.now()<deadline,'fixture native cleanup marker exceeded 8s'); await delay(50) } }
    assert.equal(actual.turnId,${JSON.stringify(interrupted.id)});
    const previous=${JSON.stringify(previous)};
    for (const key of ['pid','nativePid','nonce','setupCount']) assert.equal(actual[key],previous[key],key);
    if (${JSON.stringify(provider)}==='codex') {
      assert.equal(actual.commandLock,${JSON.stringify(ownedCommand + ".lock")});
      assert.equal(spawnSync('/usr/bin/flock',['--nonblock','--exclusive',actual.commandLock,'/bin/true'],{timeout:1000}).status,0,'owned native command retained its lifetime lock');
    }
  `, "inspect-cleanup")
  const samples: StopObservation[] = []
  for (;;) {
    const rows = JSON.parse(await readFile(observationPath, "utf8")) as StopObservation[]
    const row = rows.find(row => row.sessionId === session.id && row.stopTurnId === interrupted.id)
    if (row && (!samples.length || Date.parse(row.observedAt) > Date.parse(samples.at(-1)!.observedAt))) {
      assert.equal(row.status, "ready"); assert.equal(row.fencedAt, null)
      assert(row.stopSequence > 0 && row.stopDeadlineAt && Date.parse(row.stopDeadlineAt) > Date.parse(row.observedAt))
      const queued = await successor.get({ signal })
      assert.equal(queued.status, "queued"); assert.equal(queued.startedAt, undefined)
      assert.deepEqual((await session.get({ signal })).holds, [])
      samples.push(row)
      if (samples.length === 3) break
    }
    await delay(100, undefined, { signal })
  }
  const releasedCommandID = await command(`import {writeFile,rename} from 'node:fs/promises'; await writeFile(${JSON.stringify(marker + ".release.tmp")},${JSON.stringify(interrupted.id)},{flag:'wx'}); await rename(${JSON.stringify(marker + ".release.tmp")},${JSON.stringify(marker + ".release")});`, "release-cleanup")
  assert.equal((await interrupted.wait({ signal })).status, "interrupted")
  assert.equal((await interrupted.asks.get(askID, { signal })).status, "cancelled")
  assert.deepEqual(await interrupted.interrupt(request, { signal }), receipt)
  let cleared: StopObservation
  for (;;) {
    const rows = JSON.parse(await readFile(observationPath, "utf8")) as StopObservation[]
    const row = rows.find(row => row.sessionId === session.id && row.processEpoch === samples[0]!.processEpoch)
    if (row && row.stopTurnId === null && row.stopSequence === samples[0]!.stopSequence && Date.parse(row.observedAt) > Date.parse(samples.at(-1)!.observedAt)) {
      assert.equal(row.status, "ready"); assert.equal(row.fencedAt, null); cleared = row; break
    }
    await delay(100, undefined, { signal })
  }
  const evidence = { provider, sessionId: session.id, interruptedTurnId: interrupted.id, successorTurnId: successor.id, askId: askID, runningCommandID, inspectedCommandID, releasedCommandID, samples, cleared }
  records.push(evidence)
  process.stderr.write(JSON.stringify({ nativeStop: evidence }) + "\n")
  return successor
}
