import {
  actor,
  image,
  task,
  sandbox,
  workspaces,
} from "@helmr/sdk"
import { readFile, writeFile } from "node:fs/promises"
import { setTimeout as sleep } from "node:timers/promises"
import { z } from "zod"

const base = image("helmr-child-task-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/sandbox")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .workdir("/sandbox")

export const childTaskSmokeCallerWorkspace = sandbox(
  { id: "helmr-child-task-caller-smoke" },
)
  .image(base)
  .resources({ cpu: 1, memory: "1GiB" })

export const childTaskSmokeTargetWorkspace = sandbox(
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
  childWorkspaceId: z.string().min(1).optional(),
  holdSeconds: z.number().int().min(0).max(240).default(0),
}).strict()

type CallerPayload = z.infer<typeof callerPayload>

export const childTaskSmoke = task({
  id: "child-task-smoke",
  maxDuration: "10m",
  payload: callerPayload,
  run: async (input: CallerPayload, ctx) => {
    if (input.mode !== "same-sandbox-call" && input.childWorkspaceId === undefined) {
      throw new Error("childWorkspaceId is required for a separate-Workspace child")
    }
    if (input.mode === "same-sandbox-call" && ctx.workspace === null) {
      throw new Error("same-Workspace child call requires a Workspace")
    }
    const childWorkspace = input.mode === "same-sandbox-call"
      ? ctx.workspace!
      : workspaces.ref(input.childWorkspaceId!)
    const childInput = {
      marker: input.marker,
      fail: input.mode === "call-failure",
      holdSeconds: input.holdSeconds,
    }
    const options = {
      workspace: childWorkspace,
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
        sameWorkspaceMarkerObserved: false,
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
          throw new Error("resumed parent did not observe the child Workspace marker")
        }
      }
      return {
        ok: true,
        mode: input.mode,
        marker: input.marker,
        childRunId: output.childRunId,
        childAttemptNumber: output.attemptNumber,
        childFailure: null,
        sameWorkspaceMarkerObserved: sharedMarker !== null,
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
      sameWorkspaceMarkerObserved: false,
    }
  },
})

const actorInput = z.object({
  marker: z.string().min(1),
  childWorkspaceId: z.string().min(1),
}).strict()

export const childTaskSmokeActor = actor({
  id: "child-task-smoke-actor",
  maxDuration: "10m",
  idleTimeout: "2m",
  run: async (self, ctx) => {
    const received = await self.input.receive({ timeout: "2m" })
    if (!received.ok) {
      if (received.error.code === "session_closed") return
      throw received.error
    }
    const input = actorInput.parse(received.value)
    const output = await childTaskSmokeChild.call(
      { marker: input.marker, fail: false, holdSeconds: 0 },
      {
        workspace: workspaces.ref(input.childWorkspaceId),
        idempotencyKey: `${ctx.run.id}:actor-call`,
        metadata: { marker: input.marker, smokeMode: "actor-call" },
        tags: ["smoke", "child-task", "actor-call"],
      },
    ).unwrap()
    await self.output.append(
      {
        kind: "child-task-call-completed",
        marker: output.marker,
        childRunId: output.childRunId,
        actorRunId: ctx.run.id,
        inputRecordId: received.record.id,
      },
      { idempotencyKey: `input:${received.record.id}:actor-output` },
    )
  },
})
