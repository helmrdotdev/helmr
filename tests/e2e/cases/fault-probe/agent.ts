import { fixtureValue } from "../../support/runtime-mcp"
import { setTimeout as sleep } from "node:timers/promises"
import { image, agent, computer, type Json } from "@helmr/sdk"
import { appendFile, readFile } from "node:fs/promises"
import { z } from "zod"

const base = image("helmr-fault-probe")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")

export const faultProbeComputer = computer({ id: "helmr-fault-probe", image: base, resources: { cpu: 1, memory: "1GiB" } })

const payload = z.object({
  marker: z.string().min(1),
  mode: z.enum(["hold", "network-deny"]),
  delaySeconds: z.number().int().min(0).max(900).default(0),
  holdSeconds: z.number().int().min(1).max(900).default(30),
  denyAttempts: z.number().int().min(1).max(20).default(3),
}).strict()

export const faultProbe = agent({
  id: "fault-probe",
  computer: faultProbeComputer, maxTurnDuration: "35m",
  turn: async (turn, ctx): Promise<Json> => {
    const input = payload.parse(fixtureValue(turn.input))
    await appendFile(
      "fault-probe-turns.log",
      `${ctx.session.id}:${turn.id}:${input.marker}\n`,
    )
    await delay(input.delaySeconds, turn.signal)
    let deniedAttempts = 0
    if (input.mode === "network-deny") {
      for (let attempt = 0; attempt < input.denyAttempts; attempt += 1) {
        try {
          const response = await fetch(
            "http://169.254.169.254/latest/meta-data/",
            { signal: AbortSignal.any([turn.signal, AbortSignal.timeout(1_000)]) },
          )
          throw new Error(`private metadata request unexpectedly returned ${response.status}`)
        } catch (error) {
          if (
            error instanceof Error &&
            error.message.startsWith("private metadata request unexpectedly returned")
          ) {
            throw error
          }
          deniedAttempts += 1
        }
      }
    }
    await delay(input.holdSeconds, turn.signal)
    const attempts = await readFile("fault-probe-turns.log", "utf8")
    return {
      marker: input.marker,
      mode: input.mode,
      turnId: turn.id,
      sessionId: ctx.session.id,
      turnsObserved: attempts.trim().split("\n").length,
      deniedAttempts,
    }
  },
})

async function delay(seconds: number, signal: AbortSignal): Promise<void> {
  await sleep(seconds * 1_000, undefined, { signal })
}
