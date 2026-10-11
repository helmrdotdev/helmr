import { verify, assert, assertEqual, completedResult, waitAsk } from "../../support/context"

await verify("concurrent-questions", async ({ marker, objects, computer, startAgent }) => {
  const target = await computer("helmr-edge-smoke")
  const { turn } = await startAgent("edge-smoke", {
    computer: target, input: { mode: "concurrent-questions", marker }, idempotencyKey: `questions:${marker}`,
  })
  const [first, second] = await Promise.all(["first", "second"].map(label =>
    waitAsk(turn, ask => ask.prompt?.some(part => part.type === "text" && part.text === `${label}:${marker}`) === true)))
  assert(first && second)
  objects.ask_ids.push(first.id, second.id)
  assert.notEqual(first.id, second.id, "Concurrent questions shared an identity")
  await turn.asks.respond(second.id, { answer: "second answer", responseId: `second:${marker}` })
  assertEqual((await turn.asks.get(first.id)).status, "pending", "Answer retargeted the other question")
  await turn.asks.respond(first.id, { answer: "first answer", responseId: `first:${marker}` })
  assertEqual(await completedResult(turn), {
    mode: "concurrent-questions", marker, answers: ["first answer", "second answer"],
  }, "Concurrent question results were reordered or lost")
  return { concurrentQuestions: true, reverseAnswerOrder: true }
})
