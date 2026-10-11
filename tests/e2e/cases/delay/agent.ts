import { fixtureValue } from "../../support/runtime-mcp"
import { agent, computer, image } from "@helmr/sdk"
import { readFile, writeFile } from "node:fs/promises"
import { setTimeout as delay } from "node:timers/promises"
import { z } from "zod"

export const delayComputer = computer({
  id: "helmr-delay-smoke",
  image: image("helmr-delay-smoke").from("node:24-bookworm-slim").workdir("/workspace"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const delayAgent = agent({
  id: "delay-smoke",
  computer: delayComputer,
  maxTurnDuration: "5m",
  async turn(turn) {
    const input = z.object({ marker: z.string(), delayMs: z.number().int().min(1).max(240_000) }).strict().parse(fixtureValue(turn.input))
    const before = { marker: input.marker, pid: process.pid, steps: ["before-delay"] }
    const statePath = `delay-${turn.id}.json`
    await writeFile(statePath, JSON.stringify(before))
    await turn.output.write([{ type: "json", value: { phase: "before-delay", marker: input.marker } }])
    const startedAt = Date.now()
    // An ordinary application timer; this does not assert VM parking or restore.
    await delay(input.delayMs, undefined, { signal: turn.signal })
    const restored = JSON.parse(await readFile(statePath, "utf8")) as typeof before
    if (restored.marker !== before.marker || restored.pid !== before.pid) throw new Error("Delay lost application state")
    return { ...before, elapsedMs: Date.now() - startedAt, steps: [...before.steps, "after-delay"] }
  },
})
