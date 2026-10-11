import assert from "node:assert/strict"
import { readFile } from "node:fs/promises"
import { setTimeout as delay } from "node:timers/promises"
import type { HelmrClient } from "../../../sdk/typescript/src/client"
import type { ClientSessionRef } from "../../../sdk/typescript/src/client-session"
import { fixtureInput } from "../../e2e/support/runtime-mcp"

interface StopObservation {
  sessionId: string; processEpoch: number; stopSequence: number; stopTurnId: string | null
  stopDeadlineAt: string | null; observedAt: string; status: string; fencedAt: string | null
}

export async function interruptNativeTurn(client: HelmrClient, provider: string, session: ClientSessionRef,
  computerID: string, previous: Record<string, unknown>, nextInput: string, observationPath: string,
  signal: AbortSignal, records: Record<string, unknown>[]) {
  const interrupted = await session.enqueue(fixtureInput("fixture-native-stop"), undefined, { signal })
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
  const receipt = await interrupted.interrupt(request, { signal })
  assert.equal(receipt.turnId, interrupted.id)
  const computer = client.computers.ref(computerID)
  const marker = `/workspace/native-stop-settled-${interrupted.id}.json`
  async function command(script: string, phase: string) {
    const command = await computer.exec({ command: ["/usr/local/bin/node", "--input-type=module", "-e", script], timeout: "15s", idempotencyKey: `native-stop:${interrupted.id}:${phase}` }, { signal })
    const outcome = await command.wait({ signal })
    assert.equal(outcome.kind, "exited"); assert.equal(outcome.exitCode, 0, `native stop ${phase} failed`)
    return command.id
  }
  const inspectedCommandID = await command(`
    import assert from 'node:assert/strict'; import {readFile} from 'node:fs/promises';
    import {setTimeout as delay} from 'node:timers/promises';
    let actual; const deadline=Date.now()+8000;
    for (;;) { try { actual=JSON.parse(await readFile(${JSON.stringify(marker)},'utf8')); break }
      catch(error) { if(error.code!=='ENOENT') throw error; assert(Date.now()<deadline,'fixture native cleanup marker exceeded 8s'); await delay(50) } }
    assert.equal(actual.turnId,${JSON.stringify(interrupted.id)});
    const previous=${JSON.stringify(previous)};
    for (const key of ['pid','nativePid','nonce','setupCount','nativeScopeId']) assert.equal(actual[key],previous[key],key);
    if (${JSON.stringify(provider)}==='codex') { assert(Number.isSafeInteger(actual.commandPid)); assert.throws(()=>process.kill(actual.commandPid,0),{code:'ESRCH'}) }
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
  const evidence = { provider, sessionId: session.id, interruptedTurnId: interrupted.id, successorTurnId: successor.id, askId: askID, inspectedCommandID, releasedCommandID, samples, cleared }
  records.push(evidence)
  process.stderr.write(JSON.stringify({ nativeStop: evidence }) + "\n")
  return successor
}
