import { createHash } from "node:crypto"
import { mkdir, rename, writeFile } from "node:fs/promises"
import { dirname } from "node:path"
import {
  HelmrClient,
  type ActorStatus,
  type ClientActorIdRef,
  type JsonValue,
  type RunEventRecord,
  type RunLogRecord,
  type RunSnapshot,
  type WorkspaceExecResult,
  type WorkspaceFileEntry,
  type WorkspaceIdRef,
} from "@helmr/sdk"
import type {
  childTaskSmoke,
  childTaskSmokeActor,
  childTaskSmokeCallerWorkspace,
  childTaskSmokeTargetWorkspace,
} from "../../workflows/tasks/smoke/child-task"
import type {
  runtimeSmoke,
  runtimeSmokeWorkspace,
} from "../../workflows/tasks/smoke/runtime"
import { assert, assertEqual } from "./assert"
import { readConfig } from "./config"
import {
  runManagementSmoke,
  type ManagementEvidence,
} from "./management-smoke"

type Evidence = {
  readonly marker: string
  readonly workspaceId: string
  readonly workspaceKey: string
  readonly initialExec: ExecEvidence
  readonly replayExec: ExecEvidence
  readonly committedFile: {
    readonly path: string
    readonly sizeBytes: number
    readonly digest: string
  }
  readonly taskRunId: string
  readonly taskOutput: JsonValue
  readonly taskLogs: readonly RunLogRecord[]
  readonly taskEvents: readonly RunEventRecord[]
  readonly childTasks: ChildTaskEvidence
  readonly management: ManagementEvidence
  readonly tokenId: string
  readonly secretDelivery: "covered" | "skipped"
  readonly deletedWorkspaceId: string
}

type ExecEvidence = {
  readonly exitCode: number
  readonly stdout: string
  readonly stderr: string
}

type ChildTaskEvidence = {
  readonly targetWorkspaceId: string
  readonly taskCallerWorkspaceId: string
  readonly taskRunId: string
  readonly taskOutput: JsonValue
  readonly actorWorkspaceId: string
  readonly actorId: string
  readonly actorRunId: string
  readonly actorContinuationRunId: string
  readonly actorChildRunId: string
  readonly actorOutputSequences: readonly number[]
}

const config = readConfig()
const client = new HelmrClient({ url: config.apiUrl, apiKey: config.apiKey })
let cleanupPrimaryWorkspace: (() => Promise<unknown>) | undefined
try {
  const evidence = await runSmoke()
  await writeClientSmokeResult(evidence)
  console.log(JSON.stringify(evidence, null, 2))
} catch (error) {
  if (cleanupPrimaryWorkspace !== undefined) {
    await Promise.allSettled([cleanupPrimaryWorkspace()])
  }
  await writeClientSmokeResult(undefined)
  throw error
}

