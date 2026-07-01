import { HelmrClient, type WorkspaceExec } from "../../../../sdk/typescript/src/index"
import { assert, assertEqual } from "../assert"
import { readConfig, requestScope } from "../config"
import { byteLength, chunksText, collectStream, currentDeployment, waitForCollectedText, waitForRunningWorkspaceMount, waitForStreamDone } from "./common"

interface ExecSmokeEvidence {
  readonly marker: string
  readonly deploymentId: string
  readonly workspaceId: string
  readonly materializationId: string
  readonly execId: string
  readonly processId: string
  readonly exitCode: number
  readonly stdout: string
  readonly stderr: string
  readonly stdoutChunks: number
  readonly stderrChunks: number
  readonly stdinCursor: number
  readonly elapsedMs: {
    readonly materialize: number
    readonly startToFirstStdout: number
    readonly startToExit: number
  }
}

const config = readConfig()
const client = new HelmrClient({ url: config.apiUrl, apiKey: config.apiKey })
const scope = requestScope(config)

const evidence = await runWorkspaceExecSmoke()
console.log(JSON.stringify(evidence, null, 2))

async function runWorkspaceExecSmoke(): Promise<ExecSmokeEvidence> {
  const deployment = await currentDeployment(config)
  assert(deployment.tasks.some((task) => task.task_id === config.taskId), `current deployment does not include ${config.taskId}`)

  const workspace =
    config.workspaceId === undefined
      ? await client.workspaces.create({
          ...scope,
          sandboxId: config.sandboxId,
          deploymentId: deployment.id,
          externalId: `${config.marker}-exec`,
          metadata: { smoke: "workspace-exec", marker: config.marker },
          tags: ["smoke", "workspace-exec"],
          idempotencyKey: `workspace-exec:${config.marker}`,
          idempotencyKeyTTL: "24h",
        })
      : await client.workspaces.retrieve(config.workspaceId, scope)
  const handle = client.workspaces.open(workspace.id)
  const ownsWorkspace = config.workspaceId === undefined
  try {
    const sessionsBefore = await client.sessions.list({ ...scope, taskId: config.taskId, limit: 100 })

    const materializeStarted = Date.now()
    const requested = await handle.materialize(scope)
    const running = await waitForRunningWorkspaceMount(client, workspace.id, scope, requested.id)
    const materializedAt = Date.now()

    const command = [
      "bash",
      "-lc",
      [
        "set -euo pipefail",
        "printf 'EXEC_STDOUT_START:%s\\n' \"$SMOKE_MARKER\"",
        "printf 'EXEC_STDERR_START:%s\\n' \"$SMOKE_MARKER\" >&2",
        "IFS= read -r line",
        "printf 'EXEC_STDIN:%s\\n' \"$line\"",
        "while IFS= read -r _extra; do :; done",
        "printf 'EXEC_STDERR_DONE:%s\\n' \"$line\" >&2",
        "exit 7",
      ].join("; "),
    ]
    const startedAt = Date.now()
    const exec = await handle.exec(command, {
      ...scope,
      cwd: "/workspace",
      env: { SMOKE_MARKER: config.marker },
      idempotencyKey: `workspace-exec-command:${config.marker}`,
    })
    const streamAbort = new AbortController()
    const streamScope = { ...scope, signal: streamAbort.signal }
    const stdoutCollector = collectStream(exec.stdout.stream({ ...streamScope, fromCursor: 0, follow: true }))
    const stderrCollector = collectStream(exec.stderr.stream({ ...streamScope, fromCursor: 0, follow: true }))

    const firstStdout = await waitForCollectedText(stdoutCollector, `EXEC_STDOUT_START:${config.marker}`, "exec stdout")
    const firstStdoutAt = Date.now()
    assert(firstStdout.includes(`EXEC_STDOUT_START:${config.marker}`), "exec stdout start marker was not persisted")

    const stdinPayload = `client-input:${config.marker}\n`
    const stdinChunk = await exec.stdin.write(stdinPayload, { ...scope, offset: 0 })
    assertEqual(stdinChunk.offsetStart, 0, "stdin offset_start mismatch")
    assertEqual(stdinChunk.offsetEnd, byteLength(stdinPayload), "stdin offset_end mismatch")
    const closed = await exec.stdin.close(scope)
    assert(closed.stdinClosedAt !== null, "stdin close did not persist stdin_closed_at")

    const terminal = await exec.wait({ ...scope, pollIntervalMs: 500 })
    const exitedAt = Date.now()
    await Promise.all([waitForStreamDone(stdoutCollector, "exec stdout"), waitForStreamDone(stderrCollector, "exec stderr")])
    streamAbort.abort()
    assertEqual(terminal.state, "exited", "exec did not reach exited state")
    assertEqual(terminal.exitCode, 7, "exec exit code mismatch")
    assert(terminal.processId !== null && terminal.processId !== "", "exec process id was not persisted")
    const exitCode = terminal.exitCode
    assert(exitCode !== null, "exec exit code was not persisted")

    const stdout = chunksText(stdoutCollector.chunks)
    const stderr = chunksText(stderrCollector.chunks)
    assert(stdout.includes(`EXEC_STDIN:client-input:${config.marker}`), "exec stdin was not observed in stdout")
    assert(stderr.includes(`EXEC_STDERR_START:${config.marker}`), "exec stderr start marker was not persisted")
    assert(stderr.includes(`EXEC_STDERR_DONE:client-input:${config.marker}`), "exec stderr done marker was not persisted")

    const listed = await handle.execs.list({ ...scope, limit: 20 })
    assert(listed.some((row) => row.id === exec.id), "created exec was not listed")
    const retrieved = await handle.execs.retrieve(exec.id).retrieve(scope)
    assertExecMatchesTerminal(retrieved, terminal)

    const sessionsAfter = await client.sessions.list({ ...scope, taskId: config.taskId, limit: 100 })
    const createdSessions = sessionsAfter.filter((after) => !sessionsBefore.some((before) => before.id === after.id))
    assert(!createdSessions.some((session) => session.workspaceId === workspace.id), "direct workspace exec created a session")

    const stdoutChunks = await exec.stdout.list({ ...scope, cursor: 0, limit: 100 })
    const stderrChunks = await exec.stderr.list({ ...scope, cursor: 0, limit: 100 })

    return {
      marker: config.marker,
      deploymentId: deployment.id,
      workspaceId: workspace.id,
      materializationId: running.id,
      execId: exec.id,
      processId: terminal.processId,
      exitCode,
      stdout,
      stderr,
      stdoutChunks: stdoutChunks.length,
      stderrChunks: stderrChunks.length,
      stdinCursor: terminal.stdinCursor,
      elapsedMs: {
        materialize: materializedAt - materializeStarted,
        startToFirstStdout: firstStdoutAt - startedAt,
        startToExit: exitedAt - startedAt,
      },
    }
  } finally {
    if (ownsWorkspace) {
      await handle.stop({ ...scope, idempotencyKey: `workspace-exec-cleanup:${config.marker}` }).catch((error: unknown) => {
        console.error(`workspace exec cleanup failed: ${String(error)}`)
      })
    }
  }
}

function assertExecMatchesTerminal(actual: WorkspaceExec, terminal: WorkspaceExec): void {
  assertEqual(actual.id, terminal.id, "retrieved exec id mismatch")
  assertEqual(actual.state, terminal.state, "retrieved exec state mismatch")
  assertEqual(actual.exitCode, terminal.exitCode, "retrieved exec exit code mismatch")
}
