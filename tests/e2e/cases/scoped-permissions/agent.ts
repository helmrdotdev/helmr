import { agent, template, image } from "@helmr/sdk"
import { readFile, writeFile } from "node:fs/promises"
import { fixtureValue } from "../../support/runtime-mcp"
import { z } from "zod"

export const scopedComputer = template({
  id: "verification-scoped",
  image: image("verification-scoped").from("node:24-bookworm-slim").workdir("/workspace"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const scopedAgent = agent({
  id: "verification-scoped",
  template: scopedComputer,
  maxTurnDuration: "5m",
  async turn(turn, ctx) {
    const input = z.object({ marker: z.string().min(1), ask: z.boolean().default(false) }).strict().parse(fixtureValue(turn.input))
    let previous: string | null = null
    try { previous = await readFile("scoped-marker", "utf8") }
    catch (error) { if (!(error instanceof Error && "code" in error && error.code === "ENOENT")) throw error }
    await writeFile("scoped-marker", input.marker)
    if (input.ask) await turn.ask({ type: "choice", options: [{ id: "done", label: "Done", value: true }], prompt: [{ type: "text", text: "Complete scoped verification" }] })
    return { marker: input.marker, previous, computerId: ctx.computer.id, ambientAPIKey: !!process.env.HELMR_API_KEY }
  },
})