async function runSmoke(): Promise<Evidence> {
  const workspaceKey = `runtime-smoke-${config.marker}`
  const workspaceCreateOptions = {
    key: workspaceKey,
    ...(config.secretName === undefined
      ? {}
      : {
          secrets: [{
            name: config.secretName,
            env: "HELMR_SMOKE_SECRET_VALUE",
          }],
        }),
    idempotencyKey: `workspace:create:${config.marker}`,
  } as const
  const created = await client.workspaces.create<typeof runtimeSmokeWorkspace>(
    "helmr-runtime-smoke",
    workspaceCreateOptions,
  )
  cleanupPrimaryWorkspace = () =>
    created.delete({ idempotencyKey: `workspace:delete:${config.marker}` })
  const replayedCreate = await client.workspaces.create<typeof runtimeSmokeWorkspace>(
    "helmr-runtime-smoke",
    workspaceCreateOptions,
  )
  assertEqual(replayedCreate.id, created.id, "workspace create replay changed the ID")
  let unexpectedConflictWorkspace: WorkspaceIdRef | undefined
  try {
    unexpectedConflictWorkspace = await client.workspaces.create<
      typeof runtimeSmokeWorkspace
    >(
      "helmr-runtime-smoke",
      {
        ...workspaceCreateOptions,
        key: `${workspaceKey}-conflict`,
      },
    )
  } catch (error) {
    assertEqual(
      errorCode(error),
      "idempotency_conflict",
      "divergent Workspace create returned the wrong error",
    )
  }
  if (unexpectedConflictWorkspace !== undefined) {
    await unexpectedConflictWorkspace.delete({
      idempotencyKey: `workspace:conflict-cleanup:${config.marker}`,
    })
    throw new Error("divergent Workspace create reused an idempotency key")
  }

  const byKey = client.workspaces.ref<typeof runtimeSmokeWorkspace>(
    "helmr-runtime-smoke",
    { key: workspaceKey },
  )
  const snapshot = await byKey.retrieve()
  assertEqual(snapshot.id, created.id, "workspace key resolved to a different Workspace")
  assertEqual(snapshot.declaredId, "helmr-runtime-smoke", "workspace declared ID mismatch")

  const markerPath = "workspace-smoke/nested/marker.txt"
  const stdin = `stdin:${config.marker}\n`
  const execOptions = {
    command: [
      "sh",
      "-ceu",
      [
        "mkdir -p workspace-smoke/nested",
        "IFS= read -r line",
        "printf 'marker=%s\\nstdin=%s\\n' \"$SMOKE_MARKER\" \"$line\" > workspace-smoke/nested/marker.txt",
        "printf 'stdout:%s:%s\\n' \"$SMOKE_MARKER\" \"$line\"",
        "printf 'stderr:%s\\n' \"$SMOKE_MARKER\" >&2",
        config.secretName === undefined
          ? "true"
          : "test -n \"$HELMR_SMOKE_SECRET_VALUE\" && printf 'secret:present\\n'",
        "exit 7",
      ].join("; "),
    ],
    cwd: "/workspace",
    env: { SMOKE_MARKER: config.marker },
    stdin: new TextEncoder().encode(stdin),
    timeout: "2m",
    idempotencyKey: `workspace:exec:${config.marker}`,
  } as const
  const initialExec = await byKey.exec(execOptions)
  assertExec(initialExec, config.marker)

  const replayExec = await byKey.exec(execOptions)
  assertExec(replayExec, config.marker)
  assertEqual(
    encodeExec(replayExec),
    encodeExec(initialExec),
    "BasicExec idempotency replay changed the result",
  )

  const fileBytes = await byKey.files.read(markerPath)
  const fileText = new TextDecoder().decode(fileBytes)
  assert(fileText.includes(`marker=${config.marker}`), "committed file missed the exec marker")
  assert(fileText.includes(`stdin=stdin:${config.marker}`), "committed file missed stdin")
  const fileStat = await byKey.files.stat(markerPath)
  assertFile(fileStat, markerPath, fileBytes.byteLength)
  const listing = await byKey.files.list("workspace-smoke/nested", { limit: 100 })
  assert(
    listing.items.some((entry) => entry.path === markerPath),
    "committed file was absent from the directory listing",
  )

  const taskMarker = `${config.marker}-task`
  const started = await client.tasks.start<typeof runtimeSmoke>(
    "runtime-smoke",
    {
      payload: {
        scenario: "workspace-basic-exec-client-smoke",
        marker: taskMarker,
        expectedWorkspaceMarker: config.marker,
        expectedEnvironment: "unknown",
        exerciseToken: false,
        tokenTimeout: 120,
        largeFileKiB: 256,
      },
      workspace: byKey,
      idempotencyKey: `task:start:${config.marker}`,
      tags: ["smoke", "workspace-basic-exec"],
      metadata: { marker: config.marker },
    },
  )
  const taskOutput = await client.runs.wait<typeof runtimeSmoke>(started.id, {
    signal: AbortSignal.timeout(20 * 60 * 1_000),
  }).unwrap()
  const taskLogs = await readTelemetry(() => client.runs.logs(started.id, { limit: 100 }))
  const taskEvents = await readTelemetry(() => client.runs.events(started.id, { limit: 100 }))
  for (const level of ["debug", "info", "warn", "error"] as const) {
    assert(
      taskLogs.items.some((record) =>
        record.kind === "structured" &&
        record.level === level &&
        record.attributes["marker"] === taskMarker
      ),
      `Task logs did not include the ${level} structured logger probe`,
    )
  }
  assert(
    taskEvents.items.some((event) => event.runId === started.id),
    "Task events did not include the completed Run",
  )
  const taskFile = new TextDecoder().decode(await byKey.files.read(markerPath))
  assert(taskFile.includes(`marker=${taskMarker}`), "Task did not advance the Workspace head")

  const childTasks = await runChildTaskSmoke()
  const management = await runManagementSmoke(client, config.marker)

  const token = await client.tokens.create({
    timeout: "10m",
    tags: ["smoke", "workspace-basic-exec"],
    metadata: { marker: config.marker },
    idempotencyKey: `token:create:${config.marker}`,
  })
  const tokenSnapshot = await client.tokens.retrieve(token.id)
  assertEqual(tokenSnapshot.id, token.id, "external Token retrieve changed the ID")
  const canceled = await client.tokens.cancel(token.id, {
    idempotencyKey: `token:cancel:${config.marker}`,
  })
  assertEqual(canceled.status, "canceled", "external Token was not canceled")

  const deleted = await byKey.delete({
    idempotencyKey: `workspace:delete:${config.marker}`,
  })
  assertEqual(deleted.workspaceId, created.id, "Workspace delete receipt changed the ID")
  const deleteReplay = await created.delete({
    idempotencyKey: `workspace:delete:${config.marker}`,
  })
  assertEqual(deleteReplay.workspaceId, created.id, "Workspace delete replay changed the ID")
  cleanupPrimaryWorkspace = undefined

  return {
    marker: config.marker,
    workspaceId: created.id,
    workspaceKey,
    initialExec: execEvidence(initialExec),
    replayExec: execEvidence(replayExec),
    committedFile: {
      path: markerPath,
      sizeBytes: fileBytes.byteLength,
      digest: createHash("sha256").update(fileBytes).digest("hex"),
    },
    taskRunId: started.id,
    taskOutput,
    taskLogs: taskLogs.items,
    taskEvents: taskEvents.items,
    childTasks,
    management,
    tokenId: token.id,
    secretDelivery: config.secretName === undefined ? "skipped" : "covered",
    deletedWorkspaceId: deleted.workspaceId,
  }
}

