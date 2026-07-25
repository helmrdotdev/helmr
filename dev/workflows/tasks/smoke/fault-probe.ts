import { image, task, workspace, type JsonValue } from "@helmr/sdk"
import { appendFile, readFile } from "node:fs/promises"
import { z } from "zod"

const base = image("helmr-fault-probe")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")

export const faultProbeWorkspace = workspace("helmr-fault-probe")
  .image(base)
  .resources({ cpu: 1, memory: "1GiB" })

const payload = z.object({
  marker: z.string().min(1),
  mode: z.enum(["hold", "network-deny"]),
  delaySeconds: z.number().int().min(0).max(120).default(0),
  holdSeconds: z.number().int().min(1).max(900).default(30),
  retryHoldSeconds: z.number().int().min(1).max(900).optional(),
  denyAttempts: z.number().int().min(1).max(20).default(3),
}).strict()

type Payload = z.infer<typeof payload>

export const faultProbe = task({
  id: "fault-probe",
  maxDuration: "20m",
  retry: {
    maxAttempts: 3,
    backoff: { minDelay: "1s", maxDelay: "5s", factor: 2 },
  },
  payload,
  run: async (input: Payload, ctx): Promise<JsonValue> => {
    await appendFile(
      "fault-probe-attempts.log",
      `${ctx.run.id}:${ctx.run.attemptNumber}:${input.marker}\n`,
    )
    await delay(input.delaySeconds)
    let deniedAttempts = 0
    if (input.mode === "network-deny") {
      for (let attempt = 0; attempt < input.denyAttempts; attempt += 1) {
        try {
          const response = await fetch(
            "http://169.254.169.254/latest/meta-data/",
            { signal: AbortSignal.timeout(1_000) },
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
    await delay(
      ctx.run.attemptNumber > 1 && input.retryHoldSeconds !== undefined
        ? input.retryHoldSeconds
        : input.holdSeconds,
    )
    const attempts = await readFile("fault-probe-attempts.log", "utf8")
    return {
      marker: input.marker,
      mode: input.mode,
      runId: ctx.run.id,
      attemptNumber: ctx.run.attemptNumber,
      attemptsObserved: attempts.trim().split("\n").length,
      deniedAttempts,
    }
  },
})

async function delay(seconds: number): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, seconds * 1_000))
}
