import assert from "node:assert/strict"
import { test } from "node:test"
import { spawn } from "node:child_process"
import { once } from "node:events"
import { create, fromBinary, toBinary } from "@bufbuild/protobuf"
import { programProto as p } from "@helmr/proto"

// The fixture is installed for Linux, compiled by the actual builder, admitted
// as SquashFS, and mounted read-only at the managed Program path by the harness.
const enabled = process.env["HELMR_NATIVE_RUNTIME_TEST"] === "1"
const flags = ["--no-strip-types", "--no-global-search-paths", "--enable-source-maps", "--import=file:///opt/helmr/runtime/helmr/module-preload.mjs"]
function frame(body: Uint8Array) {
  const output = Buffer.alloc(body.length + 4)
  output.writeUInt32BE(body.length)
  output.set(body, 4)
  return output
}
for (const kind of ["task", "actor"] as const) {
  test(`admitted native Runtime executes packed SDK ${kind}`, { skip: !enabled }, async () => {
    const declaredId = kind === "task" ? "deploy" : "worker"
    const start = create(p.ProgramStartSchema, {
      entrypointDeclaredId: declaredId, runId: "run-1", attemptNumber: 1,
      cause: { kind: kind === "task" ? { case: "api", value: {} } : { case: "actorStart", value: {} } },
      deploymentId: "deployment-1", deploymentVersion: "v1",
      workspaceId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc30", baseWorkspaceVersionId: "version-1",
      entrypoint: kind === "task" ? { case: "task", value: { payload: { case: "noPayload", value: {} } } } : { case: "actor", value: { sessionId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33", startInputSequence: 0n, inputHighWatermark: 0n, runGeneration: 1n } },
    })
    const release = create(p.EntrypointReleaseSchema, { runId: start.runId, attemptNumber: start.attemptNumber, entrypoint: { declaredId, kind: kind === "task" ? { case: "task", value: {} } : { case: "actor", value: {} } } })
    const child = spawn(process.execPath, [...flags, "/opt/helmr/runtime/helmr/entry.mjs"], { stdio: ["pipe", "pipe", "pipe", "pipe"], env: { PATH: process.env["PATH"] } })
    const control = child.stdio[3]!
    const output: Buffer[] = [], errors: Buffer[] = []
    control.on("data", chunk => output.push(Buffer.from(chunk)))
    child.stderr.on("data", chunk => errors.push(Buffer.from(chunk)))
    child.stdin.write(Buffer.concat([frame(toBinary(p.ProgramStartSchema, start)), frame(toBinary(p.EntrypointReleaseSchema, release))]))
    const [code, signal] = await once(child, "close")
    assert.equal(code, 0, Buffer.concat(errors).toString()); assert.equal(signal, null)
    const data = Buffer.concat(output), events = []
    for (let offset = 0; offset < data.length;) {
      const size = data.readUInt32BE(offset); offset += 4
      events.push(fromBinary(p.RunEventSchema, data.subarray(offset, offset + size)).event); offset += size
    }
    assert.equal(events[0]?.case, "entrypointReady")
    const outcome = events.find(event => event.case === `${kind}Outcome`)
    assert.ok(outcome && (outcome.case === "taskOutcome" || outcome.case === "actorOutcome"))
    assert.equal(outcome.value.outcome.case, "succeeded", JSON.stringify(outcome, (_, value) => typeof value === "bigint" ? String(value) : value))
    if (outcome.case === "taskOutcome" && outcome.value.outcome.case === "succeeded") {
      assert.deepEqual(JSON.parse(outcome.value.outcome.value.outputJson), { asset: "installed", native: 42, same: true, worker: 42, fork: 42 })
    }
  })
}

for (const signal of ["SIGTERM", "SIGKILL"] as const) {
  test(`managed Runtime stops on ${signal} while awaiting release`, { skip: !enabled }, async () => {
    const start = create(p.ProgramStartSchema, {
      entrypointDeclaredId: "deploy", runId: "run-stop", attemptNumber: 1,
      cause: { kind: { case: "api", value: {} } }, deploymentId: "deployment-1", deploymentVersion: "v1",
      workspaceId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc30", baseWorkspaceVersionId: "version-1",
      entrypoint: { case: "task", value: { payload: { case: "noPayload", value: {} } } },
    })
    const child = spawn(process.execPath, [...flags, "/opt/helmr/runtime/helmr/entry.mjs"], { stdio: ["pipe", "pipe", "pipe", "pipe"], env: { PATH: process.env["PATH"] } })
    const closed = once(child, "close")
    const control = child.stdio[3]!
    const ready = once(control, "data")
    child.stdin.write(frame(toBinary(p.ProgramStartSchema, start)))
    await ready // The real entry imported and validated the SDK declaration.
    assert.equal(child.kill(signal), true)
    const [code, actualSignal] = await closed
    assert.equal(code, null); assert.equal(actualSignal, signal)
  })
}
