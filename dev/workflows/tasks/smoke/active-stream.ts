import { cache, image, logger, sandbox, source, streams, task } from "@helmr/sdk"
import { appendFile, writeFile } from "node:fs/promises"
import { z } from "zod"

const dependencyInputs = source.directory(".", {
  ignore: ["*", "!package.json", "!bun.lock", "!tsconfig.json", "!vendor", "!vendor/**"],
})

const base = image("helmr-active-stream-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .copy("/opt/helmr-task", dependencyInputs)
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .workdir("/opt/helmr-task")
  .run(["bun", "install", "--frozen-lockfile"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("active-stream-smoke-bun") }],
  })
  .workdir("/workspace")

const sbx = sandbox("helmr-active-stream-smoke")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

const payload = z.object({
  marker: z.string().optional(),
  correlationId: z.string().optional(),
  timeout: z.number().int().positive().max(900).default(300),
}).strict()

type Payload = z.infer<typeof payload>

const activeInput = streams.input("active-stream-smoke.input", {
  schema: z.object({
    step: z.enum(["once", "on-one", "on-two"]),
    value: z.string(),
  }).strict(),
})
const reportStream = streams.output("active-stream-smoke.report", { schema: z.unknown() })

const logPath = "active-stream-smoke.log"
const reportPath = "active-stream-smoke-report.json"
const doneSentinel = "active-stream-smoke-on-complete"

export const activeStreamSmoke = task({
  id: "active-stream-smoke",
  sandbox: sbx,
  maxDuration: 900,
  streams: [activeInput, reportStream],
  payload,
  run: async (payload: Payload, ctx) => {
    const marker = payload.marker?.trim() || `active-stream-smoke-${ctx.run.id}`
    const correlationId = payload.correlationId?.trim() || marker
    const steps: string[] = ["start"]
    await appendLog("start", { marker, correlationId, runId: ctx.run.id, pid: process.pid })

    await publishMilestone(marker, "ready-for-empty-peek")
    const emptyPeek = await activeInput.peek({ correlationId })
    if (emptyPeek !== null) {
      throw new Error(`expected empty peek before input, got ${JSON.stringify(emptyPeek)}`)
    }
    steps.push("empty-peek")

    await publishMilestone(marker, "ready-for-once")
    const oncePayload = await activeInput.once({ correlationId, timeout: payload.timeout }).unwrap()
    if (oncePayload.step !== "once") {
      throw new Error(`unexpected once payload: ${JSON.stringify(oncePayload)}`)
    }
    steps.push("once")

    const firstPeek = await activeInput.peek({ correlationId })
    const secondPeek = await activeInput.peek({ correlationId })
    if (firstPeek === null || secondPeek === null) {
      throw new Error("expected peek to replay the first active stream record")
    }
    if (firstPeek.id !== secondPeek.id || firstPeek.sequence !== secondPeek.sequence) {
      throw new Error(`peek advanced position: first=${JSON.stringify(firstPeek)} second=${JSON.stringify(secondPeek)}`)
    }
    if (firstPeek.data.step !== "once") {
      throw new Error(`unexpected peek payload: ${JSON.stringify(firstPeek.data)}`)
    }
    steps.push("peek-replay")

    const afterOncePeek = await activeInput.peek({ correlationId, afterSequence: firstPeek.sequence })
    if (afterOncePeek !== null) {
      throw new Error(`expected no record after once sequence before on input, got ${JSON.stringify(afterOncePeek)}`)
    }
    steps.push("peek-after-sequence-empty")

    await publishMilestone(marker, "ready-for-on")
    const onValues: string[] = []
    try {
      await activeInput.on(async (record) => {
        onValues.push(record.value)
        await appendLog("on-record", record)
        if (onValues.length === 2) {
          throw new Error(doneSentinel)
        }
      }, { correlationId, afterSequence: firstPeek.sequence, timeout: payload.timeout })
    } catch (error) {
      if (!(error instanceof Error) || error.message !== doneSentinel) {
        throw error
      }
    }
    if (onValues.join(",") !== "on-one,on-two") {
      throw new Error(`unexpected on values: ${JSON.stringify(onValues)}`)
    }
    steps.push("on")

    const report = {
      ok: true,
      marker,
      correlationId,
      runId: ctx.run.id,
      pid: process.pid,
      steps,
      once: oncePayload,
      peek: {
        id: firstPeek.id,
        sequence: firstPeek.sequence,
        data: firstPeek.data,
      },
      onValues,
      files: {
        log: logPath,
        report: reportPath,
      },
    }
    await writeFile(reportPath, `${JSON.stringify(report, null, 2)}\n`)
    await reportStream.append({ marker, phase: "completed", report }, { contentType: "application/vnd.helmr.active-stream-smoke+json" })
    logger.info({ phase: "active-stream-smoke", step: "completed", marker })
    return report
  },
})

async function publishMilestone(marker: string, phase: string): Promise<void> {
  logger.info({ phase: "active-stream-smoke", step: phase, marker })
  await appendLog(phase, { marker })
  await reportStream.append({ marker, phase }, { contentType: "application/vnd.helmr.active-stream-smoke+json" })
}

async function appendLog(step: string, value: unknown): Promise<void> {
  await appendFile(logPath, `${JSON.stringify({ step, at: new Date().toISOString(), value })}\n`)
}
