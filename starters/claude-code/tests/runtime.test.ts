import assert from "node:assert/strict"
import { createServer, type ServerResponse } from "node:http"
import { once } from "node:events"
import { channel } from "node:diagnostics_channel"
import { spawnSync, type ChildProcess } from "node:child_process"
import { mkdtemp, mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises"
import { existsSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { test } from "node:test"
import { ClaudeSession, nativeHome, type ClaudeOptions } from "../agents/claude-bridge"
import { SessionRuntime, type SessionDriver } from "../../../runtime/typescript/src/agent-session"
import type { InitContext, Turn } from "../../../sdk/typescript/src/agent"

const delay = (ms: number) => new Promise(resolve => setTimeout(resolve, ms))
async function until(check: () => boolean, label: string) {
  for (let n = 0; n < 1000; n++) { if (check()) return; await delay(20) }
  throw new Error(`Timed out: ${label}`)
}
const text = (value: string) => [{ type: "text" as const, text: value }]
const tool = (name: string, input: unknown) => ({ type: "tool_use", id: `tool_${crypto.randomUUID()}`, name, input })
const question = (prompt: string) => tool("AskUserQuestion", { questions: [{ question: prompt, header: "Choice", multiSelect: false, options: [{ label: "Alpha", description: "First" }, { label: "Beta", description: "Second" }] }] })

// Only model HTTP and the external Helmr driver are substituted. Both the
// native executable and Helmr SessionRuntime execute the starter's actual bridge.
test("Claude starter retains native execution and maps Helmr input, questions and cancellation", { timeout: 180_000 }, async () => {
  const root = await mkdtemp(join(tmpdir(), "helmr-claude-starter-"))
  const cwd = join(root, "workspace"), stateDirectory = join(root, "state")
  await mkdir(cwd)
  await mkdir(join(cwd, ".claude"))
  const settingsMarker = join(cwd, "native-settings-loaded")
  await writeFile(join(cwd, ".claude/settings.json"), JSON.stringify({ hooks: { SessionStart: [{ hooks: [{ type: "command", command: `echo loaded > ${settingsMarker}` }] }] } }))
  // Exercise native shell snapshots as in the Debian execution image.
  await writeFile(join(root, ".bashrc"), "# Shell snapshot fixture\n")
  const requests: any[] = [], held: { body: any; response: ServerResponse; send: (value?: any) => void }[] = [], actions: any[] = []
  const output: any[] = [], questions: any[] = [], holds: string[] = [], launchers: ChildProcess[] = []
  const server = createServer(async (request, response) => {
    let raw = ""
    for await (const chunk of request) raw += chunk
    if (request.url?.includes("count_tokens")) { response.end(JSON.stringify({ input_tokens: 10 })); return }
    if (!request.url?.includes("/messages")) { response.writeHead(404); response.end("{}"); return }
    const body = JSON.parse(raw)
    // Native auxiliary requests (for example session titles) have their own
    // system prompt and must not consume scripted conversation responses.
    const conversation = JSON.stringify(body.system ?? []).includes("You are a local fixture.")
    if (conversation) requests.push(body)
    const send = (value: any = { type: "text", text: "offline reply" }) => {
      const blocks = Array.isArray(value) ? value : [value]
      const message = { id: `msg_${crypto.randomUUID()}`, type: "message", role: "assistant", model: body.model, content: [], stop_reason: null, stop_sequence: null, usage: { input_tokens: 10, output_tokens: 1 } }
      response.writeHead(200, { "content-type": "text/event-stream" })
      const emit = (type: string, data: unknown) => response.write(`event: ${type}\ndata: ${JSON.stringify({ type, ...(data as object) })}\n\n`)
      emit("message_start", { message })
      blocks.forEach((block, index) => {
        emit("content_block_start", { index, content_block: block.type === "tool_use" ? { ...block, input: {} } : { type: "text", text: "" } })
        emit("content_block_delta", { index, delta: block.type === "tool_use" ? { type: "input_json_delta", partial_json: JSON.stringify(block.input) } : { type: "text_delta", text: block.text } })
        emit("content_block_stop", { index })
      })
      emit("message_delta", { delta: { stop_reason: blocks.some(block => block.type === "tool_use") ? "tool_use" : "end_turn", stop_sequence: null }, usage: { output_tokens: 2 } })
      emit("message_stop", {}); response.end()
    }
    const action = conversation ? actions.shift() : undefined
    if (action === "hold") held.push({ body, response, send })
    else send(action)
  })
  server.listen(0, "127.0.0.1"); await once(server, "listening")
  const originalEnv = process.env
  // The native executable receives no personal settings or host credentials.
  // Suppress external telemetry/update traffic in this host-local fixture; the
  // guest fixture exercises native optional traffic behind blocked external egress.
  process.env = { PATH: originalEnv.PATH, HOME: root, TMPDIR: root, LANG: "en_US.UTF-8", CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1", ANTHROPIC_API_KEY: "offline-fixture", ANTHROPIC_BASE_URL: `http://127.0.0.1:${(server.address() as import("node:net").AddressInfo).port}` }
  let onNativeSpawn: (() => void) | undefined
  const launches = channel("child_process")
  const observe = (value: unknown) => { const child = (value as { process: ChildProcess }).process; child.once("spawn", () => { if (child.spawnargs.includes("--input-format")) { launchers.push(child); onNativeSpawn?.() } }) }
  launches.subscribe(observe)
  const options: ClaudeOptions = { cwd, stateDirectory, sessionId: crypto.randomUUID(), model: "claude-sonnet-4-5", appendSystemPrompt: "You are a local fixture.", recovery: "initial" }
  let bridge = new ClaudeSession(options), initCount = 0, sequence = 0, answerQuestion = true, approve = true
  let responseGate: Promise<void> | undefined, responseEntered = false
  const context: InitContext = { session: { id: options.sessionId }, computer: { id: "computer" }, deployment: { id: "deployment" }, recovery: { kind: "initial" }, signal: new AbortController().signal, mcp: { getConnectionOptions: async () => { throw new Error("unused") } } }
  const driver: SessionDriver = {
    registerMessages: async () => {}, closeProcessing: async () => {},
    ask: async (_turn, _id, question, signal) => {
      questions.push(question)
      assert.equal(question.type, "choice")
      const chosen = question.options.find(option => option.id === (approve ? "allow" : "deny")) ?? question.options[0]
      return { answer: answerQuestion ? Promise.resolve({ answer: { selected: [{ id: chosen.id, value: chosen.value }] }, answeredBy: { kind: "user" as const, id: "tester" } }) : new Promise<never>((_, reject) => { signal.addEventListener("abort", () => reject(signal.reason), { once: true }) }), withdraw: async () => {} }
    },
    respond: async (_turn, _id, content) => { output.push(content); responseEntered = true; await responseGate },
    output: async (_turn, _id, content) => { output.push(content); return { sequence: output.length } },
    confirmDrain: async () => {}, finalize: async (_id, result) => ({ status: "completed", result }),
    fail: async (_id, error) => { console.error("TURN_FAILED", error); return error.code === "interrupted" ? { status: "interrupted" } : { status: "failed", error } },
    hold: async reason => { holds.push(reason); console.error("HELD", reason) },
  }
  const makeRuntime = () => new SessionRuntime({ kind: "agent", id: "fixture", template: undefined!, init: () => { initCount++; return bridge }, turn: async (turn: Turn, { initResult }) => {
    const { response, ...result } = await (initResult as ClaudeSession).run(turn)
    if (response !== undefined) await turn.respond(response)
    return result
  } }, { ...context, recovery: { kind: options.recovery } }, driver)
  let runtime = makeRuntime()
  const dispatch = (input: string) => { const n = ++sequence, id = `turn-${n}`; return { id, result: runtime.dispatch({ id, sequence: n, createdAt: new Date().toISOString(), input: text(input), source: { kind: "api_key" } }) } }
  const alive = (pid: number) => { try { process.kill(pid, 0); return true } catch { return false } }
  const shells: number[] = []
  try {
    let initialStop: Promise<void> | undefined
    onNativeSpawn = () => { onNativeSpawn = undefined; initialStop = runtime.interrupt("turn-1", new Error("stop before first input")) }
    assert.equal((await dispatch("cancel before native initialization").result).status, "interrupted")
    await initialStop
    assert.equal(requests.length, 0); assert.deepEqual(holds, [])
    const initialLaunchCount = launchers.length
    console.log("PASS first cancellation before input admission")
    assert.equal((await dispatch("remember first input").result).status, "completed")
    assert.equal((await dispatch("second input").result).status, "completed")
    assert.equal(initCount, 1); assert.equal(launchers.length, initialLaunchCount + 1)
    assert.ok(JSON.stringify(requests.at(-1).messages).includes("remember first input"))
    assert.equal((await readFile(settingsMarker, "utf8")).trim(), "loaded", "native project settings hooks are loaded")
    console.log("PASS retained owner, history and native project settings")
    actions.push("hold")
    const steering = dispatch("wait for another input")
    await until(() => held.length === 1, "held model response")
    await runtime.deliverMessage(steering.id, "followup", text("admitted followup marker"), { kind: "api_key" })
    held[0].send({ type: "text", text: "first answer" })
    assert.equal((await steering.result).status, "completed")
    assert.ok(JSON.stringify(requests.at(-1).messages).includes("admitted followup marker"))
    await assert.rejects(runtime.deliverMessage(steering.id, "stale", text("stale marker"), { kind: "api_key" }))
    console.log("PASS admitted streaming input")
    actions.push(question("Which option?"))
    const beforeQuestions = questions.length
    assert.equal((await dispatch("ask a question").result).status, "completed")
    assert.equal(questions.length, beforeQuestions + 1)
    assert.ok(JSON.stringify(requests.at(-1).messages).includes("Alpha"))
    console.log("PASS native question mapping")
    for (const allowed of [false, true]) {
      approve = allowed
      const marker = join(cwd, `approved-${allowed}`), before = questions.length
      actions.push(tool("Bash", { command: `echo approved > '${marker}'`, description: "Write fixture marker" }))
      assert.equal((await dispatch(`permission ${allowed}`).result).status, "completed")
      assert.equal(questions.length, before + 1); assert.equal(existsSync(marker), allowed)
    }
    approve = true; console.log("PASS native approvals")
    actions.push("hold")
    const cancelled = dispatch("cancel held response")
    await until(() => held.length === 2, "held cancellable model")
    await runtime.deliverMessage(cancelled.id, "queued", text("cancel queued input"), { kind: "api_key" })
    await runtime.interrupt(cancelled.id, new Error("stop"))
    assert.equal((await cancelled.result).status, "interrupted")
    assert.ok(launchers[initialLaunchCount].exitCode !== null || launchers[initialLaunchCount].signalCode !== null)
    const calls = requests.length
    await delay(200); assert.equal(requests.length, calls)
    assert.equal((await dispatch("fresh after cancellation").result).status, "completed")
    assert.equal(launchers.length, initialLaunchCount + 2); assert.equal(requests.length, calls + 1)
    assert.ok(JSON.stringify(requests.at(-1).messages).includes("remember first input"))
    console.log("PASS cancellation and history reconstruction")
    answerQuestion = false; actions.push(question("Cancel this question?"))
    const count = questions.length, pending = dispatch("cancel pending question")
    await until(() => questions.length > count, "pending question")
    await runtime.interrupt(pending.id, new Error("stop question"))
    assert.equal((await pending.result).status, "interrupted")
    answerQuestion = true
    assert.equal((await dispatch("after question cancellation").result).status, "completed")
    console.log("PASS question cancellation")
    actions.push(tool("Agent", { description: "Fixture foreground agent", prompt: "Return a short fixture reply", subagent_type: "general-purpose" }))
    assert.equal((await dispatch("run a foreground agent").result).status, "completed")
    console.log("PASS foreground subagent completion")
    for (const failed of [false, true]) {
      const releaseBackground = join(cwd, `release-background-${failed}`)
      actions.push(tool("Bash", { command: `while [ ! -f '${releaseBackground}' ]; do sleep 0.05; done; echo background-complete`, description: "Wait for fixture release", run_in_background: true }))
      assert.equal((await dispatch("start a background task that will finish naturally").result).status, "completed")
      const notificationCount = held.length
      actions.push("hold")
      await writeFile(releaseBackground, "release")
      await until(() => held.length > notificationCount, "background completion model response")
      const overlapping = dispatch("fresh input during background completion")
      await delay(100)
      if (failed) {
        const response = held[notificationCount].response
        response.writeHead(400, { "content-type": "application/json" })
        response.end(JSON.stringify({ type: "error", error: { type: "invalid_request_error", message: "fixture notification failure" } }))
      } else held[notificationCount].send({ type: "text", text: "background completion acknowledged" })
      assert.equal((await overlapping.result).status, "completed")
      assert.ok(JSON.stringify(requests.at(-1).messages).includes("fresh input during background completion"))
      assert.deepEqual(holds, [])
      console.log(`PASS background ${failed ? "failure" : "completion"} overlapping fresh input`)
    }
    // Use a real foreground-capable workload: native Bash rejects long sleep
    // placeholders under its default policy.
    const serverFile = join(cwd, "lifetime-server.cjs")
    await writeFile(serverFile, "const server = require(\"node:http\").createServer((req, res) => res.end(\"fixture\")); server.listen(0, \"127.0.0.1\"); setTimeout(() => server.close(), 60000).unref()")
    for (const background of [false, true]) {
      const marker = join(cwd, `shell-${background}`), finished = join(cwd, `finished-${background}`)
      const lock = join(cwd, `lock-${background}`)
      const body = `"${process.execPath}" "${serverFile}" & child=$!; echo $$ $child > "${marker}"; wait $child; echo finished > "${finished}"`
      const command = process.platform === "linux" ? `flock --exclusive '${lock}' /bin/sh -c '${body}'` : body
      actions.push(tool("Bash", { command, description: "Held fixture shell", ...(background ? { run_in_background: true } : {}) }))
      const active = dispatch(`start ${background ? "background" : "foreground"} tool`)
      await until(() => existsSync(marker), "tool shell started")
      const pids = (await readFile(marker, "utf8")).trim().split(/\s+/).map(Number); shells.push(...pids)
      assert.equal(pids.length, 2); assert.ok(pids.every(alive))
      let stop = active
      if (background) {
        assert.equal((await active.result).status, "completed"); assert.ok(pids.every(alive))
        assert.equal((await dispatch("next successful Turn").result).status, "completed"); assert.ok(pids.every(alive))
        actions.push("hold")
        const heldCount = held.length
        stop = dispatch("cancel retained background work")
        await until(() => held.length > heldCount, "held background cancellation")
      }
      await runtime.interrupt(stop.id, new Error("stop tools"))
      assert.equal((await stop.result).status, "interrupted")
      assert.ok(launchers.at(-1)!.exitCode !== null || launchers.at(-1)!.signalCode !== null, "cancellation joins the native owner")
      assert.equal((await dispatch("fresh after tool cancellation").result).status, "completed")
      // Native process behavior remains visible without becoming a Helmr contract.
      const lockStatus = process.platform === "linux" ? spawnSync("flock", ["--nonblock", "--exclusive", lock, "/bin/true"]).status : undefined
      if (lockStatus !== undefined) assert.ok(lockStatus === 0 || lockStatus === 1, "valid lifetime lock observation")
      console.log(JSON.stringify({ case: "native-tool-observation", platform: process.platform, background, pids: pids.map(pid => ({ pid, alive: alive(pid) })), lockHeld: lockStatus === undefined ? null : lockStatus === 1, finished: existsSync(finished) }))
      console.log(`PASS ${background ? "background" : "foreground"} native cancellation and fresh input`)
    }
    let releaseResponse!: () => void
    responseGate = new Promise(resolve => { releaseResponse = resolve }); responseEntered = false
    const closingTurn = dispatch("cancel while publishing final response")
    await until(() => responseEntered, "final response publication")
    const stoppingResponse = runtime.interrupt(closingTurn.id, new Error("stop publishing"))
    releaseResponse(); await stoppingResponse; responseGate = undefined
    assert.equal((await closingTurn.result).status, "interrupted")
    assert.ok(launchers.at(-1)!.exitCode !== null || launchers.at(-1)!.signalCode !== null)
    assert.equal((await dispatch("after final response cancellation").result).status, "completed")
    console.log("PASS cancellation after native completion")
    assert.deepEqual(holds, [])
    await bridge.close()
    const projects = join(nativeHome(options), "projects")
    const histories = (await readdir(projects, { recursive: true })).filter(name => name.endsWith(`/${options.sessionId}.jsonl`))
    assert.equal(histories.length, 1, "one native transcript for the Session")
    const transcript = join(projects, histories[0]!), saved = await readFile(transcript, "utf8")
    const before = requests.length
    options.recovery = "reconstructed"
    for (const empty of [false, true]) {
      if (empty) await writeFile(transcript, "")
      else await rm(transcript)
      bridge = new ClaudeSession(options); runtime = makeRuntime()
      assert.equal((await dispatch(empty ? "empty saved history" : "missing saved history").result).status, "interrupted")
      assert.equal(requests.length, before)
      assert.deepEqual(holds, ["session_continuation_lost"])
      holds.length = 0
    }
    await writeFile(transcript, saved)
    // Recovery is explicit after native resume rejects missing history.
    holds.length = 0
    bridge = new ClaudeSession(options); runtime = makeRuntime()
    assert.equal((await dispatch("explicit reconstruction").result).status, "completed"); assert.equal(requests.length, before + 1)
    assert.ok(JSON.stringify(requests.at(-1).messages).includes("remember first input"))
    console.log("PASS native missing/empty history refusal and explicit reconstruction")
    actions.push("hold")
    const beforeCrash = held.length, crash = dispatch("native owner will disappear")
    await until(() => held.length > beforeCrash, "native crash input")
    launchers.at(-1)!.kill("SIGKILL")
    assert.equal((await crash.result).status, "interrupted")
    assert.deepEqual(holds, ["session_continuation_lost"])
    const callsAfterCrash = requests.length
    await assert.rejects(async () => dispatch("must not silently replace the lost owner").result, /cannot dispatch/)
    assert.equal(requests.length, callsAfterCrash)
    bridge = new ClaudeSession(options); runtime = makeRuntime()
    assert.equal((await dispatch("fresh input after explicit owner reconstruction").result).status, "completed")
    assert.equal(requests.length, callsAfterCrash + 1)
    assert.ok(JSON.stringify(requests.at(-1).messages).includes("remember first input"))
    console.log("PASS native loss holds Session until explicit reconstruction")
  } finally {
    await bridge.close().catch(() => {})
    for (const child of launchers) if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL")
    for (const pid of shells) if (alive(pid)) { try { process.kill(-pid, "SIGKILL") } catch { try { process.kill(pid, "SIGKILL") } catch {} } }
    for (const item of held) item.response.destroy()
    server.closeAllConnections(); await new Promise<void>(resolve => server.close(() => resolve()))
    launches.unsubscribe(observe); process.env = originalEnv
    await rm(root, { recursive: true, force: true })
  }
})
