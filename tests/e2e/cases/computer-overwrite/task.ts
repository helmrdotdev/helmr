import { fixtureValue } from "../../support/runtime-mcp"
import { agent, computer, image, type Json } from "@helmr/sdk"
import { appendFile, mkdir, readFile, writeFile } from "node:fs/promises"
import { z } from "zod"

export const edgeSmokeComputer = computer({
  id: "helmr-edge-smoke",
  image: image("helmr-edge-smoke").from("node:24-bookworm-slim").workdir("/workspace"),
  resources: { cpu: 1, memory: "1GiB" },
})

const inputSchema = z.object({
  mode: z.enum(["concurrent-questions", "computer-overwrite", "expected-error"]),
  marker: z.string().optional(),
}).strict()

export const edgeSmoke = agent({
  id: "edge-smoke",
  computer: edgeSmokeComputer,
  maxTurnDuration: "5m",
  async turn(turn): Promise<Json> {
    const input = inputSchema.parse(fixtureValue(turn.input))
    const marker = input.marker?.trim() || `edge-${turn.id}`
    switch (input.mode) {
      case "concurrent-questions": {
        const responses = await Promise.all(["first", "second"].map(label => turn.ask({
          prompt: [{ type: "text", text: `${label}:${marker}` }],
          answer: { type: "text" },
        })))
        return { mode: input.mode, marker, answers: responses.map(response => response.answer) }
      }
      case "computer-overwrite":
        return { mode: input.mode, marker, computer: await exerciseComputerOverwrite(marker) }
      case "expected-error":
        throw new Error(`intentional edge-case failure for marker ${marker}`)
    }
  },
})

async function exerciseComputerOverwrite(marker: string): Promise<{ path: string, content: string }> {
  await mkdir("edge", { recursive: true })
  const path = "edge/overwrite.txt"
  await writeFile(path, `first:${marker}\n`)
  await appendFile(path, `second:${marker}\n`)
  await writeFile(path, `final:${marker}\n`)
  const content = await readFile(path, "utf8")
  if (content !== `final:${marker}\n`) throw new Error(`Computer overwrite produced unexpected content: ${content}`)
  return { path, content }
}
