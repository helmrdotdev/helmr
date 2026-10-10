import { verify, assert, assertEqual, completedResult, waitAsk } from "../../support/context"

await verify("question-isolation", async ({ marker, objects, computer, startAgent }) => {
  const target = await computer("verification-question")
  const first = await startAgent("verification-question", {
    computer: target, input: { marker: `${marker}:first` }, idempotencyKey: `first:${marker}`,
  })
  const second = await startAgent("verification-question", {
    computer: target, input: { marker: `${marker}:second` }, idempotencyKey: `second:${marker}`,
  })
  const questions = await Promise.all([first, second].map(({ turn }) => waitAsk(turn)))
  objects.ask_ids.push(...questions.map(question => question.id))
  assert.notEqual(questions[0]!.id, questions[1]!.id, "Independent Turns shared a question")
  await first.turn.asks.respond(questions[0]!.id, { answer: { selected: [{ id: "approve", value: true }] }, responseId: `first:${marker}` })
  const firstOutput = await completedResult(first.turn)
  assert(firstOutput !== null && typeof firstOutput === "object" && "answer" in firstOutput)
  assertEqual(firstOutput.answer, { selected: [{ id: "approve", value: true }] }, "First answer changed")
  assertEqual((await second.turn.asks.get(questions[1]!.id)).status, "pending", "One answer resolved another Turn's question")
  await second.turn.asks.respond(questions[1]!.id, { answer: { selected: [{ id: "decline", value: false }] }, responseId: `second:${marker}` })
  const secondOutput = await completedResult(second.turn)
  assert(secondOutput !== null && typeof secondOutput === "object" && "answer" in secondOutput)
  assertEqual(secondOutput.answer, { selected: [{ id: "decline", value: false }] }, "Independent answer changed")
  return { exactTurnIsolation: true, sharedComputer: true }
})
