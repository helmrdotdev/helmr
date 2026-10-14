import { fixtureValue } from "../../support/runtime-mcp"
import { agent, template, image } from "@helmr/sdk"
import { z } from "zod"

export const questionComputer = template({
  id: "verification-question",
  image: image("verification-question").from("node:24-bookworm-slim").workdir("/workspace"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const questionAgent = agent({
  id: "verification-question",
  template: questionComputer,
  maxTurnDuration: "10m",
  async turn(turn) {
    const { marker } = z.object({ marker: z.string() }).strict().parse(fixtureValue(turn.input))
    const response = await turn.ask({
      prompt: [{ type: "text", text: `Approve ${marker}` }],
       type: "choice", options: [
        { id: "approve", label: "Approve", value: true },
        { id: "decline", label: "Decline", value: false },
      ], allowCustom: true, allowNotes: true,
    })
    const answer = response.answer
    return { marker, answer, answeredBy: response.answeredBy }
  },
})
