import assert from "node:assert/strict"
import { writeFile } from "node:fs/promises"
import { setTimeout as delay } from "node:timers/promises"
import type { HelmrClient } from "../../../sdk/typescript/src/client"
import type { TurnRef } from "../../../sdk/typescript/src/contract"

const input = (text: string) => [{ type: "text" as const, text }]
const tool = (name: string, args: unknown) => ({ type: "function_call", call_id: `call_${crypto.randomUUID()}`, name, arguments: JSON.stringify(args) })
export async function runCodexStarter(client: HelmrClient, computerId: string, evidence: string, signal: AbortSignal) {
  const started = await client.sessions.create({ agent: "codex-starter", computer: { id: computerId }, input: input("starter remembered first") }, { signal })
  const session = started.session, cwd = `/workspace/codex-starter/${session.id}`
  const computer = client.computers.ref(computerId)
  let commandNumber = 0
  async function control(body: unknown = {}) {
    const command = await computer.exec({ command: ["/usr/local/bin/node", "--input-type=module", "-e", `
      import {readFile} from 'node:fs/promises';
      const {port}=JSON.parse(await readFile(${JSON.stringify(cwd + "/control.json")},'utf8'));
      const response=await fetch('http://127.0.0.1:'+port+'/control',{method:'POST',body:${JSON.stringify(JSON.stringify(body))}});
      if(!response.ok)throw Error(await response.text());
      console.log(await response.text());
    `], timeout: "60s", idempotencyKey: `starter-control-${session.id}-${++commandNumber}` }, { signal })
    const outcome = await command.wait({ signal })
    const chunks: Uint8Array[] = []
    for await (const record of command.logs.stream({}, { signal })) {
      assert.notEqual(record.kind, "gap")
      if (record.kind !== "gap") chunks.push(record.content)
    }
    const text = Buffer.concat(chunks).toString("utf8")
    assert.equal(outcome.kind, "exited", text); assert.equal(outcome.exitCode, 0, text)
    return JSON.parse(text)
  }
  function assertFreshAdmission(state: any, previous: any, marker: string) {
    assert.equal(state.inputs.length, state.admissions.length)
    const index = state.inputs.findIndex((messages: any, index: number) => index >= previous.calls && JSON.stringify(messages).includes(marker))
    assert.ok(index >= 0, "fresh input did not reach the model")
    const admission = state.admissions[index]
    assert.equal(admission.toolRunning, false)
    assert.equal(admission.toolFinished, false)
    assert.equal(admission.owners.length, previous.owners.length + 1)
    for (const owner of previous.owners) assert.ok(admission.owners.some((entry: any) => entry.pid === owner.pid && entry.exited))
    assert.equal(admission.owners.at(-1).exited, false)
    return admission
  }
  async function completed(turn: TurnRef) {
    const outcome = await turn.wait({ signal })
    assert.equal(outcome.status, "completed", JSON.stringify(outcome))
    return (await turn.get({ signal })).result as Record<string, any>
  }
  async function pendingAsk(turn: TurnRef) {
    for (;;) {
      const page = await turn.asks.list({ limit: 100 }, { signal })
      const pending = page.items.find(ask => ask.status === "pending")
      if (pending) return pending
      assert(["queued", "running"].includes((await turn.get({ signal })).status))
      await delay(100, undefined, { signal })
    }
  }
  async function observed(check: (state: any) => boolean) {
    for (;;) { const state = await control(); if (check(state)) return state; await delay(100, undefined, { signal }) }
  }
  const observations: Record<string, unknown> = { sessionId: session.id, computerId }
  try {
    const first = await completed(started.turn)
    const second = await completed(await session.enqueue(input("starter second"), undefined, { signal }))
    assert.equal(second.nativeThreadId, first.nativeThreadId)
    assert.equal(second.ownerPid, first.ownerPid)
    assert.equal(second.nonce, first.nonce)
    assert.equal(second.setups, 1)
    let state = await control()
    assert.ok(JSON.stringify(state.inputs.at(-1)).includes("starter remembered first"))
    assert.equal(state.goalTools, false)
    observations.retained = { first, second }
    const configHash = state.configHash
    await control({ actions: [tool("exec_command", { cmd: `printf tampered >> /var/lib/helmr/codex-starter/${session.id}/config.toml`, login: false })] })
    await completed(await session.enqueue(input("starter unapproved state write"), undefined, { signal }))
    assert.equal((await control()).configHash, configHash)
    observations.stateProtected = true

    await control({ actions: ["hold"] })
    const steering = await session.enqueue(input("starter steering"), undefined, { signal })
    await observed(value => value.held === 1)
    await steering.send(input("starter steer marker"), undefined, { signal })
    await control({ release: true })
    await completed(steering)
    state = await control()
    assert.ok(JSON.stringify(state.inputs.at(-1)).includes("starter steer marker"))
    await assert.rejects(steering.send(input("stale starter steering"), undefined, { signal }))
    observations.steering = steering.id

    await control({ actions: [tool("request_user_input", { questions: [{ id: "choice", header: "Pick", question: "Which?", options: [{ label: "A", description: "First" }, { label: "B", description: "Second" }] }] })] })
    const question = await session.enqueue(input("starter question"), undefined, { signal })
    const ask = await pendingAsk(question)
    await question.asks.answer(ask.id, { answer: { selectedIds: ["option-0"] }, answerId: `starter-answer-${ask.id}` }, { signal })
    await completed(question)
    observations.question = { turnId: question.id, askId: ask.id }

    // The mock yields an actual background command before holding its next model
    // request. A lifetime lock checks cessation across guest PID namespaces.
    const command = `flock --exclusive ${cwd}/tool.lock /bin/sh -c 'sleep 30; echo late > ${cwd}/tool.finished'`
    await control({ actions: [tool("exec_command", { cmd: command, login: false, yield_time_ms: 250 })] })
    const backgroundTurn = await session.enqueue(input("starter retained background"), undefined, { signal })
    await completed(backgroundTurn)
    const retainedBackground = await control()
    assert.deepEqual(retainedBackground.owners, [{ pid: first.ownerPid, exited: false }])
    assert.equal(retainedBackground.toolRunning, true)
    assert.equal(retainedBackground.toolFinished, null)
    await completed(await session.enqueue(input("starter next Turn with background"), undefined, { signal }))
    assert.equal((await control()).toolRunning, true)
    await control({ actions: ["hold"] })
    const stopRetained = await session.enqueue(input("starter stop earlier background"), undefined, { signal })
    const beforeRetainedStop = await observed(value => value.held === 1)
    assert.deepEqual(beforeRetainedStop.owners, retainedBackground.owners)
    await stopRetained.interrupt(undefined, { signal })
    assert.equal((await stopRetained.wait({ signal })).status, "interrupted")
    // Public interruption is durable before asynchronous cleanup. The successor's
    // first model request must observe cessation after the runtime stop ACK.
    const afterRetained = await completed(await session.enqueue(input("starter after retained cleanup"), undefined, { signal }))
    const retainedStopped = await control()
    observations.retainedBackground = { turnId: backgroundTurn.id, retainedBackground, beforeRetainedStop, retainedStopped, afterRetained }
    const retainedAdmission = assertFreshAdmission(retainedStopped, beforeRetainedStop, "starter after retained cleanup")
    assert.equal(afterRetained.nonce, first.nonce)
    assert.equal(afterRetained.setups, 1)
    assert.notEqual(afterRetained.ownerPid, first.ownerPid)
    Object.assign(observations.retainedBackground!, { retainedAdmission })

    await control({ actions: [tool("exec_command", { cmd: command, login: false, yield_time_ms: 250 }), "hold"] })
    const cancelled = await session.enqueue(input("starter background stop"), undefined, { signal })
    const running = await observed(value => value.toolRunning && value.held === 2)
    await cancelled.interrupt(undefined, { signal })
    assert.equal((await cancelled.wait({ signal })).status, "interrupted")
    const afterCancel = await completed(await session.enqueue(input("starter fresh after stop"), undefined, { signal }))
    const stopped = await control()
    observations.cancellation = { turnId: cancelled.id, running, stopped, afterCancel }
    const admission = assertFreshAdmission(stopped, running, "starter fresh after stop")
    assert.equal(stopped.toolRunning, false)
    assert.equal(stopped.toolFinished, null)
    assert.deepEqual((await session.get({ signal })).holds, [])
    assert.equal(afterCancel.nativeThreadId, first.nativeThreadId)
    assert.equal(afterCancel.nonce, first.nonce)
    assert.equal(afterCancel.setups, 1)
    assert.notEqual(afterCancel.ownerPid, afterRetained.ownerPid)
    Object.assign(observations.cancellation!, { admission })

    // A lost native owner is one process failure: the next input reconstructs
    // the process without a hold.
    await control({ actions: [tool("exec_command", { cmd: command, login: false, yield_time_ms: 250 }), "hold"] })
    const lost = await session.enqueue(input("starter owner failure"), undefined, { signal })
    await observed(value => value.held === 3 && value.toolRunning)
    await control({ crash: true }).catch(() => {})
    assert.equal((await lost.wait({ signal })).status, "interrupted")
    const fresh = await session.enqueue(input("starter fresh reconstructed"), undefined, { signal })
    const reconstructed = await completed(fresh)
    assert.deepEqual((await session.get({ signal })).holds, [])
    assert.equal(reconstructed.setups, 2)
    assert.notEqual(reconstructed.nonce, first.nonce)
    assert.equal(reconstructed.nativeThreadId, first.nativeThreadId)
    state = await control()
    assert.equal(state.calls, 1, "reconstruction replayed old input")
    assert.ok(JSON.stringify(state.inputs[0]).includes("starter remembered first"))
    assert.ok(JSON.stringify(state.inputs[0]).includes("starter fresh reconstructed"))
    assert.equal(state.toolRunning, false)
    assert.equal(state.toolFinished, null)
    observations.reconstruction = { lostTurnId: lost.id, freshTurnId: fresh.id, reconstructed, state }
    return { sessionId: session.id, retained: true, steering: true, questions: true, joinedBackgroundStop: true, heldOwnerFailure: true, reconstruction: true }
  } finally {
    try { await writeFile(`${evidence}/codex-starter.json`, JSON.stringify(observations, null, 2) + "\n", { mode: 0o600 }) }
    finally { await session.cancel(undefined, { signal: AbortSignal.timeout(15_000) }) }
  }
}
