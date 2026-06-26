import { cache, image, logger, sandbox, source, streams, task } from "@helmr/sdk"
import { appendFile, readFile, writeFile } from "node:fs/promises"
import { z } from "zod"

const dependencyInputs = source.directory(".", {
  ignore: ["*", "!package.json", "!bun.lock", "!tsconfig.json", "!vendor", "!vendor/**"],
})

const base = image("helmr-stream-input-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .copy("/opt/helmr-task", dependencyInputs)
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .workdir("/opt/helmr-task")
  .run(["bun", "install", "--frozen-lockfile"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("stream-input-smoke-bun") }],
  })
  .workdir("/workspace")

const sbx = sandbox("helmr-stream-input-smoke")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

const payload = z.object({
  marker: z.string().optional(),
  correlationId: z.string().optional(),
  firstTimeout: z.number().int().positive().max(900).default(300),
  secondTimeout: z.number().int().positive().max(900).default(300),
}).strict()

type Payload = z.infer<typeof payload>

const input = streams.input("input-smoke", {
  schema: z.object({
    step: z.enum(["approve", "message"]),
    approved: z.boolean().optional(),
    text: z.string().optional(),
  }).strict(),
})
const reportStream = streams.output("stream-input-smoke.report", { schema: z.unknown() })

const statePath = "stream-input-smoke-state.json"
const logPath = "stream-input-smoke.log"
const reportPath = "stream-input-smoke-report.json"

interface State {
  readonly marker: string
  readonly pid: number
  readonly steps: readonly string[]
}

export const streamInputSmoke = task({
  id: "stream-input-smoke",
  sandbox: sbx,
  maxDuration: 900,
  payload,
  run: async (payload: Payload, ctx) => {
    const marker = payload.marker?.trim() || `stream-input-smoke-${ctx.run.id}`
    const correlationId = payload.correlationId?.trim() || undefined
    const steps = ["start"]
    await writeJson(statePath, { marker, pid: process.pid, steps })
    await appendLog("start", { marker, correlationId, runId: ctx.run.id, pid: process.pid })

    logger.info({ phase: "stream-input-smoke", step: "before-approval", marker })
    const approval = await input.wait({
      correlationId,
      timeout: payload.firstTimeout,
      tags: ["smoke", "stream-input"],
      metadata: { marker, subject: `Approve stream input smoke marker ${marker}` },
    }).unwrap()
    if (approval.step !== "approve" || approval.approved !== true) {
      throw new Error(`unexpected approval input: ${JSON.stringify(approval)}`)
    }
    await assertState(marker, process.pid, ["start"])
    steps.push("after-approval")
    await writeJson(statePath, { marker, pid: process.pid, steps })
    await appendLog("after-approval", approval)

    logger.info({ phase: "stream-input-smoke", step: "before-message", marker })
    const message = await input.wait({
      correlationId,
      timeout: payload.secondTimeout,
      tags: ["smoke", "stream-input"],
      metadata: { marker, subject: `Send message input for smoke marker ${marker}` },
    }).unwrap()
    if (message.step !== "message" || typeof message.text !== "string" || message.text.trim() === "") {
      throw new Error(`unexpected message input: ${JSON.stringify(message)}`)
    }
    await assertState(marker, process.pid, ["start", "after-approval"])
    steps.push("after-message")
    await writeJson(statePath, { marker, pid: process.pid, steps })
    await appendLog("after-message", message)

    const report = {
      ok: true,
      marker,
      correlationId,
      runId: ctx.run.id,
      pid: process.pid,
      approval,
      message,
      steps,
      files: {
        state: statePath,
        log: logPath,
        report: reportPath,
      },
    }
    await writeJson(reportPath, report)
    await reportStream.append(report, { contentType: "application/vnd.helmr.stream-input-smoke+json" })
    logger.info({ phase: "stream-input-smoke", step: "completed", marker })
    return report
  },
})

async function assertState(marker: string, pid: number, expectedSteps: readonly string[]): Promise<void> {
  const state = await readJson<State>(statePath)
  if (state.marker !== marker) {
    throw new Error(`state marker mismatch: ${state.marker} != ${marker}`)
  }
  if (state.pid !== pid || process.pid !== pid) {
    throw new Error(`process pid changed across checkpoint: state=${state.pid} memory=${pid} current=${process.pid}`)
  }
  for (const step of expectedSteps) {
    if (!state.steps.includes(step)) {
      throw new Error(`state file is missing step ${step}`)
    }
  }
}

async function appendLog(step: string, value: unknown): Promise<void> {
  await appendFile(logPath, `${JSON.stringify({ step, at: new Date().toISOString(), value })}\n`)
}

async function writeJson(path: string, value: unknown): Promise<void> {
  await writeFile(path, `${JSON.stringify(value, null, 2)}\n`)
}

async function readJson<T>(path: string): Promise<T> {
  return JSON.parse(await readFile(path, "utf8")) as T
}
