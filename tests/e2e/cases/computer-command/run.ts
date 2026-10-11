import { verify, assert, assertEqual, errorCode, completedResult, waitOutput, deleteComputer } from "../../support/context"
import type { ClientComputerRef, CommandRef } from "@helmr/sdk"
await verify("computer-command", async ({ client, marker, objects, cleanup, startAgent }) => {
  const computerKey = `runtime-smoke-${marker}`
  const computerCreateOptions = {
    key: computerKey,
    idempotencyKey: `computer:create:${marker}`,
  } as const
  const created = await client.computerDefinitions.createComputer(
    "helmr-runtime-smoke",
    computerCreateOptions,
  )
  objects.computer_ids.push(created.id)
  cleanup(() => deleteComputer(created, `computer:delete:${marker}`))
  const replayedCreate = await client.computerDefinitions.createComputer(
    "helmr-runtime-smoke",
    computerCreateOptions,
  )
  assertEqual(replayedCreate.id, created.id, "computer create replay changed the ID")
  let unexpectedConflictComputer: ClientComputerRef | undefined
  try {
    unexpectedConflictComputer = await client.computerDefinitions.createComputer("helmr-runtime-smoke", {
      ...computerCreateOptions,
      key: `${computerKey}-conflict`,
    })
  } catch (error) {
    assertEqual(
      errorCode(error),
      "idempotency_conflict",
      "divergent Computer create returned the wrong error",
    )
  }
  if (unexpectedConflictComputer !== undefined) {
    await unexpectedConflictComputer.delete({
      idempotencyKey: `computer:conflict-cleanup:${marker}`,
    })
    throw new Error("divergent Computer create reused an idempotency key")
  }

  const matches = await client.computers.list({ key: computerKey })
  assertEqual(matches.items.length, 1, "computer key lookup was not exact")
  const computer = matches.items[0]!
  const byKey = client.computers.ref(computer.id)
  assertEqual(computer.id, created.id, "computer key resolved to a different Computer")
  assertEqual(computer.definitionKey, "helmr-runtime-smoke", "computer definition key mismatch")

  const markerDirectory = "sandbox-smoke/nested"
  const markerPath = `${markerDirectory}/marker.txt`
  const largeFileKiB = 256
  const largePath = `${markerDirectory}/large-${largeFileKiB}k.txt`
  const stdin = `stdin:${marker}\n`
  const execOptions = {
    command: [
      "sh",
      "-ceu",
      [
        `mkdir -p ${markerDirectory}`,
        `node -e 'require("node:fs").writeFileSync("${largePath}", "x".repeat(${largeFileKiB} * 1024))'`,
        "IFS= read -r line",
        `printf 'marker=%s\\nstdin=%s\\n' "$SMOKE_MARKER" "$line" > ${markerPath}`,
        'printf \'stdout:%s:%s\\n\' "$SMOKE_MARKER" "$line"',
        "printf 'stderr:%s\\n' \"$SMOKE_MARKER\" >&2",
        "exit 7",
      ].join("; "),
    ],
    cwd: "/workspace",
    env: { SMOKE_MARKER: marker },
    stdin: new TextEncoder().encode(stdin),
    timeout: "2m",
    idempotencyKey: `computer:exec:${marker}`,
  } as const
  const initialCommand = await byKey.exec(execOptions, {
    signal: AbortSignal.timeout(15 * 60_000),
  })
  const initialExec = await collectCommand(initialCommand)
  assertExec(initialExec, marker)

  const replayCommand = await byKey.exec(execOptions, {
    signal: AbortSignal.timeout(15 * 60_000),
  })
  assertEqual(replayCommand.id, initialCommand.id, "Command replay changed the ID")
  const replayExec = await collectCommand(replayCommand)
  assertExec(replayExec, marker)
  assertEqual(
    encodeExec(replayExec),
    encodeExec(initialExec),
    "Command idempotency replay changed the result",
  )

  const turnMarker = `${marker}-turn`
  const started = await startAgent("runtime-smoke", {
    input: {
      scenario: "computer-basic-exec-client-smoke",
      marker: turnMarker,
      expectedComputerMarker: marker,
      expectedEnvironment: "unknown",
      exerciseQuestion: false,
      largeFileKiB,
    },
    computer: { id: byKey.id },
    idempotencyKey: `agent:start:${marker}`,
  })
  const output = await waitOutput(started.session, started.turn, value =>
    value !== null && typeof value === "object" && !Array.isArray(value) &&
    "phase" in value && value.phase === "runtime-smoke" && "marker" in value && value.marker === turnMarker)
  const turnResult = await completedResult(started.turn)
  const postTurnExec = await collectCommand(await byKey.exec(
    {
      command: ["cat", markerPath],
      cwd: "/workspace",
      idempotencyKey: `computer:verify-turn:${marker}`,
    },
    {
      signal: AbortSignal.timeout(15 * 60_000),
    },
  ))
  assertEqual(postTurnExec.exitCode, 0, "Agent Computer verification command failed")
  const turnFile = new TextDecoder().decode(postTurnExec.stdout)
  assert(turnFile.includes(`marker=${turnMarker}`), "Agent did not update the Computer file")

  await started.session.cancel({ idempotencyKey: `session:cancel:${marker}` })
  const deleted = await deleteComputer(byKey, `computer:delete:${marker}`)
  assertEqual(deleted.computerId, created.id, "Computer delete receipt changed the ID")
  const deleteReplay = await created.delete({
    idempotencyKey: `computer:delete:${marker}`,
  })
  assertEqual(deleteReplay.computerId, created.id, "Computer delete replay changed the ID")

  return {
    computerCreateReplay: true,
    computerDeleteReplay: true,
    executionReplay: true,
    turnResult,
    output,
  }
})
function assertExec(result: CollectedCommand, marker: string): void {
  const stdout = new TextDecoder().decode(result.stdout)
  const stderr = new TextDecoder().decode(result.stderr)
  assertEqual(result.exitCode, 7, "Command did not preserve its nonzero exit code")
  assert(stdout.includes(`stdout:${marker}:stdin:${marker}`), "Command stdout mismatch")
  assert(stderr.includes(`stderr:${marker}`), "Command stderr mismatch")
}

function encodeExec(result: CollectedCommand): string {
  return JSON.stringify({
    exitCode: result.exitCode,
    stdout: Buffer.from(result.stdout).toString("base64"),
    stderr: Buffer.from(result.stderr).toString("base64"),
  })
}

type CollectedCommand = { exitCode: number; stdout: Uint8Array; stderr: Uint8Array }
async function collectCommand(command: CommandRef): Promise<CollectedCommand> {
 const signal = AbortSignal.timeout(15 * 60_000)
 const outcome = await command.wait({signal})
 if (outcome.kind !== "exited") throw new Error(`Command ${command.id} ended with ${outcome.kind}`)
 const output: Record<"stdout" | "stderr", Uint8Array[]> = {stdout: [],stderr: []}
 for await (const record of command.streamLogs({}, {signal})) {
  if (record.kind === "gap") throw new Error(`Command ${command.id} has an output gap`)
  output[record.stream].push(record.content)
 }
 return {exitCode: outcome.exitCode, stdout: Buffer.concat(output.stdout),stderr: Buffer.concat(output.stderr)}
}
