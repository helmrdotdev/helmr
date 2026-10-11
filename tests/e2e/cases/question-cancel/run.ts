import { verify, assert, assertEqual, errorCode, waitAsk, waitTurn } from "../../support/context"

await verify("question-cancel", async ({ marker, objects, computer, startAgent }) => {
  const target = await computer("verification-question")
  const { session, turn } = await startAgent("verification-question", {
    computer: target, input: { marker }, idempotencyKey: `question:${marker}`,
  })
  const ask = await waitAsk(turn)
  objects.ask_ids.push(ask.id)
  assertEqual((await turn.asks.get(ask.id)).status, "pending", "Question was not pending")
  await session.cancel({ idempotencyKey: `cancel:${marker}` })
  await waitTurn(turn, ["cancelled"])
  assertEqual((await turn.asks.get(ask.id)).status, "cancelled", "Session cancellation did not withdraw its question")
  await assert.rejects(turn.asks.respond(ask.id, {
    answer: { selected: [{ id: "approve", value: true }] }, responseId: `late:${marker}`,
  }), error => errorCode(error) === "ask_cancelled")
  return { retrieved: true, cancelled: true, lateAnswerRejected: true }
})
