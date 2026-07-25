import {
  actor,
  image,
  task,
  workspace,
  workspaces,
} from "@helmr/sdk"
import { writeFile } from "node:fs/promises"
import { z } from "zod"

const base = image("helmr-child-task-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .workdir("/workspace")

export const childTaskSmokeCallerWorkspace = workspace(
  "helmr-child-task-caller-smoke",
)
  .image(base)
  .resources({ cpu: 1, memory: "1GiB" })

export const childTaskSmokeTargetWorkspace = workspace(
  "helmr-child-task-target-smoke",
)
  .image(base)
  .resources({ cpu: 1, memory: "1GiB" })

const childPayload = z.object({
  marker: z.string().min(1),
  fail: z.boolean().default(false),
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
  mode: z.enum(["call-success", "call-failure", "start-detached"]),
  marker: z.string().min(1),
  childWorkspaceId: z.string().min(1),
}).strict()

type CallerPayload = z.infer<typeof callerPayload>

export const childTaskSmoke = task({
  id: "child-task-smoke",
  maxDuration: "10m",
  payload: callerPayload,
  run: async (input: CallerPayload, ctx) => {
    const childWorkspace = workspaces.ref({ id: input.childWorkspaceId })
    const childInput = {
      marker: input.marker,
      fail: input.mode === "call-failure",
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
        childError: null,
      }
    }

    const child = childTaskSmokeChild.call(childInput, options)
    if (input.mode === "call-success") {
      const output = await child.unwrap()
      if (output.marker !== input.marker || output.childRunId === ctx.run.id) {
        throw new Error("successful child Task result did not match its parent call")
      }
      return {
        ok: true,
        mode: input.mode,
        marker: input.marker,
        childRunId: output.childRunId,
        childAttemptNumber: output.attemptNumber,
        childError: null,
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
      childError: {
        code: result.error.code,
        message: result.error.message,
        retryable: result.error.retryable,
      },
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
      if (received.error.code === "actor_closed") return
      throw received.error
    }
    const input = actorInput.parse(received.value)
    const output = await childTaskSmokeChild.call(
      { marker: input.marker, fail: false },
      {
        workspace: workspaces.ref({ id: input.childWorkspaceId }),
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
      },
      { idempotencyKey: `${ctx.run.id}:actor-output` },
    )
  },
})
