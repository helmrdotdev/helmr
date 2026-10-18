import assert from "node:assert/strict"
import { createServer } from "node:http"
import { once } from "node:events"
import { execFileSync, spawnSync, type ChildProcess } from "node:child_process"
import { channel } from "node:diagnostics_channel"
import { test } from "node:test"
import { mkdtemp, mkdir, writeFile, readFile, rm } from "node:fs/promises"
import { existsSync } from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"
import { CodexStdio, spawnCodex } from "../agents/codex-stdio"
import { CodexSession, nativeHome } from "../agents/codex-bridge"
import { SessionRuntime, type SessionDriver } from "../../../runtime/typescript/src/agent-session"
import type { InitContext, Turn } from "../../../sdk/typescript/src/agent"

const quote = (value: string) => "'" + value.replaceAll("'", "'\"'\"'") + "'"
function lifetimeCommand(marker: string, body: string): string {
  // The Linux native sandbox has its own PID namespace. A file lock measures
  // command lifetime across that boundary without mistaking its PID for ours.
  return process.platform === "linux" ? `flock --exclusive ${quote(marker + ".lock")} /bin/sh -c ${quote(body)}` : body
}
function commandRunning(marker: string, pid: number): boolean {
  if (process.platform === "linux") {
    assert.ok(existsSync(marker + ".lock"), "native command acquired its lifetime lock")
    const result = spawnSync("flock", ["--nonblock", "--exclusive", marker + ".lock", "/bin/true"])
    assert.ok(result.status === 0 || result.status === 1, "valid lifetime lock observation")
    return result.status === 1
  }
  try { process.kill(pid, 0); return true }
  catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ESRCH") return false
    throw error
  }
}
const delay = (ms: number) => new Promise(resolve => setTimeout(resolve, ms))
async function until(check: () => boolean, label: string) {
  for (let n = 0; n < 1000; n++) { if (check()) return; await delay(20) }
  throw new Error(`Timed out: ${label}`)
}
const message = (text = "offline reply", phase?: string) => ({ ...(phase ? { phase } : {}), type: "message", role: "assistant", id: `msg_${crypto.randomUUID()}`, content: [{ type: "output_text", text }] })
const tool = (name: string, args: unknown) => ({ type: "function_call", call_id: `call_${crypto.randomUUID()}`, name, arguments: JSON.stringify(args) })
const text = (value: string) => [{ type: "text" as const, text: value }]

