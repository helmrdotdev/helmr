import { fixtureValue } from "../../support/runtime-mcp"
import { agent, computer, image } from "@helmr/sdk"
import { z } from "zod"

export const questionComputer = computer({
  id: "verification-question",
  image: image("verification-question").from("node:24-bookworm-slim").workdir("/workspace"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const questionAgent = agent({
  id: "verification-question",
  computer: questionComputer,
  maxTurnDuration: "10m",
  async turn(turn) {
    const { marker } = z.object({ marker: z.string() }).strict().parse(fixtureValue(turn.input))
    const response = await turn.ask({
      prompt: [{ type: "text", text: `Approve ${marker}` }],
      answer: { type: "choice", options: [
        { id: "approve", label: "Approve", value: true },
        { id: "decline", label: "Decline", value: false },
      ], allowText: true },
    })
    const answer = response.answer
    return { marker, answer, respondedBy: response.respondedBy }
  },
})