async function runChildTaskSmoke(): Promise<ChildTaskEvidence> {
  const workspaces: Array<Readonly<{
    ref: WorkspaceIdRef
    deleteKey: string
  }>> = []
  let actorRef: ClientActorIdRef | undefined
  try {
    const targetWorkspace = await client.workspaces.create<
      typeof childTaskSmokeTargetWorkspace
    >(
      "helmr-child-task-target-smoke",
      {
        key: `child-target-${config.marker}`,
        idempotencyKey: `child-target:create:${config.marker}`,
      },
    )
    workspaces.push({
      ref: targetWorkspace,
      deleteKey: `child-target:delete:${config.marker}`,
    })
    const taskCallerWorkspace = await client.workspaces.create<
      typeof childTaskSmokeCallerWorkspace
    >(
      "helmr-child-task-caller-smoke",
      {
        key: `child-task-caller-${config.marker}`,
        idempotencyKey: `child-task-caller:create:${config.marker}`,
      },
    )
    workspaces.push({
      ref: taskCallerWorkspace,
      deleteKey: `child-task-caller:delete:${config.marker}`,
    })
    const task = await client.tasks.start<typeof childTaskSmoke>(
      "child-task-smoke",
      {
        payload: {
          mode: "call-success",
          marker: `${config.marker}-task-call`,
          childWorkspaceId: targetWorkspace.id,
        },
        workspace: taskCallerWorkspace,
        idempotencyKey: `child-task:start:${config.marker}`,
      },
    )
    const taskOutput = await client.runs.wait<typeof childTaskSmoke>(
      task.id,
      { signal: AbortSignal.timeout(20 * 60 * 1_000) },
    ).unwrap()
    assert(
      taskOutput.mode === "call-success" &&
        taskOutput.marker === `${config.marker}-task-call`,
      "Task child call output did not match the requested smoke marker",
    )

    const actorWorkspace = await client.workspaces.create<
      typeof childTaskSmokeCallerWorkspace
    >(
      "helmr-child-task-caller-smoke",
      {
        key: `child-actor-caller-${config.marker}`,
        idempotencyKey: `child-actor-caller:create:${config.marker}`,
      },
    )
    workspaces.push({
      ref: actorWorkspace,
      deleteKey: `child-actor-caller:delete:${config.marker}`,
    })
    const actor = await client.actors.start<typeof childTaskSmokeActor>(
      "child-task-smoke-actor",
      {
        key: `child-call:${config.marker}`,
        input: {
          marker: `${config.marker}-actor-call`,
          childWorkspaceId: targetWorkspace.id,
        },
        workspace: actorWorkspace,
        idempotencyKey: `child-actor:start:${config.marker}`,
      },
    )
    actorRef = actor.ref
    const actorRun = await waitForTerminalRun(actor.run.id)
    assertEqual(actorRun.status, "succeeded", "Actor child call Run did not succeed")
    const actorOutput = await waitForActorOutput(actor.ref)
    assert(
      actorOutput.data !== null &&
        typeof actorOutput.data === "object" &&
        "kind" in actorOutput.data &&
        actorOutput.data.kind === "child-task-call-completed" &&
        "marker" in actorOutput.data &&
        actorOutput.data.marker === `${config.marker}-actor-call` &&
        "childRunId" in actorOutput.data &&
        typeof actorOutput.data.childRunId === "string",
      "Actor output did not contain its child Task result",
    )
    const actorChildRunId = actorOutput.data.childRunId
    const actorByID = client.actors.ref<typeof childTaskSmokeActor>(
      "child-task-smoke-actor",
      { id: actor.ref.id },
    )
    const actorByKey = client.actors.ref<typeof childTaskSmokeActor>(
      "child-task-smoke-actor",
      { key: `child-call:${config.marker}` },
    )
    const replayedInput = {
      marker: `${config.marker}-actor-continuation`,
      childWorkspaceId: targetWorkspace.id,
    }
    const sent = await actorByKey.input.send(
      replayedInput,
      { idempotencyKey: `child-actor:input:${config.marker}` },
      { signal: AbortSignal.timeout(30_000) },
    )
    const replayedSend = await actorByID.input.send(
      replayedInput,
      { idempotencyKey: `child-actor:input:${config.marker}` },
      { signal: AbortSignal.timeout(30_000) },
    )
    assertEqual(
      replayedSend.sequence,
      sent.sequence,
      "Actor input idempotency replay changed the sequence",
    )
    const actorOutputs = await waitForActorOutputs(actorByID, 2)
    const continuationOutput = actorOutputs.find((record) =>
      record.data !== null &&
      typeof record.data === "object" &&
      "marker" in record.data &&
      record.data.marker === `${config.marker}-actor-continuation`
    )
    assert(
      continuationOutput !== undefined,
      "Actor output omitted the continuation marker",
    )
    const actorContinuationRunId = continuationOutput.provenance.runId
    assert(
      actorContinuationRunId !== actor.run.id,
      "Actor continuation reused its initial Run",
    )
    const actorContinuationRun = await waitForTerminalRun(
      actorContinuationRunId,
    )
    assertEqual(
      actorContinuationRun.status,
      "succeeded",
      "Actor continuation Run did not succeed",
    )
    const outputSequences = actorOutputs.map((record) => record.sequence)
    assert(
      outputSequences.every((sequence, index) =>
        index === 0 || sequence > outputSequences[index - 1]!
      ),
      "Actor output sequences were not strictly ordered",
    )
    const streamedOutputs = []
    for await (
      const record of actorByID.output.read(
        { after: 0, limit: 1 },
        { signal: AbortSignal.timeout(30_000) },
      )
    ) {
      streamedOutputs.push(record)
    }
    assertEqual(
      streamedOutputs.length,
      actorOutputs.length,
      "Actor streaming output read did not paginate through all records",
    )

    await closeSmokeActor(actorByID)
    const retainedOutputs = await actorByID.output.list({ after: 0, limit: 10 })
    assertEqual(
      retainedOutputs.length,
      actorOutputs.length,
      "Actor close discarded durable output",
    )
    for (const workspace of workspaces.toReversed()) {
      await workspace.ref.delete({ idempotencyKey: workspace.deleteKey })
    }

    return {
      targetWorkspaceId: targetWorkspace.id,
      taskCallerWorkspaceId: taskCallerWorkspace.id,
      taskRunId: task.id,
      taskOutput,
      actorWorkspaceId: actorWorkspace.id,
      actorId: actor.ref.id,
      actorRunId: actor.run.id,
      actorContinuationRunId,
      actorChildRunId,
      actorOutputSequences: outputSequences,
    }
  } catch (error) {
    await cleanupChildTaskSmoke(actorRef, workspaces)
    throw error
  }
}

