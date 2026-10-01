import { image, task, sandbox, logger } from "@helmr/sdk"
import { randomUUID } from "node:crypto"
import { readFile, writeFile } from "node:fs/promises"
import { z } from "zod"

export const outageComputer = sandbox({ id: "verification-outage" })
  .image(image("verification-outage").from("node:24-bookworm-slim").workdir("/sandbox"))
  .resources({ cpu: 1, memory: "1GiB" })

export const outageTask = task({
  id: "verification-outage", maxDuration: "10m", retry: { maxAttempts: 1 },
  payload: z.object({ marker: z.string() }).strict(),
  run: async ({ marker }, ctx) => {
    const nonce = randomUUID()
    await writeFile("outage-marker", marker)
    await logger.info("outage probe started", { marker, nonce })
    await new Promise(resolve => setTimeout(resolve, 360_000))
    if (await readFile("outage-marker", "utf8") !== marker) throw new Error("outage lost the file")
    return { marker, nonce, runId: ctx.run.id, attemptNumber: ctx.run.attemptNumber }
  },
})
