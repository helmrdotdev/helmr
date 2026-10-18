import assert from "node:assert/strict"
import { test } from "node:test"
import { createServer, type ServerResponse } from "node:http"
import { once } from "node:events"
import { mkdtemp, mkdir, readFile, rm, stat } from "node:fs/promises"
import { join } from "node:path"
import { tmpdir } from "node:os"
import { createWebsiteAssistant } from "../agents/assistant"
import { nativeOptions } from "../agents/settings"
import type { OpenCodeSession } from "../agents/opencode-bridge"
import { exampleSession, delay, until } from "../../../tests/fixtures/example-session"

test("website example creates and revises files with one-time permissions, questions and retained native history", { timeout: 120_000 }, async () => {
  const root = await mkdtemp(join(tmpdir(), "helmr-website-")), cwd = join(root, "workspace")
  await mkdir(cwd)
  const requests: any[] = [], actions: Array<{ name: string; input: unknown }> = []
  const held: Array<() => void> = []
  let hold = false, modelError = false
  function reply(response: ServerResponse, body: any) {
    response.writeHead(200, { "content-type": "text/event-stream" })
    const send = (type: string, data: object) => response.write(`event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`)
    if (modelError) { send("error", { error: { type: "invalid_request_error", message: "synthetic failure" } }); response.end(); return }
    send("message_start", { message: { id: `msg_${crypto.randomUUID()}`, type: "message", role: "assistant", model: body.model, content: [], stop_reason: null, usage: { input_tokens: 10, output_tokens: 1 } } })
    const action = body.tools?.length ? actions.shift() : undefined
    if (action) {
      assert.ok(body.tools.some((tool: any) => tool.name === action.name), `native tool ${action.name} exists`)
      send("content_block_start", { index: 0, content_block: { type: "tool_use", id: `tool_${crypto.randomUUID()}`, name: action.name, input: {} } })
      send("content_block_delta", { index: 0, delta: { type: "input_json_delta", partial_json: JSON.stringify(action.input) } })
    } else {
      send("content_block_start", { index: 0, content_block: { type: "text", text: "" } })
      send("content_block_delta", { index: 0, delta: { type: "text_delta", text: "Inspect index.html in the workspace." } })
    }
    send("content_block_stop", { index: 0 })
    send("message_delta", { delta: { stop_reason: action ? "tool_use" : "end_turn", stop_sequence: null }, usage: { output_tokens: 2 } })
    send("message_stop", {}); response.end()
  }
  const server = createServer(async (request, response) => {
    let raw = ""; for await (const chunk of request) raw += chunk
    const body = JSON.parse(raw || "{}")
    const path = new URL(request.url ?? "/", "http://fixture").pathname
    if (path.endsWith("/count_tokens")) { response.end(JSON.stringify({ input_tokens: 10 })); return }
    if (!path.endsWith("/messages")) { response.writeHead(404).end(); return }
    requests.push(body)
    if (hold) held.push(() => reply(response, body)); else reply(response, body)
  })
  server.listen(0, "127.0.0.1"); await once(server, "listening")
  const port = (server.address() as { port: number }).port
  const configured = nativeOptions(join(root, "config"))
  // Substitute only the upstream model. Preserve the application's permissions,
  // provider policy, sharing and project-configuration choices.
  const native = { ...configured, config: { ...configured.config, content: JSON.stringify({
    ...JSON.parse(configured.config!.content as string),
    providers: { "opencode-go": { name: "Synthetic model", package: "@opencode/ai/providers/anthropic", settings: { baseURL: `http://127.0.0.1:${port}/v1`, apiKey: "offline" }, models: { "glm-5.3": { name: "Synthetic", limit: { context: 100000, output: 1000 }, capabilities: { tools: true, input: ["text"], output: ["text"] } } } } },
  }) }, models: { fetch: false, snapshot: false }, fs: { filewatcher: false, fff: false }, plugins: [] }
  const application = createWebsiteAssistant({ cwd, stateDirectory: join(root, "state"), native })
  let session = exampleSession(application), bridge: OpenCodeSession | undefined
  const question = { name: "question", input: { questions: [{ question: "What is the website for?", header: "Purpose", options: [{ label: "Community event", description: "An event page" }, { label: "Portfolio", description: "Personal work" }] }] } }
  try {
    bridge = (await session.runtime.initialize()).initResult as OpenCodeSession
    actions.push(question, { name: "shell", input: { command: "printf '<h1>Community event</h1>\n' > index.html", description: "Create the event page" } })
    assert.equal((await session.dispatch("Build a small event website").result).status, "completed")
    assert.match(await readFile(join(cwd, "index.html"), "utf8"), /Community event/)
    assert.ok(session.questions.length >= 2, "clarification and command approval reach Helmr")
    assert.ok(session.questions.every(q => q.type !== "choice" || !q.options.some(o => o.id === "always")), "permissions remain one-time")
    assert.ok(requests.filter(r => r.tools?.length).every(r => !r.tools.some((t: any) => t.name === "subagent")), "the example disables subagents")

    const beforeEdit = session.questions.length
    actions.push({ name: "shell", input: { command: "printf '<p>Saturday, 10am</p>\n' >> index.html", description: "Add the event time" } })
    assert.equal((await session.dispatch("Add the event time: Saturday at 10am").result).status, "completed")
    assert.match(await readFile(join(cwd, "index.html"), "utf8"), /Saturday, 10am/)
    assert.ok(session.questions.length > beforeEdit, "a later operation asks again")
    assert.ok(JSON.stringify(requests.at(-1)).includes("Build a small event website"))

    const answer = session.controls.answer
    session.controls.answer = async q => ({ answer: q.type === "text" ? "no" : { selected: [q.options.find(o => o.id === "reject")!] }, answeredBy: { kind: "user", id: "tester" } })
    actions.push({ name: "shell", input: { command: "touch denied.txt", description: "An operation the human denies" } })
    assert.equal((await session.dispatch("Try a denied operation").result).status, "completed")
    assert.equal(await stat(join(cwd, "denied.txt")).then(() => true, () => false), false)
    session.controls.answer = answer

    hold = true
    const steering = session.dispatch("Wait for a design detail")
    await until(() => held.length > 0)
    await session.runtime.deliverMessage(steering.id, "detail", [{ type: "text", text: "Use blue headings" }], { kind: "api_key" })
    hold = false; held.splice(0).forEach(release => release())
    assert.equal((await steering.result).status, "completed")
    assert.ok(JSON.stringify(requests.at(-1)).includes("Use blue headings"))

    const beforeQuestion = session.questions.length, beforeResponse = session.responses.length
    session.controls.answer = async (_q, signal) => new Promise((_, reject) => { signal.addEventListener("abort", () => reject(signal.reason), { once: true }) })
    actions.push(question)
    const waiting = session.dispatch("Ask before continuing")
    await until(() => session.questions.length > beforeQuestion)
    await session.runtime.interrupt(waiting.id, new Error("cancel question"))
    assert.equal((await waiting.result).status, "interrupted")
    assert.equal(session.responses.length, beforeResponse)
    session.controls.answer = answer
    assert.equal((await session.dispatch("Continue after cancellation").result).status, "completed")

    const responsesBeforeError = session.responses.length
    modelError = true
    assert.equal((await session.dispatch("Fail this synthetic request").result).status, "failed")
    assert.equal(session.responses.length, responsesBeforeError)
    modelError = false
    await bridge.close()
    const count = requests.length
    session = exampleSession(application, "reconstructed")
    bridge = (await session.runtime.initialize()).initResult as OpenCodeSession
    await delay(50)
    assert.equal(requests.length, count, "reconstruction does not replay old input")
    assert.equal((await session.dispatch("Continue editing the event page").result).status, "completed")
    assert.ok(JSON.stringify(requests.at(-1)).includes("Build a small event website"))
    await bridge.close(); bridge = undefined
    await rm(join(root, "state", "example", "session.sqlite"))
    await assert.rejects(exampleSession(application, "reconstructed").runtime.initialize(), /ENOENT/)
  } finally {
    await bridge?.close()
    server.closeAllConnections(); await new Promise<void>(resolve => server.close(() => resolve()))
    await rm(root, { recursive: true, force: true })
  }
})