async function waitForTerminalRun(runId: string): Promise<RunSnapshot> {
  const deadline = Date.now() + 20 * 60 * 1_000
  for (;;) {
    const run = await client.runs.retrieve(runId)
    if (
      run.status === "succeeded" ||
      run.status === "failed" ||
      run.status === "cancelled" ||
      run.status === "expired" ||
      run.status === "system-failed"
    ) {
      return run
    }
    if (Date.now() >= deadline) {
      throw new Error(`timed out waiting for Actor Run ${runId}`)
    }
    await new Promise((resolve) => setTimeout(resolve, 500))
  }
}

async function waitForActorOutput(
  ref: ClientActorIdRef,
) {
  const deadline = Date.now() + 60_000
  for (;;) {
    const records = await ref.output.list({ after: 0, limit: 10 })
    if (records[0] !== undefined) return records[0]
    if (Date.now() >= deadline) {
      throw new Error(`timed out waiting for Actor output ${ref.id}`)
    }
    await new Promise((resolve) => setTimeout(resolve, 500))
  }
}

async function waitForActorOutputs(
  ref: ClientActorIdRef,
  count: number,
) {
  const deadline = Date.now() + 60_000
  for (;;) {
    const records = await ref.output.list({ after: 0, limit: 10 })
    if (records.length >= count) return records
    if (Date.now() >= deadline) {
      throw new Error(`timed out waiting for ${count} Actor outputs ${ref.id}`)
    }
    await new Promise((resolve) => setTimeout(resolve, 500))
  }
}

