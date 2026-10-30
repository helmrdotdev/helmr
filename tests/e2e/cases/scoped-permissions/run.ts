import { setTimeout as delay } from "node:timers/promises"
import { HelmrClient } from "@helmr/sdk"
import { assert, completedResult, deadline, deleteComputer, errorCode, verify, waitAsk } from "../../support/context"
import { fixtureInput } from "../../support/runtime-mcp"

// The ordinary verification credential is an environment operator with Computer
// cleanup rights. These two distinct keys must each have only sessions.create.
const rawA = process.env.HELMR_SCOPED_KEY_A
const rawB = process.env.HELMR_SCOPED_KEY_B
assert(rawA && rawB && rawA !== rawB, "Two distinct scoped verification keys are required")
const options = { url: process.env.HELMR_API_URL, fetch: ((input, init) => fetch(input, { ...init, signal: init?.signal ?? deadline(30_000) })) as typeof fetch }
const a = new HelmrClient({ ...options, apiKey: rawA })
const b = new HelmrClient({ ...options, apiKey: rawB })
const denied = async (operation: () => Promise<unknown>) => assert.rejects(operation, error => ["forbidden", "not_found", "session_not_found", "computer_not_found"].includes(errorCode(error) ?? ""))

await verify("scoped-permissions", async ({ client: operator, marker, objects, cleanup }) => {
  const start = async (client: HelmrClient, suffix: string, ask = false) => {
    const receipt = await client.sessions.create({ agent: "verification-scoped", input: fixtureInput({ marker: `${marker}:${suffix}`, ask }), idempotencyKey: `${marker}:${suffix}` })
    objects.session_ids.push(receipt.session.id)
    objects.turn_ids.push(receipt.turn.id)
    // Record the native Computer as soon as admission is available, so failures
    // before Turn completion still leave cleanup ownership in the evidence.
    let state = await operator.sessions.get(receipt.session.id)
    const signal = deadline(60_000)
    while (!state.computerId) { await delay(250, undefined, { signal }); state = await operator.sessions.get(receipt.session.id, { signal }) }
    const computerId = state.computerId
    objects.computer_ids.push(computerId)
    cleanup(() => deleteComputer(operator.computers.ref(computerId), `${marker}:cleanup:${computerId}`))
    return { ...receipt, computerId }
  }
  const first = await start(a, "first")
  const firstResult = await completedResult(operator.sessions.ref(first.session.id).turns.ref(first.turn.id))
  assert.deepEqual(firstResult, { marker: `${marker}:first`, previous: null, computerId: first.computerId, ambientAPIKey: false })
  const second = await start(b, "second")
  const secondResult = await completedResult(operator.sessions.ref(second.session.id).turns.ref(second.turn.id))
  assert.notEqual(first.computerId, second.computerId, "Distinct origins shared a Computer")
  assert.deepEqual(secondResult, { marker: `${marker}:second`, previous: null, computerId: second.computerId, ambientAPIKey: false })
  await denied(() => b.sessions.get(first.session.id))
  await denied(() => a.sessions.get(second.session.id))
  await denied(() => b.sessions.ref(first.session.id).events.list({ after: 0, limit: 100 }))
  await denied(() => b.sessions.ref(first.session.id).enqueue(fixtureInput({ marker: "forbidden" }), { idempotencyKey: `${marker}:cross-input` }))
  await denied(() => a.computers.get(first.computerId))
  await denied(() => operator.sessions.create({ agent: "verification-scoped", computer: { id: first.computerId }, input: fixtureInput({ marker: "forbidden" }), idempotencyKey: `${marker}:broad-attachment` }))
  await denied(() => a.sessions.get(first.session.id))
  await denied(() => first.session.enqueue(fixtureInput({ marker: "forbidden" }), { idempotencyKey: `${marker}:own-input` }))
  const continuation = await operator.sessions.ref(first.session.id).enqueue(fixtureInput({ marker: `${marker}:continued` }), { idempotencyKey: `${marker}:continued` })
  objects.turn_ids.push(continuation.id)
  assert.deepEqual(await completedResult(continuation), { marker: `${marker}:continued`, previous: `${marker}:first`, computerId: first.computerId, ambientAPIKey: false })
  const questioning = await start(a, "question", true)
  const turn = operator.sessions.ref(questioning.session.id).turns.ref(questioning.turn.id)
  const ask = await waitAsk(turn)
  objects.ask_ids.push(ask.id)
  await denied(() => questioning.turn.asks.answer(ask.id, { answer: { selectedIds: ["done"] }, answerId: `${marker}:own-answer` }))
  await denied(() => b.sessions.ref(questioning.session.id).turns.ref(questioning.turn.id).asks.answer(ask.id, { answer: { selectedIds: ["done"] }, answerId: `${marker}:cross-answer` }))
  await turn.asks.answer(ask.id, { answer: { selectedIds: ["done"] }, answerId: `${marker}:operator-answer` })
  await completedResult(turn)
  return { distinctKeyOrigins: true, distinctNativeComputers: true, isolatedGuestFiles: true, fileContinuity: true, createOnlyReadInputAnswerDenied: true, explicitOperatorActions: true, broadAttachmentDenied: true, ambientWorkloadKeyAbsent: true }
})