// Only the model transport and Helmr's external driver are substituted. The
// generated bridge, native executable and Helmr SessionRuntime are real.
test("Codex starter uses actual Helmr Turns for continuation, steering and cancellation", { timeout: 120_000 }, async () => {
  const root = await mkdtemp(join(homedir(), ".helmr-codex-starter-"))
  const cwd = join(root, "workspace"), stateDirectory = join(root, "state")
  await mkdir(cwd)
  const requests: any[] = [], held: any[] = [], actions: any[] = [], output: unknown[] = [], questions: unknown[] = [], holds: string[] = []
  let childScenario: { marker: string; action: unknown; rootCalls: number; childCalls: number; rootSend?: (item: any) => void } | undefined
  const server = createServer(async (request, response) => {
    if (!request.url?.endsWith("/responses")) { response.end(JSON.stringify({ data: [] })); return }
    let payload = ""
    for await (const chunk of request) payload += chunk
    const body = JSON.parse(payload)
    requests.push(body)
    response.writeHead(200, { "content-type": "text/event-stream" })
    const send = (item: any) => {
      const id = `resp_${crypto.randomUUID()}`
      const items = item ? (Array.isArray(item) ? item : [item]) : []
      const events = [{ type: "response.created", response: { id } }, ...items.map(item => ({ type: "response.output_item.done", item })), { type: "response.completed", response: { id, output: items, usage: { input_tokens: 10, output_tokens: 2, total_tokens: 12 } } }]
      for (const event of events) response.write(`data: ${JSON.stringify(event)}\n\n`)
      response.end()
    }
    if (body.generate !== false && childScenario) {
      const scenario = childScenario
      const child = body.input.some((item: any) => item.role === "user" && item.content?.some((part: any) => part.text === scenario.marker))
      if (child) { send(++scenario.childCalls === 1 ? scenario.action : message("child-only response", "final_answer")); return }
      if (++scenario.rootCalls === 1) send({ ...tool("spawn_agent", { message: scenario.marker }), namespace: "multi_agent_v1" })
      else if (scenario.rootCalls === 2) scenario.rootSend = send
      else send(message("root-only response", "final_answer"))
      return
    }
    const action = body.generate === false ? null : actions.shift() ?? message()
    if (action === "hold") held.push({ body, send })
    else send(action)
  })
  server.listen(0, "127.0.0.1")
  await once(server, "listening")
  const port = (server.address() as import("node:net").AddressInfo).port
  const launchers: ChildProcess[] = [], sent: any[] = []
  const launches = channel("child_process")
  const observe = (value: unknown) => {
    const child = (value as { process: ChildProcess }).process
    child.once("spawn", () => {
      if (!child.spawnargs.some(arg => arg.endsWith("/bin/codex.js"))) return
      launchers.push(child)
      const write = child.stdin!.write.bind(child.stdin) as (...args: any[]) => boolean
      child.stdin!.write = ((chunk: any, ...args: any[]) => {
        sent.push(JSON.parse(String(chunk)))
        return write(chunk, ...args)
      }) as NonNullable<ChildProcess["stdin"]>["write"]
    })
  }
  launches.subscribe(observe)
  const sessionId = "starter-test"
  const home = nativeHome(stateDirectory, sessionId)
  await mkdir(home, { recursive: true })
  await writeFile(join(home, "config.toml"), `model_provider = "fixture"\nmodel_reasoning_effort = "low"\n[model_providers.fixture]\nname = "Local fixture"\nbase_url = "http://127.0.0.1:${port}/v1"\nwire_api = "responses"\nrequires_openai_auth = false\nsupports_websockets = false\n[features]\ngoals = true\n`)
  const options = { cwd, stateDirectory, sessionId, model: "gpt-5.1-codex", instructions: "You are a local test agent.", recovery: "initial" as "initial" | "reconstructed" }
  let bridge = new CodexSession(options), initCount = 0, sequence = 0
  const context: InitContext = { session: { id: sessionId }, computer: { id: "computer" }, deployment: { id: "deployment" }, recovery: { kind: "initial" }, signal: new AbortController().signal, mcp: { getConnectionOptions: async () => { throw Error("unused") } } }
  let responseGate: Promise<void> | undefined, responseEntered = false
  let answerQuestion = true, allowApproval = true
  const driver: SessionDriver = {
    registerMessages: async () => {}, closeProcessing: async () => {},
    ask: async (_turn, _id, question, signal) => {
      questions.push(question)
      const answer = answerQuestion
        ? Promise.resolve({ answer: { selected: [question.type === "choice" && question.options.some(option => option.id === "allow") ? question.options.find(option => option.id === (allowApproval ? "allow" : "deny"))! : { id: "option-0", label: "A", value: "A" }] }, answeredBy: { kind: "user" as const, id: "tester" } })
        : new Promise<never>((_, reject) => { signal.addEventListener("abort", () => reject(signal.reason), { once: true }) })
      return { answer, withdraw: async () => {} }
    },
    respond: async (_turn, _id, content) => { output.push(content); responseEntered = true; await responseGate },
    output: async (_turn, _id, content) => { output.push(content); return { sequence: output.length } },
    confirmDrain: async () => {}, finalize: async (_id, result) => ({ status: "completed", result }),
    fail: async (_id, error) => { if (error.code !== "interrupted") console.error("TURN_FAILED", error); return error.code === "interrupted" ? { status: "interrupted" } : { status: "failed", error } },
    hold: async reason => { holds.push(reason) },
  }
  const createRuntime = () => new SessionRuntime({ kind: "agent", id: "test", template: undefined!, init: () => { initCount++; return bridge }, turn: async (turn: Turn, { initResult }) => {
    const { response, ...result } = await (initResult as CodexSession).run(turn)
    if (response !== undefined) await turn.respond(response)
    return result
  } }, { ...context, recovery: { kind: options.recovery } }, driver)
  let runtime = createRuntime()
  const dispatch = (input: string) => { const n = ++sequence; return { id: `turn-${n}`, result: runtime.dispatch({ id: `turn-${n}`, sequence: n, createdAt: new Date().toISOString(), input: text(input), source: { kind: "api_key" } }) } }
  const calls = () => requests.filter(item => item.generate !== false)
  try {
    const first = await dispatch("remember first input").result
    assert.equal(first.status, "completed")
    assert.equal((await dispatch("second input").result).status, "completed")
    assert.equal(initCount, 1)
    assert.equal(launchers.length, 1)
    assert.ok(JSON.stringify(calls().at(-1).input).includes("remember first input"))
    assert.ok(!calls().at(-1).tools.some((item: any) => ["create_goal", "update_goal", "get_goal"].includes(item.name)))

    const nativeConfig = join(home, "config.toml")
    const savedConfig = await readFile(nativeConfig, "utf8")
    allowApproval = false
    actions.push(tool("exec_command", { cmd: `printf tampered >> '${nativeConfig}'`, login: false }))
    assert.equal((await dispatch("attempt unapproved native configuration write").result).status, "completed")
    assert.equal(await readFile(nativeConfig, "utf8"), savedConfig)
    allowApproval = true

    for (const phase of ["final_answer", undefined]) {
      const heldIndex = held.length
      actions.push("hold", message("latest answer", phase))
      const steering = dispatch("wait for steering")
      await until(() => held.length > heldIndex, "held steering model")
      await runtime.deliverMessage(steering.id, "message-1", text("steered marker"), { kind: "api_key" })
      held[heldIndex].send(message("first completed answer", "final_answer"))
      assert.equal((await steering.result).status, "completed")
      assert.ok(JSON.stringify(calls().at(-1).input).includes("steered marker"))
      await assert.rejects(runtime.deliverMessage(steering.id, "stale", text("stale marker"), { kind: "api_key" }))
    }

    for (const phase of ["final_answer", undefined]) {
      const outputStart = output.length
      actions.push([message("first completed answer", "final_answer"), message("latest answer", phase)])
      assert.equal((await dispatch("ordered completed output").result).status, "completed")
      assert.deepEqual(output.slice(outputStart).map(content => (content as { text: string }[]).map(part => part.text).join("")), ["first completed answer", "latest answer"])
    }

    actions.push("hold")
    const cancelled = dispatch("cancel held response")
    await until(() => held.length === 3, "held cancellable model")
    await runtime.interrupt(cancelled.id, new Error("stop"))
    assert.equal((await cancelled.result).status, "interrupted")
    assert.ok(launchers[0].exitCode !== null || launchers[0].signalCode !== null, "cancel joins native owner exit")
    const count = calls().length
    await delay(100)
    assert.equal(calls().length, count)
    const savedIdentity = await readFile(join(home, "conversation.json"), "utf8")
    await rm(join(home, "conversation.json"))
    assert.equal((await dispatch("missing identity after cancellation").result).status, "failed")
    assert.equal(calls().length, count)
    assert.equal(existsSync(join(home, "conversation.json")), false)
    await writeFile(join(home, "conversation.json"), savedIdentity)
    assert.equal((await dispatch("fresh after cancel").result).status, "completed")
    assert.equal(initCount, 1)
    assert.ok(JSON.stringify(calls().at(-1).input).includes("remember first input"))

    const questionsBefore = questions.length
    actions.push(tool("request_user_input", { questions: [{ id: "choice", header: "Pick", question: "Which?", options: [{ label: "A", description: "First" }, { label: "B", description: "Second" }] }] }))
    assert.equal((await dispatch("ask a question").result).status, "completed")
    assert.equal(questions.length, questionsBefore + 1)
    assert.ok(calls().at(-1).input.some((item: any) => item.type === 'function_call_output' && item.output.startsWith('{') && JSON.parse(item.output).answers?.choice?.answers?.includes('A')))
    answerQuestion = false
    actions.push(tool("request_user_input", { questions: [{ id: "choice", header: "Pick", question: "Cancel?", options: [{ label: "A", description: "First" }, { label: "B", description: "Second" }] }] }))
    const question = dispatch("cancel question")
    await until(() => questions.length === questionsBefore + 2, "pending Helmr ask")
    await runtime.interrupt(question.id, new Error("stop question"))
    assert.equal((await question.result).status, "interrupted")
    answerQuestion = true

    for (const allow of [false, true]) {
      allowApproval = allow
      const marker = join(cwd, `approval-${allow}`)
      const approvalCount: number = questions.length
      const ownerCount: number = launchers.length, outputCount: number = output.length
      actions.push(tool("exec_command", { cmd: `echo approved > ${marker}`, login: false, sandbox_permissions: "require_escalated", justification: "Test one-time approval" }))
      assert.equal((await dispatch(`approval ${allow}`).result).status, "completed")
      assert.equal(questions.length, approvalCount + 1)
      assert.equal(existsSync(marker), allow)
      if (!allow) {
        assert.ok(JSON.stringify(output.slice(outputCount)).includes("The operation was not approved."))
        assert.ok(launchers.at(-1)!.exitCode !== null || launchers.at(-1)!.signalCode !== null)
      } else assert.equal(launchers.length, ownerCount + 1)
    }

    const backgroundPID = join(cwd, "completed-background-pid")
    const completedBackground = join(cwd, "completed-background")
    const completedHeld = held.length
    actions.push(tool("exec_command", { cmd: lifetimeCommand(backgroundPID, `echo $$ > ${backgroundPID}; sleep 30; echo done > ${completedBackground}`), login: false, yield_time_ms: 10 }), "hold")
    const backgroundCompletion = dispatch("start a Session background command")
    await until(() => held.length > completedHeld, "normal command yielded")
    assert.ok(existsSync(backgroundPID), `background command did not start: ${JSON.stringify(calls().at(-1).input.filter((item: any) => item.type === "function_call_output"))}`)
    held[completedHeld].send(message("background response"))
    assert.equal((await backgroundCompletion.result).status, "completed")
    assert.equal(existsSync(completedBackground), false)
    const backgroundOwner = Number(await readFile(backgroundPID, "utf8"))
    const backgroundAlive = () => commandRunning(backgroundPID, backgroundOwner)
    assert.equal(backgroundAlive(), true)
    assert.equal((await dispatch("next Turn while background runs").result).status, "completed")
    assert.equal(backgroundAlive(), true)
    const cancelHeld = held.length
    actions.push("hold")
    const cancelBackground = dispatch("cancel with an earlier background command")
    await until(() => held.length > cancelHeld, "later Turn admitted")
    await runtime.interrupt(cancelBackground.id, new Error("stop retained background"))
    assert.equal((await cancelBackground.result).status, "interrupted")
    assert.equal(backgroundAlive(), false)
    assert.equal(existsSync(completedBackground), false)

    const natural = join(cwd, "natural-background")
    actions.push(tool("exec_command", { cmd: `sleep 1; echo done > ${natural}`, login: false, yield_time_ms: 10 }))
    assert.equal((await dispatch("let a background command finish naturally").result).status, "completed")
    const outputAfterNaturalTurn = output.length
    await until(() => existsSync(natural), "background finished after its Turn")
    assert.equal(output.length, outputAfterNaturalTurn, "late command receipts cannot write to a closed Turn")

    const finalizingPID = join(cwd, "finalizing-pid")
    actions.push(tool("exec_command", { cmd: lifetimeCommand(finalizingPID, `echo $$ > ${finalizingPID}; sleep 30`), login: false, yield_time_ms: 10 }))
    let releaseResponse!: () => void
    responseGate = new Promise<void>(resolve => { releaseResponse = resolve })
    responseEntered = false
    const finalizing = dispatch("cancel after the native Turn completes")
    await until(() => responseEntered, "response publication reached after bridge completion")
    const finalizingOwner = Number(await readFile(finalizingPID, "utf8"))
    const stopping = runtime.interrupt(finalizing.id, new Error("cancel during response"))
    releaseResponse()
    await stopping
    responseGate = undefined
    assert.equal((await finalizing.result).status, "interrupted")
    assert.equal(commandRunning(finalizingPID, finalizingOwner), false, "finalization cancellation stopped background command")

    for (const background of [false, true]) {
      const marker = join(cwd, `pid-${background}`), finished = join(cwd, `finished-${background}`)
      actions.push(tool("exec_command", { cmd: lifetimeCommand(marker, `echo $$ > ${marker}; sleep 30; echo late > ${finished}`), login: false, yield_time_ms: background ? 10 : 10000 }), "hold")
      const previousHeld = held.length
      const execution = dispatch(`cancel command ${background}`)
      await until(() => existsSync(marker), "actual command started")
      if (background) await until(() => held.length > previousHeld, "command yielded and next model request started")
      const pid = Number(await readFile(marker, "utf8"))
      const alive = () => commandRunning(marker, pid)
      assert.equal(alive(), true)
      await runtime.interrupt(execution.id, new Error("stop command"))
      assert.equal((await execution.result).status, "interrupted")
      await until(() => !alive(), "actual command stopped")
      assert.equal(existsSync(finished), false)
      actions.length = 0
      assert.equal((await dispatch("fresh after command cleanup").result).status, "completed")
    }
    await bridge.close()
    const { id: savedThread } = JSON.parse(await readFile(join(home, "conversation.json"), "utf8"))
    const inspector = new CodexStdio(() => {}, spawnCodex(home))
    try {
      await inspector.request("initialize", { clientInfo: { name: "fixture", version: "1" }, capabilities: { experimentalApi: true } })
      await inspector.send({ method: "initialized" })
      await inspector.request("thread/read", { threadId: savedThread, includeTurns: false })
      for (let n = 0; n < 100; n++) await inspector.request("thread/queue/add", { threadId: savedThread, clientUserMessageId: crypto.randomUUID(), input: text(`cancelled queued marker ${n}`) })
    } finally { await inspector.close() }
    const beforeReopen = calls().length
    options.recovery = "reconstructed"
    bridge = new CodexSession(options)
    runtime = createRuntime()
    assert.equal((await dispatch("fresh reconstructed session").result).status, "completed")
    assert.equal(calls().length, beforeReopen + 1)
    assert.ok(!JSON.stringify(calls().at(-1).input).includes("cancelled queued marker"))
    assert.ok(JSON.stringify(calls().at(-1).input).includes("remember first input"))
    assert.deepEqual(holds, [])
    for (const mode of ["approve", "decline", "question", "parent-completes", "cancel", "background"] as const) {
      const marker = `native child ${mode} ${crypto.randomUUID()}`
      const file = join(mode === "background" ? cwd : root, `child-${mode}`)
      const approval = mode !== "question" && mode !== "background"
      allowApproval = mode !== "decline"
      answerQuestion = mode !== "parent-completes" && mode !== "cancel"
      childScenario = { marker, rootCalls: 0, childCalls: 0, action: mode === "background"
        ? tool("exec_command", { cmd: lifetimeCommand(file, `echo $$ > '${file}'; sleep 60`), login: false, yield_time_ms: 10 })
        : approval
        ? tool("exec_command", { cmd: `echo child > '${file}'`, login: false, sandbox_permissions: "require_escalated", justification: "Fixture child approval" })
        : tool("request_user_input", { questions: [{ id: "child-choice", header: "Child", question: "Child choice?", options: [{ label: "A", description: "First" }, { label: "B", description: "Second" }] }] }) }
      const scenario = childScenario, questionCount = questions.length, outputStart = output.length, sentStart = sent.length
      const running = dispatch(`start child ${mode}`)
      let settled = false
      void running.result.then(() => { settled = true })
      await until(() => (mode === "question" || mode === "background" ? scenario.childCalls >= 2 : questions.length > questionCount) && !!scenario.rootSend, `native child ${mode} request`).catch(error => {
        console.error("CHILD_FIXTURE", JSON.stringify({ mode, rootCalls: scenario.rootCalls, childCalls: scenario.childCalls,
          outputs: calls().filter(body => body.input.some((item: any) => item.role === "user" && item.content?.some((part: any) => part.text === marker))).flatMap(body => body.input.filter((item: any) => item.type === "function_call_output")) }))
        throw error
      })
      if (mode === "cancel") {
        await runtime.interrupt(running.id, new Error("cancel child question"))
        assert.equal((await running.result).status, "interrupted")
        assert.ok(launchers.at(-1)!.exitCode !== null || launchers.at(-1)!.signalCode !== null)
      } else {
        if (mode !== "parent-completes" && mode !== "decline") {
          await until(() => scenario.childCalls >= 2, `native child ${mode} continuation`)
          assert.equal(settled, false, "child completion must not settle its parent's Helmr Turn")
        }
        scenario.rootSend!(message("root-only response", "final_answer"))
        assert.equal((await running.result).status, "completed")
        if (mode === "parent-completes") assert.ok(sent.slice(sentStart).some(item => item.error?.message === "Helmr Turn is closed"), "pending child receives explicit rejection")
        const published = output.slice(outputStart).map(content => (content as { text: string }[]).map(part => part.text).join(""))
        assert.ok(published.length > 0)
        assert.ok(published.every(value => value.trim() === "root-only response"), "only root output is published")
      }
      if (approval) assert.equal(existsSync(file), mode === "approve")
      else if (mode === "question") {
        assert.equal(questions.length, questionCount, "native child question restriction is preserved")
        assert.ok(calls().some(body => body.input.some((item: any) => item.type === "function_call_output" && String(item.output).includes("request_user_input can only be used by the root thread"))))
      }
      childScenario = undefined; answerQuestion = true; allowApproval = true
      assert.equal((await dispatch(`fresh after child ${mode}`).result).status, "completed")
      if (mode === "background") {
        const pid = Number(await readFile(file, "utf8"))
        assert.equal(commandRunning(file, pid), true)
        const heldBefore = held.length, cleanupStart = sent.length
        actions.push("hold")
        const stop = dispatch("cancel retained child background")
        await until(() => held.length > heldBefore, "root model before child cleanup")
        await runtime.interrupt(stop.id, new Error("stop native child work"))
        assert.equal((await stop.result).status, "interrupted")
        assert.ok(launchers.at(-1)!.exitCode !== null || launchers.at(-1)!.signalCode !== null)
        const cleanedThreads = new Set(sent.slice(cleanupStart).filter(item => item.method === "thread/backgroundTerminals/clean").map(item => item.params.threadId))
        assert.ok(cleanedThreads.size >= 2, "native cleanup covers root and known child command threads")
        console.log(JSON.stringify({ case: "native-child-background-observation", platform: process.platform, running: commandRunning(file, pid) }))
        assert.equal((await dispatch("fresh after child background cancellation").result).status, "completed")
      }
      console.log(`PASS native child ${mode}`)
    }
    // A real owner failure must poison this initResult and hold the Helmr Session.
    actions.push("hold")
    const heldCount = held.length
    const failed = dispatch("kill native owner")
    await until(() => held.length > heldCount, "model before owner failure")
    const processRows = execFileSync("ps", ["-eo", "pid=,ppid=,args="], { encoding: "utf8" }).trim().split("\n").map(line => {
      const match = line.trim().match(/^(\d+)\s+(\d+)\s+(.*)$/)!
      return { pid: Number(match[1]), ppid: Number(match[2]), command: match[3] }
    })
    const launcher = processRows.find(row => row.ppid === process.pid && row.command.includes("codex.js"))
    assert.ok(launcher, "owned launcher exists")
    const native = processRows.find(row => row.ppid === launcher.pid && row.command.includes("app-server"))
    assert.ok(native, "owned native process exists")
    process.kill(native.pid, "SIGKILL")
    await failed.result
    assert.ok(holds.length > 0)
    assert.equal(runtime.idle, false)
    await assert.rejects(async () => dispatch("must not reuse failed owner").result)

    await bridge.close()
    options.recovery = "reconstructed"
    bridge = new CodexSession(options)
    runtime = createRuntime()
    const callsBeforeRecovery = calls().length
    assert.equal((await dispatch("fresh after killed owner").result).status, "completed")
    assert.equal(calls().length, callsBeforeRecovery + 1)
    assert.ok(JSON.stringify(calls().at(-1).input).includes("remember first input"))
    await bridge.close()
    const missingId = crypto.randomUUID(), callsBeforeMissing = calls().length
    await writeFile(join(home, "conversation.json"), JSON.stringify({ id: missingId }))
    options.recovery = "reconstructed"
    bridge = new CodexSession(options)
    runtime = createRuntime()
    assert.equal((await dispatch("missing saved history").result).status, "failed")
    assert.equal(calls().length, callsBeforeMissing)
    assert.equal(JSON.parse(await readFile(join(home, "conversation.json"), "utf8")).id, missingId)
    await bridge.close()
    await rm(join(home, "conversation.json"))
    bridge = new CodexSession(options)
    runtime = createRuntime()
    assert.equal((await dispatch("missing identity on reconstruction").result).status, "failed")
    assert.equal(calls().length, callsBeforeMissing)
    assert.equal(existsSync(join(home, "conversation.json")), false)
    console.log(JSON.stringify({ case: "codex-starter-real-runtime", retained: true, steering: true, cancellation: true, questions: true, approvals: true, ownerFailureHoldsSession: true, foregroundAndBackgroundStopped: true, reconstruction: true, queuedInputsRetired: 100, missingHistoryRejected: true, modelCalls: calls().length }))
  } finally { await bridge.close(); launches.unsubscribe(observe); server.closeAllConnections(); server.close(); await rm(root, { recursive: true, force: true }) }
})