async function closeSmokeActor(ref: ClientActorIdRef): Promise<void> {
  await ref.close({
    idempotencyKey: `child-actor:close:${config.marker}`,
  })
  const status = await waitForActorClosed(ref)
  assertEqual(status.status, "closed", "Actor did not close after its smoke turn")
}

async function waitForActorClosed(ref: ClientActorIdRef): Promise<ActorStatus> {
  const deadline = Date.now() + 20 * 60 * 1_000
  for (;;) {
    const status = await ref.status()
    if (status.status === "closed") return status
    if (status.status === "cancelled" || status.status === "failed") {
      return status
    }
    if (Date.now() >= deadline) {
      throw new Error(`timed out closing Actor ${ref.id}`)
    }
    await new Promise((resolve) => setTimeout(resolve, 500))
  }
}

async function cleanupChildTaskSmoke(
  actorRef: ClientActorIdRef | undefined,
  workspaces: readonly Readonly<{
    ref: WorkspaceIdRef
    deleteKey: string
  }>[],
): Promise<void> {
  if (actorRef !== undefined) {
    try {
      await actorRef.close({
        idempotencyKey: `child-actor:close:${config.marker}`,
      })
      await waitForActorClosed(actorRef)
    } catch {
      // Preserve the original smoke failure while still attempting Workspace cleanup.
    }
  }
  await Promise.allSettled(
    workspaces.toReversed().map((workspace) =>
      workspace.ref.delete({ idempotencyKey: workspace.deleteKey })
    ),
  )
}

async function readTelemetry<T>(
  read: () => Promise<Readonly<{ items: readonly T[] }>>,
): Promise<Readonly<{ items: readonly T[] }>> {
  const deadline = Date.now() + 60_000
  for (;;) {
    try {
      return await read()
    } catch (error) {
      const code = errorCode(error)
      if (code !== "telemetry_lagging" || Date.now() >= deadline) throw error
      await new Promise((resolve) => setTimeout(resolve, 500))
    }
  }
}

