import { fixtureValue } from "../../support/runtime-mcp"
import { agent, image, computer } from "@helmr/sdk"
import { randomUUID } from "node:crypto"
import { mkdir, readFile, readlink, symlink, writeFile } from "node:fs/promises"
import { z } from "zod"

export const durabilityComputer = computer({
  id: "computer-durability", image: image("computer-durability").from("node:24-bookworm-slim").workdir("/workspace"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const durabilityAgent = agent({
  id: "computer-durability", computer: durabilityComputer, maxTurnDuration: "10m",
  setup: () => ({ runtimeNonce: randomUUID(), count: 0, path: `/root/helmr-durability/${randomUUID()}`, marker: "" }),
  turn: async (turn, ctx) => {
    const { marker } = z.object({ marker: z.string().min(1) }).strict().parse(fixtureValue(turn.input))
    const memory = ctx.setupResult
    const { runtimeNonce, path } = memory
    const count = ++memory.count
    if (count === 1) {
      memory.marker = marker
      await mkdir(path, { recursive: true })
      await writeFile(`${path}/state.json`, JSON.stringify({ marker }))
      await symlink("state.json", `${path}/current`)
    } else if (count !== 2 || memory.marker !== marker) throw new Error("Session state changed")
    if (await readlink(`${path}/current`) !== "state.json") throw new Error("symlink changed")
    if (JSON.parse(await readFile(`${path}/current`, "utf8")).marker !== marker) throw new Error("persisted marker changed")
    const result = { marker, runtimeNonce, count, path }
    if (count === 2) {
      let finish!: () => void
      const observed = new Promise<void>(resolve => { finish = resolve })
      await turn.onMessage(message => { if (fixtureValue(message) === "finish") finish() })
      await turn.output.write([{ type: "json", value: { ...result, phase: "durability-restored" } }])
      await observed
    }
    // Turn 1 completes before the provider waits for an idle checkpoint.
    return result
  },
})
