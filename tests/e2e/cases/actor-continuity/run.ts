import { verify, assert, assertEqual, errorCode, deadline } from "../../support/context"
import type { ComputerRef, SessionRef, Session, Run } from "@helmr/sdk"
import type { childTaskSmoke } from "../child-tasks/task"
await verify("actor-continuity", async ({ client, marker, objects, cleanup }) => {
  const targetComputer = await client.sandboxes.createComputer("helmr-child-task-target-smoke", {
    key: `child-target-${marker}`,
    idempotencyKey: `child-target:create:${marker}`,
  })
  objects.computer_ids.push(targetComputer.id)
  cleanup(() => deleteChildSmokeComputer(targetComputer, `child-target:delete:${marker}`))
  const taskCallerComputer = await client.sandboxes.createComputer(
    "helmr-child-task-caller-smoke",
    {
      key: `child-task-caller-${marker}`,
      idempotencyKey: `child-task-caller:create:${marker}`,
    },
  )
  objects.computer_ids.push(taskCallerComputer.id)
  cleanup(() =>
    deleteChildSmokeComputer(taskCallerComputer, `child-task-caller:delete:${marker}`),
  )
  const task = await client.tasks.start<typeof childTaskSmoke>("child-task-smoke", {
    payload: {
      mode: "call-success",
      marker: `${marker}-task-call`,
      childComputerId: targetComputer.id,
    },
    computer: taskCallerComputer,
    idempotencyKey: `child-task:start:${marker}`,
  })
  objects.run_ids.push(task.id)
  const taskOutput = await client.runs
    .wait(task, { signal: AbortSignal.timeout(20 * 60 * 1_000) })
    .unwrap()
  assert(
    taskOutput.mode === "call-success" && taskOutput.marker === `${marker}-task-call`,
    "Task child call output did not match the requested smoke marker",
  )

  const actorComputer = await client.sandboxes.createComputer("helmr-child-task-caller-smoke", {
    key: `child-actor-caller-${marker}`,
    idempotencyKey: `child-actor-caller:create:${marker}`,
  })
  objects.computer_ids.push(actorComputer.id)
  cleanup(() => deleteChildSmokeComputer(actorComputer, `child-actor-caller:delete:${marker}`))
  const actor = await client.actors.start("child-task-smoke-actor", {
    key: `child-call:${marker}`,
    computer: actorComputer,
    idempotencyKey: `child-actor:start:${marker}`,
  })
  cleanup(() => closeSmokeActor(actor.session))
  objects.session_ids.push(actor.session.id)
  objects.run_ids.push(actor.run.id)
  const firstTurn = await actor.session.enqueue(
    {
      marker: `${marker}-actor-call`,
      childComputerId: targetComputer.id,
    },
    { idempotencyKey: `child-actor:first:${marker}` },
  )
  const actorRun = await waitForTerminalRun(actor.run.id)
  assertEqual(actorRun.status, "succeeded", "Actor child call Run did not succeed")
  assertEqual(
    (await firstTurn.retrieve()).status,
    "completed",
    "Actor returned before explicit Turn completion",
  )
  const actorOutput = await waitForActorOutput(actor.session)
  assert(
    actorOutput.data !== null &&
      typeof actorOutput.data === "object" &&
      "kind" in actorOutput.data &&
      actorOutput.data.kind === "child-task-call-completed" &&
      "marker" in actorOutput.data &&
      actorOutput.data.marker === `${marker}-actor-call` &&
      "childRunId" in actorOutput.data &&
      typeof actorOutput.data.childRunId === "string",
    "Actor output did not contain its child Task result",
  )
  const actorChildRunId = actorOutput.data.childRunId
  const actorByID = client.sessions.ref(actor.session.id)
  const sessionMatches = await client.sessions.list({
    actorId: "child-task-smoke-actor",
    key: `child-call:${marker}`,
  })
  assertEqual(sessionMatches.items.length, 1, "Session key lookup was not exact")
  const actorByKey = client.sessions.ref(sessionMatches.items[0]!.id)
  const replayedInput = {
    marker: `${marker}-actor-continuation`,
    childComputerId: targetComputer.id,
  }
  const sent = await actorByKey.enqueue(
    replayedInput,
    { idempotencyKey: `child-actor:input:${marker}` },
    { signal: AbortSignal.timeout(30_000) },
  )
  const replayedSend = await actorByID.enqueue(
    replayedInput,
    { idempotencyKey: `child-actor:input:${marker}` },
    { signal: AbortSignal.timeout(30_000) },
  )
  assertEqual(replayedSend.id, sent.id, "Actor input idempotency replay changed the Turn ID")
  const actorOutputs = await waitForActorOutputs(actorByID, 2)
  const continuationOutput = actorOutputs.find(
    (record) =>
      record.data !== null &&
      typeof record.data === "object" &&
      "marker" in record.data &&
      record.data.marker === `${marker}-actor-continuation`,
  )
  assert(continuationOutput !== undefined, "Actor output omitted the continuation marker")
  assert(continuationOutput.provenance !== null, "Actor output is missing Run provenance")
  const actorContinuationRunId = continuationOutput.provenance.runId
  assert(actorContinuationRunId !== actor.run.id, "Actor continuation reused its initial Run")
  objects.run_ids.push(actorContinuationRunId, actorChildRunId)
  const actorContinuationRun = await waitForTerminalRun(actorContinuationRunId)
  assertEqual(actorContinuationRun.status, "succeeded", "Actor continuation Run did not succeed")
  const outputSequences = actorOutputs.map((record) => record.sequence)
  assert(
    outputSequences.every(
      (sequence, index) => index === 0 || sequence > outputSequences[index - 1]!,
    ),
    "Actor output sequences were not strictly ordered",
  )
  const streamedOutputs = []
  let after = 0
  for (;;) {
    const page = await actorByID.events.list(
      { after, limit: 1 },
      { signal: AbortSignal.timeout(30_000) },
    )
    streamedOutputs.push(...page.records.filter((event) => event.kind === "output"))
    if (!page.hasMore) break
    after = page.nextAfter
  }
  assertEqual(
    streamedOutputs.length,
    actorOutputs.length,
    "Actor streaming output read did not paginate through all records",
  )

  await closeSmokeActor(actorByID)
  const retainedOutputs = await actorByID.events.list({ after: 0, limit: 100 })
  assertEqual(
    retainedOutputs.records.filter((event) => event.kind === "output").length,
    actorOutputs.length,
    "Actor close discarded durable output",
  )
  return {
    targetComputerId: targetComputer.id,
    taskCallerComputerId: taskCallerComputer.id,
    taskRunId: task.id,
    taskOutput,
    actorComputerId: actorComputer.id,
    sessionId: actor.session.id,
    actorRunId: actor.run.id,
    actorContinuationRunId,
    actorChildRunId,
    actorOutputSequences: outputSequences,
  }
  async function deleteChildSmokeComputer(
    ref: ComputerRef,
    idempotencyKey: string,
  ): Promise<void> {
    try {
      await ref.delete({ idempotencyKey })
    } catch (error) {
      if (errorCode(error) !== "computer_not_found") throw error
    }
  }

  async function waitForTerminalRun(runId: string): Promise<Run> {
    const deadline = Date.now() + 20 * 60 * 1_000
    for (;;) {
      const run = await client.runs.retrieve(runId, { signal: AbortSignal.timeout(30_000) })
      if (
        run.status === "succeeded" ||
        run.status === "failed" ||
        run.status === "cancelled" ||
        run.status === "expired" ||
        run.status === "system_failed"
      ) {
        return run
      }
      if (Date.now() >= deadline) {
        throw new Error(`timed out waiting for Actor Run ${runId}`)
      }
      await new Promise((resolve) => setTimeout(resolve, 500))
    }
  }

  async function waitForActorOutput(ref: SessionRef) {
    const deadline = Date.now() + 5 * 60_000
    for (;;) {
      const page = await ref.events.list({ after: 0, limit: 100 })
      const output = page.records.find((event) => event.kind === "output")
      if (output !== undefined) return output
      if (Date.now() >= deadline) {
        throw new Error(`timed out waiting for Actor output ${ref.id}`)
      }
      await new Promise((resolve) => setTimeout(resolve, 500))
    }
  }

  async function waitForActorOutputs(ref: SessionRef, count: number) {
    const deadline = Date.now() + 5 * 60_000
    for (;;) {
      const page = await ref.events.list({ after: 0, limit: 100 })
      const outputs = page.records.filter((event) => event.kind === "output")
      if (outputs.length >= count) return outputs
      if (Date.now() >= deadline) {
        throw new Error(`timed out waiting for ${count} Actor outputs ${ref.id}`)
      }
      await new Promise((resolve) => setTimeout(resolve, 500))
    }
  }

  async function closeSmokeActor(ref: SessionRef): Promise<void> {
    await ref.close({
      idempotencyKey: `child-actor:close:${marker}`,
    })
    const status = await waitForActorClosed(ref)
    assertEqual(status.status, "closed", "Actor did not close after its smoke turn")
  }

  async function waitForActorClosed(ref: SessionRef): Promise<Session> {
    const deadline = Date.now() + 20 * 60 * 1_000
    for (;;) {
      const status = await client.sessions.retrieve(ref.id)
      if (status.status === "closed") return status
      if (status.status === "failed") {
        return status
      }
      if (Date.now() >= deadline) {
        throw new Error(`timed out closing Actor ${ref.id}`)
      }
      await new Promise((resolve) => setTimeout(resolve, 500))
    }
  }
})
