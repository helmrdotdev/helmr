import { image, sandbox, task } from "@helmr/sdk"
import { readFile, writeFile } from "node:fs/promises"
import { z } from "zod"

// No package installation or application-specific services in the first VM case.
export const verificationSandbox = sandbox({ id: "verification" })
  .image(image("verification").from("node:24-bookworm-slim").workdir("/sandbox"))
  .resources({ cpu: 1, memory: "1GiB" })

export const verificationTask = task({
  id: "verification-task",
  maxDuration: "2m",
  payload: z.object({ marker: z.string().min(1) }).strict(),
  run: async ({ marker }, ctx) => {
    await writeFile("verification-marker.txt", marker)
    const observed = await readFile("verification-marker.txt", "utf8")
    if (observed !== marker) throw new Error("guest filesystem round trip failed")
    return { marker: observed, runId: ctx.run.id, computerId: ctx.computer.id }
  },
})
