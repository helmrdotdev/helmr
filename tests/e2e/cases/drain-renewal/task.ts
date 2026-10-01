import { image, task, sandbox, logger } from "@helmr/sdk"
import { randomUUID } from "node:crypto"
import { readFile, writeFile } from "node:fs/promises"
import { z } from "zod"

export const renewalComputer = sandbox({ id: "verification-drain-renewal" })
  .image(image("verification-drain-renewal").from("node:24-bookworm-slim").workdir("/sandbox"))
  .resources({ cpu: 1, memory: "1GiB" })

export const renewalTask = task({
  id: "verification-drain-renewal", maxDuration: "40m", retry: { maxAttempts: 1 },
  payload: z.object({ marker: z.string() }).strict(),
  run: async ({ marker }, ctx) => {
    const nonce = randomUUID()
    await writeFile("drain-renewal-marker", marker)
    await logger.info("drain renewal probe started", { marker, nonce })
    // Ordinary JS timers keep this Task resident; a managed wait could checkpoint
    // it and would not exercise continued execution grants during planned drain.
    for (let tick = 1; tick <= 72; tick++) {
      await new Promise(resolve => setTimeout(resolve, 30_000))
      await logger.info("drain renewal probe running", { marker, nonce, tick })
    }
    if (await readFile("drain-renewal-marker", "utf8") !== marker) throw new Error("drain lost the file")
    return { marker, nonce, runId: ctx.run.id, attemptNumber: ctx.run.attemptNumber }
  },
})
