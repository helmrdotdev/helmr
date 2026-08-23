import { createHash } from "node:crypto"
import { mkdir, rename, writeFile } from "node:fs/promises"
import { dirname } from "node:path"
import {
  HelmrClient,
  type Session,
  type SessionRef,
  type JsonValue,
  type RunEventRecord,
  type RunLogRecord,
  type Run,
  type WorkspaceExecResult,
  type WorkspaceFileEntry,
  type WorkspaceRef,
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
  readonly sessionId: string
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
    idempotencyKey: `workspace:create:${config.marker}`,
  } as const
  const created = await client.sandboxes.createWorkspace(
    "helmr-runtime-smoke",
    workspaceCreateOptions,
  )
  cleanupPrimaryWorkspace = () =>
    created.delete({ idempotencyKey: `workspace:delete:${config.marker}` })
  const replayedCreate = await client.sandboxes.createWorkspace(
    "helmr-runtime-smoke",
    workspaceCreateOptions,
  )
  assertEqual(replayedCreate.id, created.id, "workspace create replay changed the ID")
  let unexpectedConflictWorkspace: WorkspaceRef | undefined
  try {
    unexpectedConflictWorkspace = await client.sandboxes.createWorkspace(
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

  const matches = await client.workspaces.list({ key: workspaceKey })
  assertEqual(matches.items.length, 1, "workspace key lookup was not exact")
  const workspace = matches.items[0]!
  const byKey = client.workspaces.ref(workspace.id)
  assertEqual(workspace.id, created.id, "workspace key resolved to a different Workspace")
  assertEqual(workspace.sandboxId, "helmr-runtime-smoke", "workspace Sandbox ID mismatch")

  const markerDirectory = "sandbox-smoke/nested"
  const markerPath = `${markerDirectory}/marker.txt`
  const stdin = `stdin:${config.marker}\n`
  const execOptions = {
    command: [
      "sh",
      "-ceu",
      [
        `mkdir -p ${markerDirectory}`,
        "IFS= read -r line",
        `printf 'marker=%s\\nstdin=%s\\n' "$SMOKE_MARKER" "$line" > ${markerPath}`,
        "printf 'stdout:%s:%s\\n' \"$SMOKE_MARKER\" \"$line\"",
        "printf 'stderr:%s\\n' \"$SMOKE_MARKER\" >&2",
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
  const listing = await byKey.files.list(markerDirectory, { limit: 100 })
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
  const taskOutput = await client.runs.wait(started, {
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
  assertEqual(canceled.status, "cancelled", "external Token was not cancelled")

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
    deletedWorkspaceId: deleted.workspaceId,
  }
}

async function runChildTaskSmoke(): Promise<ChildTaskEvidence> {
  const workspaces: Array<Readonly<{
    ref: WorkspaceRef
    deleteKey: string
  }>> = []
  let actorRef: SessionRef | undefined
  try {
    const targetWorkspace = await client.sandboxes.createWorkspace(
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
    const taskCallerWorkspace = await client.sandboxes.createWorkspace(
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
    const taskOutput = await client.runs.wait(
      task,
      { signal: AbortSignal.timeout(20 * 60 * 1_000) },
    ).unwrap()
    assert(
      taskOutput.mode === "call-success" &&
        taskOutput.marker === `${config.marker}-task-call`,
      "Task child call output did not match the requested smoke marker",
    )

    const actorWorkspace = await client.sandboxes.createWorkspace(
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
    const actor = await client.actors.start(
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
    actorRef = actor.session
    const actorRun = await waitForTerminalRun(actor.run.id)
    assertEqual(actorRun.status, "succeeded", "Actor child call Run did not succeed")
    const actorOutput = await waitForActorOutput(actor.session)
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
    const actorByID = client.sessions.ref(actor.session.id)
    const sessionMatches = await client.sessions.list({
      actorId: "child-task-smoke-actor",
      key: `child-call:${config.marker}`,
    })
    assertEqual(sessionMatches.items.length, 1, "Session key lookup was not exact")
    const actorByKey = client.sessions.ref(sessionMatches.items[0]!.id)
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
    let after = 0
    for (;;) {
      const page = await actorByID.output.list(
        { after, limit: 1 },
        { signal: AbortSignal.timeout(30_000) },
      )
      streamedOutputs.push(...page.records)
      if (!page.hasMore) break
      after = page.nextAfter
    }
    assertEqual(
      streamedOutputs.length,
      actorOutputs.length,
      "Actor streaming output read did not paginate through all records",
    )

    await closeSmokeActor(actorByID)
    const retainedOutputs = await actorByID.output.list({ after: 0, limit: 10 })
    assertEqual(
      retainedOutputs.records.length,
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
      sessionId: actor.session.id,
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

async function waitForTerminalRun(runId: string): Promise<Run> {
  const deadline = Date.now() + 20 * 60 * 1_000
  for (;;) {
    const run = await client.runs.retrieve(runId)
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

async function waitForActorOutput(
  ref: SessionRef,
) {
  const deadline = Date.now() + 5 * 60_000
  for (;;) {
    const page = await ref.output.list({ after: 0, limit: 10 })
    if (page.records[0] !== undefined) return page.records[0]
    if (Date.now() >= deadline) {
      throw new Error(`timed out waiting for Actor output ${ref.id}`)
    }
    await new Promise((resolve) => setTimeout(resolve, 500))
  }
}

async function waitForActorOutputs(
  ref: SessionRef,
  count: number,
) {
  const deadline = Date.now() + 5 * 60_000
  for (;;) {
    const page = await ref.output.list({ after: 0, limit: 10 })
    if (page.records.length >= count) return page.records
    if (Date.now() >= deadline) {
      throw new Error(`timed out waiting for ${count} Actor outputs ${ref.id}`)
    }
    await new Promise((resolve) => setTimeout(resolve, 500))
  }
}

async function closeSmokeActor(ref: SessionRef): Promise<void> {
  await ref.close({
    idempotencyKey: `child-actor:close:${config.marker}`,
  })
  const status = await waitForActorClosed(ref)
  assertEqual(status.status, "closed", "Actor did not close after its smoke turn")
}

async function waitForActorClosed(ref: SessionRef): Promise<Session> {
  const deadline = Date.now() + 20 * 60 * 1_000
  for (;;) {
    const status = await client.sessions.retrieve(ref.id)
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
  actorRef: SessionRef | undefined,
  workspaces: readonly Readonly<{
    ref: WorkspaceRef
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
  const deadline = Date.now() + 2 * 60_000
  for (;;) {
    try {
      return await read()
    } catch (error) {
      const code = errorCode(error)
      if (
        (code !== "telemetry_lagging" && code !== "telemetry_unavailable") ||
        Date.now() >= deadline
      ) throw error
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
      schedule_ids: [],
      token_ids: evidence === undefined
        ? []
        : [evidence.tokenId, evidence.management.completedTokenId],
      session_ids: evidence === undefined ? [] : [evidence.childTasks.sessionId],
    },
    observations: evidence === undefined
      ? {}
      : {
          actor_output_count: evidence.childTasks.actorOutputSequences.length,
        },
  }
  await mkdir(dirname(path), { recursive: true })
  await writeFile(`${path}.tmp`, `${JSON.stringify(result)}\n`, { mode: 0o600 })
  await rename(`${path}.tmp`, path)
}
