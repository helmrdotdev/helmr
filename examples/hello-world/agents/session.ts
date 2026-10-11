import { agent } from "@helmr/sdk"
import { z } from "zod"
import { helloWorldComputer } from "./hello-world"

// Output is observable before validation and successful settlement.
export const checkedReply = agent({
  id: "checked-reply",
  computer: helloWorldComputer,
  async turn(turn) {
    const input = z.object({ text: z.string(), expected: z.string() }).parse(JSON.parse(turn.input.map(part => part.text).join("")))
    await turn.output.write(input.text)
    // Replace this check with the project's deterministic test command.
    if (input.text !== input.expected) throw new Error("Reply check failed")
    await turn.respond(input.text)
    return { characters: input.text.length }
  },
})
