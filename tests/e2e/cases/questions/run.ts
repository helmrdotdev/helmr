import { verify, assert, assertEqual, completedResult, waitAsk } from "../../support/context"

await verify("questions", async ({ marker, objects, computer, startAgent }) => {
  const target = await computer("verification-question")
  const { turn } = await startAgent("verification-question", {
    computer: target, input: { marker }, idempotencyKey: `question:${marker}`,
  })
  const ask = await waitAsk(turn)
  objects.ask_ids.push(ask.id)
  assertEqual(ask.turnId, turn.id, "Question changed its Turn")
  assertEqual(ask.sessionId, turn.sessionId, "Question changed its Session")
  const request = { answer: { selectedIds: ["approve"], notes: marker }, answerId: `answer:${marker}` }
  const receipt = await turn.asks.answer(ask.id, request)
  assertEqual(receipt.status, "answered", "Question did not accept answer")
  const output = await completedResult(turn)
  assert(output !== null && typeof output === "object" && "answer" in output)
  assertEqual(output.answer, { selected: [{ id: "approve", value: true }], notes: marker }, "Answer changed before handler resumed")
  assert("answeredBy" in output && output.answeredBy !== null && typeof output.answeredBy === "object" && "kind" in output.answeredBy)
  assertEqual(output.answeredBy.kind, "api_key", "Answer attribution was lost")
  const replay = await turn.asks.answer(ask.id, request)
  assertEqual(replay.id, receipt.id, "Answer retry changed question identity")
  assertEqual(replay.answeredByApiKeyId, receipt.answeredByApiKeyId, "Answer retry changed attribution")
  return { createdInsideTurn: true, resumed: true, answerRetry: true }
})