function errorCode(error: unknown): string | undefined {
  return error !== null && typeof error === "object" && "code" in error &&
      typeof error.code === "string"
    ? error.code
    : undefined
}

function assertExec(result: WorkspaceExecResult, marker: string): void {
  const stdout = new TextDecoder().decode(result.stdout)
  const stderr = new TextDecoder().decode(result.stderr)
  assertEqual(result.exitCode, 7, "BasicExec did not preserve its nonzero exit code")
  assert(stdout.includes(`stdout:${marker}:stdin:${marker}`), "BasicExec stdout mismatch")
  assert(stderr.includes(`stderr:${marker}`), "BasicExec stderr mismatch")
  if (config.secretName !== undefined) {
    assert(stdout.includes("secret:present"), "BasicExec did not receive its Workspace Secret")
  }
}

function assertFile(entry: WorkspaceFileEntry, path: string, sizeBytes: number): void {
  assertEqual(entry.path, path, "Workspace file stat path mismatch")
  assertEqual(entry.kind, "file", "Workspace file stat kind mismatch")
  if (entry.kind === "file") {
    assertEqual(entry.sizeBytes, sizeBytes, "Workspace file stat size mismatch")
  }
}

function execEvidence(result: WorkspaceExecResult): ExecEvidence {
  return {
    exitCode: result.exitCode,
    stdout: new TextDecoder().decode(result.stdout),
    stderr: new TextDecoder().decode(result.stderr),
  }
}

function encodeExec(result: WorkspaceExecResult): string {
  return JSON.stringify({
    exitCode: result.exitCode,
    stdout: Buffer.from(result.stdout).toString("base64"),
    stderr: Buffer.from(result.stderr).toString("base64"),
  })
}

async function writeClientSmokeResult(
  evidence: Evidence | undefined,
): Promise<void> {
  const path = process.env["HELMR_CLIENT_SMOKE_RESULT_FILE"]
  if (path === undefined || path === "") return
  const checkIDs = [
    "workspace-lifecycle",
    "workspace-basic-exec",
    "idempotency-conflict",
    "task-runtime-telemetry",
    "child-task-call",
    "actor-continuation",
    "actor-output-pagination",
    "deployment-read",
    "schedule-read",
    "schedule-fire",
    "secret-lifecycle",
    "token-management",
    "external-token-fanout",
    "run-list-cancel",
  ] as const
  const status = evidence === undefined ? "failed" : "passed"
  const result = {
    schema: "helmrdotdev.client-smoke-result.v1",
    status,
    reason: evidence === undefined ? "client_smoke_failed" : null,
    checks: checkIDs.map((id) => ({ id, status })),
    objects: {
      run_ids: evidence === undefined
        ? []
        : [
            evidence.taskRunId,
            evidence.childTasks.taskRunId,
            evidence.childTasks.actorRunId,
            evidence.childTasks.actorContinuationRunId,
            evidence.childTasks.actorChildRunId,
            evidence.management.scheduledRunId,
            evidence.management.cancelledRunId,
            ...evidence.management.externalTokenRunIds,
          ],
      workspace_ids: evidence === undefined
        ? []
        : [
            evidence.workspaceId,
            evidence.childTasks.targetWorkspaceId,
            evidence.childTasks.taskCallerWorkspaceId,
            evidence.childTasks.actorWorkspaceId,
            ...evidence.management.externalTokenWorkspaceIds,
          ],
      deployment_ids: evidence === undefined
        ? []
        : [evidence.management.deploymentId],
      schedule_ids: evidence === undefined
        ? []
        : [evidence.management.scheduleId],
      token_ids: evidence === undefined
        ? []
        : [evidence.tokenId, evidence.management.completedTokenId],
      actor_ids: evidence === undefined ? [] : [evidence.childTasks.actorId],
    },
    observations: evidence === undefined
      ? {}
      : {
          actor_output_count: evidence.childTasks.actorOutputSequences.length,
          secret_delivery: evidence.secretDelivery,
        },
  }
  await mkdir(dirname(path), { recursive: true })
  await writeFile(`${path}.tmp`, `${JSON.stringify(result)}\n`, { mode: 0o600 })
  await rename(`${path}.tmp`, path)
}
