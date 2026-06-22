import { HelmrClient, type WorkspacePty } from "../../../../sdk/typescript/src/index"
import { assert, assertEqual } from "../assert"
import { readConfig, requestScope } from "../config"
import { byteLength, chunksText, collectStream, currentDeployment, delay, waitForCollectedText, waitForRunningMaterialization, waitForStreamDone } from "./common"

interface PtySmokeEvidence {
  readonly marker: string
  readonly deploymentId: string
  readonly workspaceId: string
  readonly materializationId: string
  readonly ptyId: string
  readonly processId: string
  readonly output: string
  readonly inputCursor: number
  readonly outputCursor: number
  readonly finalState: WorkspacePty["state"]
  readonly elapsedMs: {
    readonly materialize: number
    readonly createToOpen: number
    readonly inputToOutput: number
    readonly close: number
  }
}

const config = readConfig()
const client = new HelmrClient({ url: config.apiUrl, apiKey: config.apiKey })
const scope = requestScope(config)

const evidence = await runWorkspacePtySmoke()
console.log(JSON.stringify(evidence, null, 2))

async function runWorkspacePtySmoke(): Promise<PtySmokeEvidence> {
  const deployment = await currentDeployment(config)
  assert(deployment.tasks.some((task) => task.task_id === config.taskId), `current deployment does not include ${config.taskId}`)

  const workspace =
    config.workspaceId === undefined
      ? await client.workspaces.create({
          ...scope,
          sandboxId: config.sandboxId,
          deploymentId: deployment.id,
          externalId: `${config.marker}-pty`,
          metadata: { smoke: "workspace-pty", marker: config.marker },
          tags: ["smoke", "workspace-pty"],
          idempotencyKey: `workspace-pty:${config.marker}`,
          idempotencyKeyTTL: "24h",
        })
      : await client.workspaces.retrieve(config.workspaceId, scope)
  const handle = client.workspaces.open(workspace.id)
  const ownsWorkspace = config.workspaceId === undefined
  try {
    const sessionsBefore = await client.sessions.list({ ...scope, taskId: config.taskId, limit: 100 })

    const materializeStarted = Date.now()
    const requested = await handle.materialize(scope)
    const running = await waitForRunningMaterialization(client, workspace.id, scope, requested.id)
    const materializedAt = Date.now()

    const createStarted = Date.now()
    const pty = await handle.pty.create({
      ...scope,
      cwd: "/workspace",
      cols: 80,
      rows: 24,
      idempotencyKey: `workspace-pty-open:${config.marker}`,
    })
    const streamAbort = new AbortController()
    const outputCollector = collectStream(pty.output.stream({ ...scope, signal: streamAbort.signal, fromCursor: 0, follow: true }))
    const opened = await waitForPtyState(() => pty.retrieve(scope), "open")
    const openedAt = Date.now()
    assert(opened.processId !== null && opened.processId !== "", "pty process id was not persisted")

    const firstInput = `printf 'PTY_MARKER:${config.marker}\\n'\n`
    const inputStarted = Date.now()
    const inputChunk = await pty.input(firstInput, { ...scope, offset: 0 })
    assertEqual(inputChunk.offsetStart, 0, "pty input offset_start mismatch")
    assertEqual(inputChunk.offsetEnd, byteLength(firstInput), "pty input offset_end mismatch")
    await waitForCollectedText(outputCollector, `PTY_MARKER:${config.marker}`, "pty output")
    const inputOutputAt = Date.now()

    const resizing = await pty.resize(100, 40, scope)
    assert(resizing.state === "resizing" || resizing.state === "open", `unexpected pty resize state ${resizing.state}`)
    const resized = await waitForPtySize(() => pty.retrieve(scope), 100, 40)
    assertEqual(resized.cols, 100, "pty resize cols mismatch")
    assertEqual(resized.rows, 40, "pty resize rows mismatch")

    const closeStarted = Date.now()
    const closing = await pty.close(scope)
    assert(closing.state === "closing" || closing.state === "closed", `unexpected pty close state ${closing.state}`)
    const closed = await waitForPtyTerminal(() => pty.retrieve(scope))
    const closedAt = Date.now()
    await waitForStreamDone(outputCollector, "pty output")
    streamAbort.abort()
    assertEqual(closed.state, "closed", "pty did not close cleanly")

    const listed = await handle.pty.list({ ...scope, limit: 20 })
    assert(listed.some((row) => row.id === pty.id), "created pty was not listed")
    const retrieved = await handle.pty.retrieve(pty.id).retrieve(scope)
    assertEqual(retrieved.id, pty.id, "retrieved pty id mismatch")

    const sessionsAfter = await client.sessions.list({ ...scope, taskId: config.taskId, limit: 100 })
    const createdSessions = sessionsAfter.filter((after) => !sessionsBefore.some((before) => before.id === after.id))
    assert(!createdSessions.some((session) => session.workspaceId === workspace.id), "direct workspace pty created a task session")

    return {
      marker: config.marker,
      deploymentId: deployment.id,
      workspaceId: workspace.id,
      materializationId: running.id,
      ptyId: pty.id,
      processId: opened.processId,
      output: chunksText(outputCollector.chunks),
      inputCursor: closed.inputCursor,
      outputCursor: closed.outputCursor,
      finalState: closed.state,
      elapsedMs: {
        materialize: materializedAt - materializeStarted,
        createToOpen: openedAt - createStarted,
        inputToOutput: inputOutputAt - inputStarted,
        close: closedAt - closeStarted,
      },
    }
  } finally {
    if (ownsWorkspace) {
      await handle.stop({ ...scope, idempotencyKey: `workspace-pty-cleanup:${config.marker}` }).catch((error: unknown) => {
        console.error(`workspace pty cleanup failed: ${String(error)}`)
      })
    }
  }
}

async function waitForPtyState(read: () => Promise<WorkspacePty>, state: WorkspacePty["state"]): Promise<WorkspacePty> {
  for (let attempt = 0; attempt < 240; attempt += 1) {
    const pty = await read()
    if (pty.state === state) {
      return pty
    }
    assert(pty.state === "creating" || pty.state === "resizing" || pty.state === "closing", `unexpected pty state ${pty.state}`)
    await delay(500)
  }
  throw new Error(`pty did not reach ${state}`)
}

async function waitForPtySize(read: () => Promise<WorkspacePty>, cols: number, rows: number): Promise<WorkspacePty> {
  for (let attempt = 0; attempt < 240; attempt += 1) {
    const pty = await read()
    if (pty.state === "open" && pty.cols === cols && pty.rows === rows) {
      return pty
    }
    assert(pty.state === "open" || pty.state === "resizing", `unexpected pty resize state ${pty.state}`)
    await delay(500)
  }
  throw new Error(`pty did not resize to ${cols}x${rows}`)
}

async function waitForPtyTerminal(read: () => Promise<WorkspacePty>): Promise<WorkspacePty> {
  for (let attempt = 0; attempt < 240; attempt += 1) {
    const pty = await read()
    if (pty.state === "closed" || pty.state === "lost" || pty.state === "failed") {
      return pty
    }
    assert(pty.state === "closing" || pty.state === "open" || pty.state === "resizing", `unexpected pty close state ${pty.state}`)
    await delay(500)
  }
  throw new Error("pty did not reach a terminal state")
}
