// Invoked by TestSessionNativeLocalPostgres. The bridge replaces only the Worker
// transport/capture, not the SDK, TS runtime, native Actor or Control Plane.
import { HelmrClient } from "../../../sdk/typescript/src/client"
import { runNativeProgram } from "./native-program"
import { PassThrough } from "node:stream"
import { mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import assert from "node:assert/strict"
import { test } from "node:test"
import { claudeModel, codexModel } from "../../../dev/workflows/probes/native-model"
import { conversation } from "../../../dev/workflows/tasks/issue-fixer/conversation"

const bridge = process.env.HELMR_LOCAL_BRIDGE
const qualification = bridge ? test : test.skip
qualification("native Actor through runtime, SDK HTTP and Postgres", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-session-native-"))
  const config = await (await fetch(`${bridge}/config`)).json() as any
  const requests: any[] = []
  const model = config.provider === "claude" ? claudeModel(requests) : codexModel(requests, false, true)
  const input = new PassThrough()
  let runtime: Promise<unknown> | undefined
  let finished = false
  const originalEnv = { ...process.env }
  const deadline = setTimeout(() => input.destroy(new Error("Native integration timed out")), 60000)
  try {
    await new Promise<void>(resolve => model.listen(0, "127.0.0.1", resolve))
    const port = (model.address() as { port: number }).port
    process.env.ISSUE_FIXER_REPOSITORY = directory
    await writeFile(join(directory, "package.json"), JSON.stringify({ scripts: { test: "node check.cjs" } }))
    await writeFile(join(directory, "check.cjs"), 'const fs=require("node:fs"); const p="checks"; fs.writeFileSync(p,String((fs.existsSync(p)?Number(fs.readFileSync(p)):0)+1))')
    process.env.ANTHROPIC_BASE_URL = `http://127.0.0.1:${port}`
    process.env.ANTHROPIC_API_KEY = "local-fixture"
    process.env.CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = "1"
    process.env.DISABLE_TELEMETRY = "1"
    const saved = await conversation(directory, config.sessionId, config.provider)
    if (config.provider === "codex") await writeFile(join(saved.directory, "config.toml"), `model_provider = "fixture"\nmodel = "gpt-5.1-codex"\n[model_providers.fixture]\nname = "local fixture"\nbase_url = "http://127.0.0.1:${port}/v1"\nwire_api = "responses"\nrequires_openai_auth = false\n`)
    const definition = config.provider === "claude"
      ? (await import("../../../dev/workflows/tasks/issue-fixer/claude")).claudeIssueFixer
      : (await import("../../../dev/workflows/tasks/issue-fixer/codex")).codexIssueFixer
    const client = new HelmrClient({ url: bridge!, apiKey: "fixture" })
    const session = client.sessions.ref(config.sessionId)
    const first = await session.send({ issue: "first unique input" })
    assert.equal(first.kind, "enqueued")
    let completed = 0, received = 0, ready = false
    let nativeID: string | undefined
    runtime = runNativeProgram(bridge!, config, definition, input, {
      async received() {
        if (received++ !== 0) return
        // Durable activation happened, but the Actor has not received this Turn.
        assert.equal(ready, false)
        const early = await session.send({ type: "invalid_fixture_message" })
        assert.equal(early.kind, "messaged")
        assert.equal(early.turn.id, first.turn.id)
      },
      ready() { ready = true },
      outcome(value) { assert.equal(value.outcome.case, "succeeded") },
      async beforeSettle(value) {
          assert.equal(value.disposition, "completed")
          assert.equal(Number(await readFile(join(directory, "checks"), "utf8")), completed + 1)
          const current = await conversation(directory, config.sessionId, config.provider)
          assert.ok(current.id)
          if (nativeID) assert.equal(current.id, nativeID)
          nativeID = current.id
      },
      async settled() { if (++completed === 2) await session.close() },
    })
    void runtime.finally(() => { finished = true }).catch(() => {})
    // Consume only persisted public events, like a Slack/Linear/desktop adapter.
    let after = 0, questionReply: any, questionCount = 0, approvalCount = 0
    let secondID: string | undefined
    const interact = async () => {
      while (!finished && completed < 2) {
        const page = await session.events.list({ after, limit: 1000 })
        after = page.nextAfter
        for (const event of page.records) {
          const data = event.data as any
          if (event.kind !== "output" || data.type !== "human_requested") continue
          assert.equal(event.turnId, first.turn.id)
          assert.equal((await first.turn.retrieve()).status, "running")
          if (data.kind === "answer") {
            questionCount++
            const second = await session.enqueue({ issue: "second unique input" })
            secondID = second.id
            assert.notEqual(secondID, first.turn.id)
            assert.equal((await second.retrieve()).status, "queued")
            questionReply = { type: "answer", requestId: data.requestId,
              answers: { [config.provider === "claude" ? "Which option?" : "choice"]: ["A"] } }
            const sent = await session.send(questionReply)
            assert.equal(sent.kind, "messaged")
            assert.equal(sent.turn.id, first.turn.id)
          } else {
            approvalCount++
            // Distinct message identity: application rejects an already answered request.
            await first.turn.send(questionReply)
            await first.turn.send({ type: "approval", requestId: data.requestId, allow: false })
          }
        }
        await new Promise(resolve => setTimeout(resolve, 10))
      }
    }
    await Promise.all([runtime, interact()])
    assert.equal(questionCount, 1); assert.equal(approvalCount, 1); assert.equal(completed, 2)
    assert.equal((await first.turn.retrieve()).status, "completed")
    assert.equal((await session.turn(secondID!).retrieve()).status, "completed")
    await assert.rejects(first.turn.send(questionReply))
    const all = await session.events.list({ after: 0, limit: 1000 })
    assert.equal(all.hasMore, false)
    assert.deepEqual(all.records.filter(e => e.kind === "turn.completed").map(e => e.turnId), [first.turn.id, secondID!])
    assert.equal((all.records.filter(e => e.kind === "message.handled")).length, 2)
    assert.equal((all.records.filter(e => e.kind === "message.rejected")).length, 2)
    assert.equal((all.records.filter(e => e.kind === "turn.failed" || e.kind === "message.unknown")).length, 0)
    const last = requests.at(-1)
    if (config.provider === "codex") {
      const answer = last.input.find((item: any) => item.type === "function_call_output" && item.call_id === "call_question")
      assert.deepEqual(JSON.parse(answer.output), { answers: { choice: { answers: ["A"] } } })
    } else {
      const answer = last.messages.flatMap((message: any) => Array.isArray(message.content) ? message.content : [])
        .find((block: any) => block.type === "tool_result" && block.tool_use_id === "toolu_question")
      assert.ok(answer.content.includes('"Which option?"="A"'))
    }
    const history = JSON.stringify(last)
    for (const text of ["first unique input", "second unique input"]) assert.ok(history.includes(text))
    assert.match(history, config.provider === "claude" ? /User denied this operation/ : /reject|declin/i)
    await assert.rejects(stat(join(directory, "denied-command-marker")), { code: "ENOENT" })
  } finally {
    clearTimeout(deadline)
    finished = true
    input.destroy()
    // EOF cancels the actual runtime and its native child before state deletion.
    await runtime?.catch(() => {})
    model.closeAllConnections(); await new Promise<void>(resolve => model.close(() => resolve()))
    for (const key of Object.keys(process.env)) delete process.env[key]
    Object.assign(process.env, originalEnv)
    await rm(directory, { recursive: true, force: true })
  }
})
