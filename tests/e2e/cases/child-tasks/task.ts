import {
  actor,
  image,
  task,
  sandbox,
  computers,
} from "@helmr/sdk"
import { readFile, writeFile } from "node:fs/promises"
import { setTimeout as sleep } from "node:timers/promises"
import { z } from "zod"

const base = image("helmr-child-task-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/sandbox")
  .workdir("/sandbox")

export const childTaskSmokeCallerComputer = sandbox(
  { id: "helmr-child-task-caller-smoke" },
)
  .image(base)
  .resources({ cpu: 1, memory: "1GiB" })

export const childTaskSmokeTargetComputer = sandbox(
  { id: "helmr-child-task-target-smoke" },
)
  .image(base)
  .resources({ cpu: 1, memory: "1GiB" })

const childPayload = z.object({
  marker: z.string().min(1),
  fail: z.boolean().default(false),
  holdSeconds: z.number().int().min(0).max(240).default(0),
}).strict()

type ChildPayload = z.infer<typeof childPayload>

export const childTaskSmokeChild = task({
  id: "child-task-smoke-child",
  maxDuration: "5m",
  payload: childPayload,
  run: async (input: ChildPayload, ctx) => {
    await writeFile(
      "child-task-smoke.json",
      `${JSON.stringify({
        marker: input.marker,
        runId: ctx.run.id,
        attemptNumber: ctx.run.attemptNumber,
      }, null, 2)}\n`,
    )
    if (input.holdSeconds > 0) {
      await sleep(input.holdSeconds * 1000)
    }
    if (input.fail) {
      throw new Error(`intentional child Task failure for ${input.marker}`)
    }
    return {
      marker: input.marker,
      childRunId: ctx.run.id,
      attemptNumber: ctx.run.attemptNumber,
    }
  },
})

const callerPayload = z.object({
  mode: z.enum([
    "call-success",
    "call-failure",
    "same-sandbox-call",
    "start-detached",
  ]),
  marker: z.string().min(1),
  childComputerId: z.string().min(1).optional(),
  holdSeconds: z.number().int().min(0).max(240).default(0),
}).strict()

type CallerPayload = z.infer<typeof callerPayload>

export const childTaskSmoke = task({
  id: "child-task-smoke",
  maxDuration: "10m",
  payload: callerPayload,
  run: async (input: CallerPayload, ctx) => {
    if (input.mode !== "same-sandbox-call" && input.childComputerId === undefined) {
      throw new Error("childComputerId is required for a separate-Computer child")
    }
    if (input.mode === "same-sandbox-call" && ctx.computer === null) {
      throw new Error("same-Computer child call requires a Computer")
    }
    const childComputer = input.mode === "same-sandbox-call"
      ? ctx.computer!
      : computers.ref(input.childComputerId!)
    const childInput = {
      marker: input.marker,
      fail: input.mode === "call-failure",
      holdSeconds: input.holdSeconds,
    }
    const options = {
      computer: childComputer,
      idempotencyKey: `${ctx.run.id}:${input.mode}`,
      metadata: { marker: input.marker, smokeMode: input.mode },
      tags: ["smoke", "child-task", input.mode],
    } as const

    if (input.mode === "start-detached") {
      const child = await childTaskSmokeChild.start(childInput, options)
      return {
        ok: true,
        mode: input.mode,
        marker: input.marker,
        childRunId: child.id,
        childAttemptNumber: null,
        childFailure: null,
        sameComputerMarkerObserved: false,
      }
    }

    const child = childTaskSmokeChild.call(childInput, options)
    if (input.mode === "call-success" || input.mode === "same-sandbox-call") {
      const output = await child.unwrap()
      if (output.marker !== input.marker || output.childRunId === ctx.run.id) {
        throw new Error("successful child Task result did not match its parent call")
      }
      let sharedMarker: string | null = null
      if (input.mode === "same-sandbox-call") {
        sharedMarker = await readFile("child-task-smoke.json", "utf8")
        if (!sharedMarker.includes(input.marker) || !sharedMarker.includes(output.childRunId)) {
          throw new Error("resumed parent did not observe the child Computer marker")
        }
      }
      return {
        ok: true,
        mode: input.mode,
        marker: input.marker,
        childRunId: output.childRunId,
        childAttemptNumber: output.attemptNumber,
        childFailure: null,
        sameComputerMarkerObserved: sharedMarker !== null,
      }
    }

    const result = await child
    if (result.ok) {
      throw new Error("failing child Task unexpectedly succeeded")
    }
    return {
      ok: true,
      mode: input.mode,
      marker: input.marker,
      childRunId: result.run.id,
      childAttemptNumber: null,
      childFailure: {
        code: result.failure.code,
        message: result.failure.message,
        details: result.failure.details,
      },
      sameComputerMarkerObserved: false,
    }
  },
})

const actorInput = z.object({
  marker: z.string().min(1),
  childComputerId: z.string().min(1),
}).strict()

export const childTaskSmokeActor = actor({
  id: "child-task-smoke-actor",
  maxDuration: "10m",
  idleTimeout: "2m",
  run: async (self, ctx) => {
    const turn = await self.receive({ timeout: "2m" })
    if (turn === null) return
    const input = actorInput.parse(turn.input)
    const output = await childTaskSmokeChild.call(
      { marker: input.marker, fail: false, holdSeconds: 0 },
      {
        computer: computers.ref(input.childComputerId),
        idempotencyKey: `${ctx.run.id}:actor-call`,
        metadata: { marker: input.marker, smokeMode: "actor-call" },
        tags: ["smoke", "child-task", "actor-call"],
      },
    ).unwrap()
    await turn.output.write(
      {
        kind: "child-task-call-completed",
        marker: output.marker,
        childRunId: output.childRunId,
        actorRunId: ctx.run.id,
        inputRecordId: turn.id,
      },
      { idempotencyKey: `turn:${turn.id}:actor-output` },
    )
    await turn.complete()
  },
})
