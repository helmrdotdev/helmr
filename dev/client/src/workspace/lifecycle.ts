import { HelmrClient, type TaskStartResult } from "../../../../sdk/typescript/src/index"
import { assert, assertEqual } from "../assert"
import { readConfig, requestScope } from "../config"
import { currentDeployment, waitForRunningMaterialization } from "./common"

interface SmokeEvidence {
  readonly marker: string
  readonly deploymentId: string
  readonly deploymentSandboxId: string
  readonly directWorkspaceId: string
  readonly directMaterializationId: string
  readonly firstSessionId: string
  readonly firstRunId: string
  readonly secondSessionId: string
  readonly secondRunId: string
  readonly versions: {
    readonly directInitial: string
    readonly afterFirst: string
    readonly afterSecond: string
  }
}

interface RuntimeSmokePayload {
  readonly scenario: string
  readonly marker: string
  readonly expectedEnvironment: "unknown"
  readonly expectedWorkspaceMarker?: string
}

interface RuntimeSmokeStartOptions {
  readonly workspaceId?: string
  readonly priority: number
  readonly metadata: Record<string, unknown>
  readonly tags: readonly string[]
  readonly externalId: string
}

const config = readConfig()
const client = new HelmrClient({ url: config.apiUrl, apiKey: config.apiKey })
const scope = requestScope(config)
const startRuntimeSmoke = client.tasks.start as unknown as (
  id: "runtime-smoke",
  payload: RuntimeSmokePayload,
  opts: RuntimeSmokeStartOptions,
) => Promise<TaskStartResult<unknown>>

const evidence = await runWorkspaceLifecycleSmoke()
console.log(JSON.stringify(evidence, null, 2))

async function runWorkspaceLifecycleSmoke(): Promise<SmokeEvidence> {
  const deployment = await currentDeployment(config)
  assert(deployment.tasks.some((task) => task.task_id === config.taskId), `current deployment does not include ${config.taskId}`)

  const directWorkspace = await client.workspaces.create({
    ...scope,
    sandboxId: config.sandboxId,
    deploymentId: deployment.id,
    externalId: `${config.marker}-direct`,
    metadata: { smoke: "workspace-lifecycle", marker: config.marker },
    tags: ["smoke", "workspace-lifecycle"],
    idempotencyKey: `workspace-lifecycle:${config.marker}`,
    idempotencyKeyTTL: "24h",
  })
  assert(directWorkspace.currentVersionId !== null, "direct workspace did not get initial currentVersionId")

  const createdAgain = await client.workspaces.create({
    ...scope,
    sandboxId: config.sandboxId,
    deploymentId: deployment.id,
    externalId: `${config.marker}-direct`,
    metadata: { smoke: "workspace-lifecycle", marker: config.marker },
    tags: ["smoke", "workspace-lifecycle"],
    idempotencyKey: `workspace-lifecycle:${config.marker}`,
    idempotencyKeyTTL: "24h",
  })
  assertEqual(createdAgain.id, directWorkspace.id, "workspace create idempotency returned a different workspace")

  const handle = client.workspaces.open(directWorkspace.id)
  assertEqual(handle.id, directWorkspace.id, "workspace open returned an unexpected handle id")
  const retrievedFromHandle = await handle.retrieve(scope)
  assertEqual(retrievedFromHandle.id, directWorkspace.id, "workspace handle retrieve returned a different workspace")

  const patched = await handle.update({
    ...scope,
    metadata: { smoke: "workspace-lifecycle", marker: config.marker, patched: true },
    tags: ["smoke", "workspace-lifecycle", "patched"],
  })
  assertEqual(patched.id, directWorkspace.id, "workspace update returned a different workspace")
  assert(patched.tags.includes("patched"), "workspace update did not persist tags")

  const materialized = await handle.materialize(scope)
  const connected = await handle.connect(scope)
  assertEqual(connected.id, materialized.id, "connect did not ensure the pending materialization")
  const running = await waitForRunningMaterialization(client, handle.id, scope)
  assertEqual(running.id, materialized.id, "running materialization id changed")

  const sessionsBeforeAttach = await sessionsForTask(config.taskId)
  assert(!sessionsBeforeAttach.some((session) => session.workspaceId === directWorkspace.id), "direct materialization created a task session")

  const first = await startAndWaitRuntime(`${config.marker}-first`, directWorkspace.id)
  const firstSession = await client.sessions.retrieve(first.sessionId)
  assertEqual(firstSession.workspaceId, directWorkspace.id, "first task did not attach to direct workspace")
  const afterFirst = await client.workspaces.retrieve(directWorkspace.id, scope)
  assert(afterFirst.currentVersionId !== null, "workspace lost currentVersionId after first run")
  assert(afterFirst.currentVersionId !== directWorkspace.currentVersionId, "workspace currentVersionId did not advance after first run")

  const second = await startAndWaitRuntime(`${config.marker}-second`, directWorkspace.id, `${config.marker}-first`)
  const secondSession = await client.sessions.retrieve(second.sessionId)
  assertEqual(secondSession.workspaceId, directWorkspace.id, "second task did not attach to direct workspace")
  const afterSecond = await client.workspaces.retrieve(directWorkspace.id, scope)
  assert(afterSecond.currentVersionId !== null, "workspace lost currentVersionId after second run")
  assert(afterSecond.currentVersionId !== afterFirst.currentVersionId, "workspace currentVersionId did not advance after second run")

  return {
    marker: config.marker,
    deploymentId: deployment.id,
    deploymentSandboxId: directWorkspace.deploymentSandboxId,
    directWorkspaceId: directWorkspace.id,
    directMaterializationId: running.id,
    firstSessionId: first.sessionId,
    firstRunId: first.runId,
    secondSessionId: second.sessionId,
    secondRunId: second.runId,
    versions: {
      directInitial: directWorkspace.currentVersionId,
      afterFirst: afterFirst.currentVersionId,
      afterSecond: afterSecond.currentVersionId,
    },
  }
}

async function startAndWaitRuntime(marker: string, workspaceId?: string, expectedWorkspaceMarker?: string): Promise<{ sessionId: string; runId: string }> {
  const started = await startRuntimeSmoke(
    config.taskId,
    {
      scenario: "workspace-lifecycle-client-smoke",
      marker,
      expectedEnvironment: "unknown",
      ...(expectedWorkspaceMarker === undefined ? {} : { expectedWorkspaceMarker }),
    },
    {
      ...(workspaceId === undefined ? {} : { workspaceId }),
      priority: 25,
      metadata: { smoke: "workspace-lifecycle", marker },
      tags: ["smoke", "workspace-lifecycle"],
      externalId: marker,
    },
  )
  const run = await client.runs.wait(started.run, { timeoutMs: 900_000 })
  assertEqual(run.status, "succeeded", `run ${started.run.id} did not succeed`)
  return {
    sessionId: started.session.id,
    runId: started.run.id,
  }
}

async function sessionsForTask(taskId: string) {
  return await client.sessions.list({ ...scope, taskId, limit: 100 })
}
