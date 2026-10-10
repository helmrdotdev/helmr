import assert from "node:assert/strict"
import { test } from "node:test"
import { spawn } from "node:child_process"
import { once } from "node:events"
import { create, fromBinary, toBinary } from "@bufbuild/protobuf"
import { agentProto as p } from "@helmr/proto"

// The fixture is installed for Linux, compiled by the actual builder, admitted
// as SquashFS, and mounted read-only at the managed Program path by the harness.
const enabled = process.env["HELMR_NATIVE_RUNTIME_TEST"] === "1"
const flags = ["--no-strip-types", "--no-global-search-paths", "--enable-source-maps", "--import=file:///opt/helmr/runtime/helmr/module-preload.mjs"]
const identity = create(p.SessionIdentitySchema, { sessionId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33", processEpoch: 1n })
const json = (value: unknown) => Buffer.from(JSON.stringify(value))
function launch(agentId: string) {
  const child = spawn(process.execPath, [...flags, "/opt/helmr/runtime/helmr/entry.mjs"], { cwd: "/workspace", stdio: ["pipe", "pipe", "pipe", "pipe"], env: { PATH: process.env["PATH"] } })
  const closed = once(child, "close")
  const errors: Buffer[] = []
  child.stderr.on("data", chunk => errors.push(Buffer.from(chunk)))
  child.stdout.resume()
  const control = child.stdio[3] as NodeJS.ReadableStream
  function send(command: p.GuestCommand["command"], deliveryId = "") {
    const body = toBinary(p.GuestCommandSchema, create(p.GuestCommandSchema, { identity, command, deliveryId }))
    const header = Buffer.alloc(4); header.writeUInt32BE(body.length)
    child.stdin.write(Buffer.concat([header, body]))
  }
  async function* events() {
    let pending = Buffer.alloc(0)
    for await (const chunk of control) {
      pending = Buffer.concat([pending, Buffer.from(chunk)])
      while (pending.length >= 4) {
        const size = pending.readUInt32BE(0)
        assert.ok(size > 0 && size <= 8 * 1024 * 1024)
        if (pending.length < size + 4) break
        const event = fromBinary(p.ProgramEventSchema, pending.subarray(4, size + 4))
        pending = pending.subarray(size + 4)
        assert.deepEqual(event.identity, identity)
        yield event.event
      }
    }
    assert.equal(pending.length, 0, "truncated Session frame")
  }
  send({ case: "start", value: create(p.SessionStartSchema, { agentId, computerId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc30", deploymentId: "deployment", recoveryKind: p.SessionStart_RecoveryKind.INITIAL }) })
  return { child, closed, errors, send, events }
}
for (const agentId of ["deploy", "worker"]) {
  test(`admitted native Runtime executes packed SDK Agent ${agentId}`, { skip: !enabled, timeout: 30_000 }, async t => {
    const f = launch(agentId)
    t.after(() => { f.child.kill("SIGKILL") })
    let ready = false, completed = 0, shutdown = false
    for await (const event of f.events()) {
      assert.notEqual(event.case, "failed", JSON.stringify(event))
      if (event.case === "ready") {
        assert.equal(ready, false); ready = true
        f.send({ case: "dispatch", value: create(p.TurnDispatchSchema, { turnId: "turn", sequence: 1n, createdAt: "2026-10-09T00:00:00Z", inputJson: json(null), sourceJson: json({ kind: "api" }) }) }, "dispatch")
      } else if (event.case === "operation") {
        const operation = event.value
        assert.ok(new Set<p.Operation_Method>([p.Operation_Method.CLOSE_PROCESSING, p.Operation_Method.CONVERGE_NATIVE, p.Operation_Method.FINALIZE]).has(operation.method), `unexpected operation ${operation.method}`)
        // The protocol fixture acknowledges finalization; it does not prove a
        // control-plane save or publication of the Computer's disk.
        const payload = JSON.parse(Buffer.from(operation.payloadJson).toString())
        const value = operation.method === p.Operation_Method.FINALIZE ? { status: "completed", result: payload.result } : null
        f.send({ case: "operationResult", value: create(p.OperationResultSchema, { requestId: operation.requestId, outcome: { case: "valueJson", value: json(value) } }) })
      } else if (event.case === "deliveryResult") {
        assert.equal(event.value.outcome.case, "valueJson")
        if (event.value.deliveryId === "dispatch") {
          assert.equal(completed++, 0)
          const outcome = JSON.parse(Buffer.from(event.value.outcome.value as Uint8Array).toString())
          assert.equal(outcome.status, "completed")
          assert.deepEqual(outcome.result, agentId === "deploy"
            ? { asset: "installed", native: 42, same: true, worker: 42, fork: 42 }
            : { asset: "installed", same: true })
          f.send({ case: "shutdown", value: create(p.SessionShutdownSchema, { reason: "fixture completed" }) }, "shutdown")
        } else { assert.equal(event.value.deliveryId, "shutdown"); shutdown = true }
      }
    }
    const [code, signal] = await f.closed
    assert.equal(code, 0, Buffer.concat(f.errors).toString()); assert.equal(signal, null)
    assert.equal(ready, true); assert.equal(completed, 1); assert.equal(shutdown, true)
  })
}
for (const signal of ["SIGTERM", "SIGKILL"] as const) {
  test(`managed Session Runtime stops on ${signal} while idle`, { skip: !enabled, timeout: 15_000 }, async t => {
    const f = launch("deploy")
    t.after(() => { f.child.kill("SIGKILL") })
    let ready = false
    for await (const event of f.events()) {
      assert.equal(event.case, "ready")
      ready = true
      assert.equal(f.child.kill(signal), true)
    }
    const [code, actualSignal] = await f.closed
    assert.equal(ready, true); assert.equal(code, null); assert.equal(actualSignal, signal)
  })
}
