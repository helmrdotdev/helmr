import { HelmrClient, type WorkspaceStopResult } from "../../../../sdk/typescript/src/index"
import { assert, assertEqual } from "../assert"
import { readConfig, requestScope } from "../config"
import { chunksText, currentDeployment, delay, waitForRunningMaterialization } from "./common"

interface StopSmokeEvidence {
  readonly marker: string
  readonly deploymentId: string
  readonly workspaceId: string
  readonly firstMaterializationId: string
  readonly secondMaterializationId: string
  readonly writeExecId: string
  readonly readExecId: string
  readonly versionBeforeStop: string | null
  readonly versionAfterStop: string | null
  readonly stopStates: readonly string[]
  readonly finalStopStates: readonly string[]
  readonly persistedText: string
  readonly elapsedMs: {
    readonly firstMaterialize: number
    readonly stop: number
    readonly rematerialize: number
    readonly finalStop: number
  }
}

const config = readConfig()
const client = new HelmrClient({ url: config.apiUrl, apiKey: config.apiKey })
const scope = requestScope(config)

const evidence = await runWorkspaceStopSmoke()
console.log(JSON.stringify(evidence, null, 2))

async function runWorkspaceStopSmoke(): Promise<StopSmokeEvidence> {
  const deployment = await currentDeployment(config)
  assert(deployment.tasks.some((task) => task.task_id === config.taskId), `current deployment does not include ${config.taskId}`)

  const workspace =
    config.workspaceId === undefined
      ? await client.workspaces.create({
          ...scope,
          sandboxId: config.sandboxId,
          deploymentId: deployment.id,
          externalId: `${config.marker}-stop`,
          metadata: { smoke: "workspace-stop", marker: config.marker },
          tags: ["smoke", "workspace-stop"],
          idempotencyKey: `workspace-stop-create:${config.marker}`,
          idempotencyKeyTTL: "24h",
        })
      : await client.workspaces.retrieve(config.workspaceId, scope)
  const handle = client.workspaces.open(workspace.id)
  const sessionsBefore = await client.sessions.list({ ...scope, taskId: config.taskId, limit: 100 })

  const firstMaterializeStarted = Date.now()
  const requested = await handle.materialize(scope)
  const firstRunning = await waitForRunningMaterialization(client, workspace.id, scope, requested.id)
  const firstMaterializedAt = Date.now()

  const markerPath = `/workspace/stop-smoke-${config.marker}.txt`
  const expectedText = `STOP_MARKER:${config.marker}`
  const writeExec = await handle.exec(
    ["bash", "-lc", `set -euo pipefail; printf '%s\\n' "${expectedText}" > ${shellQuote(markerPath)}; cat ${shellQuote(markerPath)}`],
    {
      ...scope,
      cwd: "/workspace",
      idempotencyKey: `workspace-stop-write:${config.marker}`,
    },
  )
  const writeTerminal = await writeExec.wait({ ...scope, pollIntervalMs: 500 })
  assertEqual(writeTerminal.state, "exited", "write exec did not exit")
  assertEqual(writeTerminal.exitCode, 0, "write exec exit code mismatch")
  const writeStdout = chunksText(await writeExec.stdout.list({ ...scope, cursor: 0, limit: 100 }))
  assert(writeStdout.includes(expectedText), "write exec did not read back marker before stop")

  const beforeStop = await client.workspaces.retrieve(workspace.id, scope)
  const stopStarted = Date.now()
  const stopStates: string[] = []
  const stopped = await waitForStoppedWorkspace(handle, stopStates, `workspace-stop:${config.marker}`)
  const stoppedAt = Date.now()
  assertEqual(stopped.state, "no_active_materialization", "workspace stop did not become idempotent no-active success")

  const afterStop = await client.workspaces.retrieve(workspace.id, scope)
  assert(afterStop.currentVersionId !== null, "workspace stop lost current version")
  assert(afterStop.currentVersionId !== beforeStop.currentVersionId, "dirty workspace stop did not promote a system workspace version")

  const secondMaterializeStarted = Date.now()
  const secondRequested = await handle.connect(scope)
  const secondRunning = await waitForRunningMaterialization(client, workspace.id, scope, secondRequested.id)
  const secondMaterializedAt = Date.now()
  assert(secondRunning.id !== firstRunning.id, "connect after stop reused the stopped materialization")

  const readExec = await handle.exec(["bash", "-lc", `set -euo pipefail; cat ${shellQuote(markerPath)}`], {
    ...scope,
    cwd: "/workspace",
    idempotencyKey: `workspace-stop-read:${config.marker}`,
  })
  const readTerminal = await readExec.wait({ ...scope, pollIntervalMs: 500 })
  assertEqual(readTerminal.state, "exited", "read exec did not exit")
  assertEqual(readTerminal.exitCode, 0, "read exec exit code mismatch")
  const persistedText = chunksText(await readExec.stdout.list({ ...scope, cursor: 0, limit: 100 }))
  assert(persistedText.includes(expectedText), "rematerialized workspace did not contain stopped capture marker")

  const finalStopStarted = Date.now()
  const finalStopStates: string[] = []
  await waitForStoppedWorkspace(handle, finalStopStates, `workspace-stop-final:${config.marker}`)
  const finalStoppedAt = Date.now()

  const sessionsAfter = await client.sessions.list({ ...scope, taskId: config.taskId, limit: 100 })
  const createdSessions = sessionsAfter.filter((after) => !sessionsBefore.some((before) => before.id === after.id))
  assert(!createdSessions.some((session) => session.workspaceId === workspace.id), "direct workspace stop smoke created a task session")

  return {
    marker: config.marker,
    deploymentId: deployment.id,
    workspaceId: workspace.id,
    firstMaterializationId: firstRunning.id,
    secondMaterializationId: secondRunning.id,
    writeExecId: writeExec.id,
    readExecId: readExec.id,
    versionBeforeStop: beforeStop.currentVersionId,
    versionAfterStop: afterStop.currentVersionId,
    stopStates,
    finalStopStates,
    persistedText,
    elapsedMs: {
      firstMaterialize: firstMaterializedAt - firstMaterializeStarted,
      stop: stoppedAt - stopStarted,
      rematerialize: secondMaterializedAt - secondMaterializeStarted,
      finalStop: finalStoppedAt - finalStopStarted,
    },
  }
}

async function waitForStoppedWorkspace(
  handle: { stop: (opts: Record<string, unknown>) => Promise<WorkspaceStopResult> },
  states: string[],
  idempotencyKey: string,
): Promise<WorkspaceStopResult> {
  const first = await handle.stop({
    ...scope,
    idempotencyKey,
    idempotencyKeyTTL: "24h",
  })
  states.push(first.state)
  if (first.state === "no_active_materialization") {
    return first
  }
  assert(
    first.state === "capturing" || first.state === "stopping" || first.state === "stopped" || first.state === "requested",
    `unexpected stop state ${first.state}`,
  )

  for (let attempt = 0; attempt < 180; attempt += 1) {
    await delay(2_000)
    const result = await handle.stop(scope)
    states.push(result.state)
    if (result.state === "no_active_materialization") {
      return result
    }
    assert(
      result.state === "capturing" || result.state === "stopping" || result.state === "stopped" || result.state === "requested",
      `unexpected stop state ${result.state}`,
    )
  }
  throw new Error("workspace stop did not reach no-active success")
}

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`
}
