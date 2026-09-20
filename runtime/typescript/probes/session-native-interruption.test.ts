// Real native/SDK/runtime/DB interruption chain. Worker physical observations and
// captured Workspace bytes are fixtures; the provider home stays on local disk.
import { randomUUIDv7 } from "node:crypto"
import { PassThrough } from "node:stream"
import { mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import assert from "node:assert/strict"
import { test } from "node:test"
import { HelmrClient } from "../../../sdk/typescript/src/client"
import { programProto } from "../../../proto/typescript/src/index"
import { frame, runNativeProgram } from "./native-program"
import { claudeModel, codexModel } from "../../../dev/workflows/probes/native-model"
import { conversation } from "../../../dev/workflows/tasks/issue-fixer/conversation"

const bridge = process.env.HELMR_LOCAL_BRIDGE
const qualification = bridge ? test : test.skip
qualification("interrupt, exact hold and native history across Helmr Runs", async () => {
  const initial = await (await fetch(`${bridge}/config`)).json() as any
  const directory = await mkdtemp(join(tmpdir(), "helmr-interrupted-native-"))
  const requests: any[] = []
  const model = initial.provider === "claude" ? claudeModel(requests, true) : codexModel(requests)
  const streams: PassThrough[] = [], runtimes: Promise<unknown>[] = []
  const originalEnv = { ...process.env }
  let finished = false
  const deadline = setTimeout(() => { for (const input of streams) input.destroy(new Error("Interruption qualification timed out")) }, 60000)
  const post = async (path: string, body: unknown = {}) => {
    const response = await fetch(`${bridge}${path}`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(body) })
    if (!response.ok) throw new Error(`${path}: ${response.status} ${await response.text()}`)
    return await response.json() as any
  }
  try {
    await new Promise<void>(resolve => model.listen(0, "127.0.0.1", resolve))
    const port = (model.address() as { port: number }).port
    Object.assign(process.env, { ISSUE_FIXER_REPOSITORY: directory,
      ANTHROPIC_BASE_URL: `http://127.0.0.1:${port}`, ANTHROPIC_API_KEY: "local-fixture",
      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1", DISABLE_TELEMETRY: "1" })
    await writeFile(join(directory, "package.json"), JSON.stringify({ scripts: { test: "node check.cjs" } }))
    await writeFile(join(directory, "check.cjs"), 'const fs=require("node:fs"); const p="checks"; fs.writeFileSync(p,String((fs.existsSync(p)?Number(fs.readFileSync(p)):0)+1))')
    const saved = await conversation(directory, initial.sessionId, initial.provider)
    if (initial.provider === "codex") await writeFile(join(saved.directory, "config.toml"), `model_provider = "fixture"\nmodel = "gpt-5.1-codex"\n[model_providers.fixture]\nname = "local fixture"\nbase_url = "http://127.0.0.1:${port}/v1"\nwire_api = "responses"\nrequires_openai_auth = false\n`)
    const definition = initial.provider === "claude"
      ? (await import("../../../dev/workflows/tasks/issue-fixer/claude")).claudeIssueFixer
      : (await import("../../../dev/workflows/tasks/issue-fixer/codex")).codexIssueFixer
    const session = new HelmrClient({ url: bridge!, apiKey: "fixture" }).sessions.ref(initial.sessionId)
    const first = await session.send({ issue: "first interrupted input" })
    assert.equal(first.kind, "enqueued")
    const input = new PassThrough(); streams.push(input)
    let interrupted: programProto.ActorOutcome | undefined
    const stopped = runNativeProgram(bridge!, initial, definition, input, {
      outcome(value) { interrupted = value; assert.equal(value.outcome.case, "interrupted") },
      async beforeSettle() { assert.fail("interrupted native processing must not settle a Turn") },
      async settled() { assert.fail("unexpected settlement") },
    })
    runtimes.push(stopped)
    void stopped.finally(() => { finished = true }).catch(() => {})
    let after = 0, pendingReply: any, second: ReturnType<typeof session.turn> | undefined
    let requestedHold: string | undefined
    const interrupt = async () => {
      while (!finished) {
        const page = await session.events.list({ after, limit: 1000 }); after = page.nextAfter
        for (const event of page.records) {
          const data = event.data as any
          if (event.kind !== "output" || data.type !== "human_requested") continue
          assert.equal(data.kind, "answer")
          assert.equal(event.turnId, first.turn.id)
          pendingReply = { type: "answer", requestId: data.requestId,
            answers: { [initial.provider === "claude" ? "Which option?" : "choice"]: ["A"] } }
          second = await session.enqueue({ issue: "second resumed input" })
          const receipt = await first.turn.interrupt()
          requestedHold = receipt.holdId
          assert.equal((await first.turn.retrieve()).status, "running")
          assert.equal((await second.retrieve()).status, "queued")
          await assert.rejects(session.resume({ holdId: receipt.holdId }), { code: "not_settled" })
          await assert.rejects(first.turn.send(pendingReply))
          await assert.rejects(session.send({ issue: "must not enqueue while held" }), { code: "session_held" })
          // Observe the real CP control response and forward its exact scope to
          // the runtime, as the Worker does. No direct AbortController injection.
          const control = await post("/worker/control", { lease: initial.lease,
            correlation_id: randomUUIDv7(), run_generation: initial.runGeneration })
          assert.equal(control.hold_id, receipt.holdId)
          assert.equal(control.turn_id, first.turn.id)
          input.write(frame(programProto.ResumeDecisionSchema, { kind: "session_stop", dataJson: JSON.stringify({
            execution: { session_id: initial.sessionId, run_id: initial.runId, attempt_number: 1, run_generation: initial.runGeneration },
            turn_id: control.turn_id, hold_id: control.hold_id, reason: control.reason,
          }) }))
          return
        }
        await new Promise(resolve => setTimeout(resolve, 10))
      }
      assert.fail("native process ended before interruption")
    }
    await Promise.all([stopped, interrupt()])
    assert.ok(interrupted); assert.equal(interrupted.outcome.case, "interrupted")
    if (interrupted.outcome.case !== "interrupted") assert.fail("missing interruption outcome")
    assert.equal(interrupted.outcome.value.holdId, requestedHold)
    assert.equal(interrupted.outcome.value.turnId, first.turn.id)
    await assert.rejects(stat(join(directory, "checks")), { code: "ENOENT" })
    const nativeID = (await conversation(directory, initial.sessionId, initial.provider)).id
    assert.ok(nativeID)
    assert.ok(second)
    assert.equal((await second.retrieve()).status, "queued")
    // The production Actor awaited direct native exit. Only now may the Go test
    // provide its explicit physical/capture fixture and apply owning finalization.
    await post("/fixture/finalize", { run_generation: initial.runGeneration,
      interrupted: { hold_id: interrupted.outcome.value.holdId, turn_id: interrupted.outcome.value.turnId } })
    const held = await session.retrieve()
    assert.equal(held.currentRunId, null)
    assert.equal(held.activeTurnId, null)
    assert.equal(held.dispatch.state, "held")
    if (held.dispatch.state !== "held") assert.fail("expected settled hold")
    assert.equal(held.dispatch.reason, "interrupted")
    assert.notEqual(held.dispatch.holdId, requestedHold)
    assert.equal((await first.turn.retrieve()).status, "interrupted")
    assert.equal((await second.retrieve()).status, "queued")
    await assert.rejects(session.resume({ holdId: requestedHold! }), { code: "stale_hold" })
    await assert.rejects(first.turn.send(pendingReply))
    await session.resume({ holdId: held.dispatch.holdId })
    const resumed = await post("/fixture/start")
    assert.notEqual(resumed.runId, initial.runId)
    assert.ok(resumed.runGeneration > 0)
    assert.equal(resumed.startInputSequence, 1)
    const nextInput = new PassThrough(); streams.push(nextInput)
    let acceptedStale = false, completed = 0
    const continued = runNativeProgram(bridge!, resumed, definition, nextInput, {
      async received(result) {
        if (result.resolution_kind !== "completed") return
        assert.equal(result.resolution.turn.id, second!.id)
        assert.equal(acceptedStale, false)
        await second!.send(pendingReply)
        acceptedStale = true
      },
      outcome(value) { assert.equal(value.outcome.case, "succeeded") },
      async beforeSettle(value) {
        assert.equal(value.disposition, "completed")
        assert.equal(Number(await readFile(join(directory, "checks"), "utf8")), 1)
        assert.equal((await conversation(directory, initial.sessionId, initial.provider)).id, nativeID)
      },
      async settled() { completed++; await session.close() },
    })
    runtimes.push(continued)
    await continued
    assert.equal(completed, 1)
    assert.equal((await second.retrieve()).status, "completed")
    const events = await session.events.list({ after: 0, limit: 1000 })
    assert.equal(events.hasMore, false)
    assert.deepEqual(events.records.filter(e => e.kind === "turn.interrupted").map(e => e.turnId), [first.turn.id])
    assert.deepEqual(events.records.filter(e => e.kind === "turn.completed").map(e => e.turnId), [second.id])
    assert.equal(events.records.filter(e => e.kind === "message.handled" || e.kind === "message.unknown").length, 0)
    const rejected = events.records.filter(e => e.kind === "message.rejected")
    assert.equal(rejected.length, 1)
    assert.equal((rejected[0]!.data as any).code, "handler_rejected")
    const history = JSON.stringify(requests.at(-1))
    assert.ok(history.includes("first interrupted input")); assert.ok(history.includes("second resumed input"))
    assert.ok(requests.length >= 2)
  } finally {
    clearTimeout(deadline); finished = true
    for (const input of streams) input.destroy()
    await Promise.allSettled(runtimes)
    model.closeAllConnections(); await new Promise<void>(resolve => model.close(() => resolve()))
    for (const key of Object.keys(process.env)) delete process.env[key]
    Object.assign(process.env, originalEnv)
    await rm(directory, { recursive: true, force: true })
  }
})
