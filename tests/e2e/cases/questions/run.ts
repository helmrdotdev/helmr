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
  const request = { answer: { selected: [{ id: "approve", value: true }], text: marker }, responseId: `answer:${marker}` }
  const receipt = await turn.asks.respond(ask.id, request)
  assertEqual(receipt.status, "responded", "Question did not accept answer")
  const output = await completedResult(turn)
  assert(output !== null && typeof output === "object" && "answer" in output)
  assertEqual(output.answer, request.answer, "Answer changed before handler resumed")
  assert("respondedBy" in output && output.respondedBy !== null && typeof output.respondedBy === "object" && "kind" in output.respondedBy)
  assertEqual(output.respondedBy.kind, "api_key", "Answer attribution was lost")
  const replay = await turn.asks.respond(ask.id, request)
  assertEqual(replay.id, receipt.id, "Answer retry changed question identity")
  assertEqual(replay.respondedByApiKeyId, receipt.respondedByApiKeyId, "Answer retry changed attribution")
  return { createdInsideTurn: true, resumed: true, responseRetry: true }
})
