import assert from "node:assert/strict"
import { writeFile } from "node:fs/promises"
import { setTimeout as delay } from "node:timers/promises"
import type { HelmrClient } from "../../../sdk/typescript/src/client"
import type { TurnRef } from "../../../sdk/typescript/src/contract"

const input = (text: string) => [{ type: "text" as const, text }]
export async function runEmbeddedStarter(kind: "opencode" | "pi", client: HelmrClient, computerId: string, evidence: string, signal: AbortSignal) {
  const started = await client.sessions.create({ agent: `${kind}-starter`, computer: { id: computerId }, input: input("starter remembered first") }, { signal })
  const session = started.session, cwd = `/workspace/${kind}-starter/${session.id}`, computer = client.computers.ref(computerId)
  let commandNumber = 0
  async function control(body: unknown = {}) {
    const command = await computer.exec({ command: ["/usr/local/bin/node", "--input-type=module", "-e", `
      import {readFile} from 'node:fs/promises';
      const {port}=JSON.parse(await readFile(${JSON.stringify(cwd + "/control.json")},'utf8'));
      const response=await fetch('http://127.0.0.1:'+port+'/control',{method:'POST',body:${JSON.stringify(JSON.stringify(body))}});
      if(!response.ok)throw Error(await response.text());
      console.log(await response.text());
    `], timeout: "60s", idempotencyKey: `${kind}-control-${session.id}-${++commandNumber}` }, { signal })
    const outcome = await command.wait({ signal })
    const chunks: Uint8Array[] = []
    for await (const record of command.logs.stream({}, { signal })) { assert.notEqual(record.kind, "gap"); if (record.kind !== "gap") chunks.push(record.content) }
    const text = Buffer.concat(chunks).toString("utf8")
    assert.equal(outcome.kind, "exited", text); assert.equal(outcome.exitCode, 0, text)
    return JSON.parse(text)
  }
  async function completed(turn: TurnRef) {
    const outcome = await turn.wait({ signal })
    assert.equal(outcome.status, "completed", JSON.stringify(outcome))
    return (await turn.get({ signal })).result as Record<string, any>
  }
  async function observed(check: (state: any) => boolean) {
    for (;;) { const state = await control(); if (check(state)) return state; await delay(100, undefined, { signal }) }
  }
  async function pendingAsk(turn: TurnRef) {
    for (;;) {
      const pending = (await turn.asks.list({ limit: 100 }, { signal })).items.find(ask => ask.status === "pending")
      if (pending) return pending
      assert(["queued", "running"].includes((await turn.get({ signal })).status))
      await delay(100, undefined, { signal })
    }
  }
  const observations: Record<string, unknown> = { sessionId: session.id, computerId }
  try {
    const first = await completed(started.turn)
    const second = await completed(await session.enqueue(input("starter second"), undefined, { signal }))
    assert.equal(second.nativeSessionId, first.nativeSessionId)
    assert.equal(second.ownerPid, first.ownerPid); assert.equal(second.nonce, first.nonce); assert.equal(second.setups, 1)
    let state = await control()
    assert.ok(JSON.stringify(state.inputs.at(-1)).includes("starter remembered first"))
    observations.retained = { first, second }

    await control({ actions: ["hold"] })
    const steering = await session.enqueue(input("starter steering"), undefined, { signal })
    await observed(value => value.held === 1)
    await steering.send(input("starter steer marker"), undefined, { signal })
    await control({ release: true })
    await completed(steering)
    state = await control()
    assert.ok(JSON.stringify(state.inputs.at(-1)).includes("starter steer marker"))
    await assert.rejects(steering.send(input("stale steering"), undefined, { signal }))
    observations.steering = { turnId: steering.id }

    if (kind === "opencode") await control({ actions: [{ name: "question", input: { questions: [{ question: "Which fixture?", header: "Fixture", options: [{ label: "A", description: "First" }, { label: "B", description: "Second" }] }] } }] })
    const question = await session.enqueue(input(kind === "pi" ? "/question" : "starter question"), undefined, { signal })
    const ask = await pendingAsk(question)
    await question.asks.answer(ask.id, { answer: { selectedIds: [kind === "pi" ? "yes" : "option-0"] }, answerId: `${kind}-${ask.id}` }, { signal })
    await completed(question)
    observations.question = { turnId: question.id, askId: ask.id }

    const command = `echo $$ > '${cwd}/tool.pid'; sleep 300; echo done > '${cwd}/tool.finished'`
    const action = kind === "opencode" ? { name: "shell", input: { command, description: "Hold fixture shell" } } : { name: "bash", input: { command } }
    await control({ actions: [action] })
    const cancelled = await session.enqueue(input("starter native tool stop"), undefined, { signal })
    const running = await observed(value => value.toolRunning)
    await cancelled.send(input("retire this pending input"), undefined, { signal })
    await cancelled.interrupt(undefined, { signal })
    assert.equal((await cancelled.wait({ signal })).status, "interrupted")
    const afterCancel = await completed(await session.enqueue(input("starter fresh after stop"), undefined, { signal }))
    state = await control()
    const freshIndex = state.inputs.findIndex((messages: any, index: number) => index >= running.calls && JSON.stringify(messages).includes("starter fresh after stop"))
    assert.ok(freshIndex >= 0)
    assert.equal(state.admissions[freshIndex].toolRunning, false)
    assert.equal(state.admissions[freshIndex].toolFinished, false)
    assert.ok(!JSON.stringify(state.inputs[freshIndex]).includes("retire this pending input"))
    assert.equal(afterCancel.nativeSessionId, first.nativeSessionId)
    assert.equal(afterCancel.nonce, first.nonce); assert.equal(afterCancel.setups, 1)
    assert.deepEqual((await session.get({ signal })).holds, [])
    observations.cancellation = { turnId: cancelled.id, running, afterCancel, state }

    await control({ actions: [action] })
    const lost = await session.enqueue(input("starter owner failure"), undefined, { signal })
    await observed(value => value.toolRunning)
    await control({ crash: true }).catch(() => {})
    assert.equal((await lost.wait({ signal })).status, "interrupted")
    let hold
    for (;;) {
      hold = (await session.get({ signal })).holds.find(value => ["session_continuation_lost", "turn_drain_failed"].includes(value.reason))
      if (hold) break
      await delay(100, undefined, { signal })
    }
    const fresh = await session.enqueue(input("starter fresh reconstructed"), undefined, { signal })
    assert.equal((await fresh.get({ signal })).status, "queued")
    await session.resume({ holdId: hold.id, idempotencyKey: `${kind}-resume` }, { signal })
    const reconstructed = await completed(fresh)
    assert.equal(reconstructed.setups, 2); assert.notEqual(reconstructed.nonce, first.nonce)
    assert.equal(reconstructed.nativeSessionId, first.nativeSessionId)
    state = await control()
    assert.equal(state.calls, 1, "reconstruction replayed old input")
    assert.ok(JSON.stringify(state.inputs[0]).includes("starter remembered first"))
    assert.ok(JSON.stringify(state.inputs[0]).includes("starter fresh reconstructed"))
    assert.equal(state.admissions[0].toolRunning, false); assert.equal(state.admissions[0].toolFinished, false)
    observations.reconstruction = { lostTurnId: lost.id, freshTurnId: fresh.id, hold, reconstructed, state }
    return { kind, sessionId: session.id, retained: true, steering: true, questions: true, joinedNativeStop: true, heldOwnerFailure: true, reconstruction: true }
  } finally {
    try { await writeFile(`${evidence}/${kind}-starter.json`, JSON.stringify(observations, null, 2) + "\n", { mode: 0o600 }) }
    finally { await session.cancel(undefined, { signal: AbortSignal.timeout(15_000) }) }
  }
}
