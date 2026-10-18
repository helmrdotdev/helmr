// Execute the editable embedded SDK bridges unchanged against a local model.
import { agent, type TemplateDefinition, type JsonValue } from "../../../sdk/typescript/src/index"
import { createServer, type ServerResponse } from "node:http"
import { once } from "node:events"
import { mkdir, readFile, writeFile } from "node:fs/promises"
import { existsSync } from "node:fs"
import { join } from "node:path"
import { ModelRuntime, SettingsManager, DefaultResourceLoader } from "@earendil-works/pi-coding-agent"
import { OpenCodeSession } from "../../../starters/opencode/agents/opencode-bridge"
import { PiSession } from "../../../starters/pi/agents/pi-bridge"

export function createEmbeddedStarter(kind: "opencode" | "pi", template: TemplateDefinition) { return agent({
  id: `${kind}-starter`, template,
  async init({ session, recovery }) {
    const cwd = `/workspace/${kind}-starter/${session.id}`
    await mkdir(cwd, { recursive: true })
    let setups = 0
    try { setups = Number(await readFile(join(cwd, "setups"), "utf8")) }
    catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error }
    await writeFile(join(cwd, "setups"), String(++setups))
    const requests: any[] = [], actions: any[] = [], held: Array<() => void> = []
    const toolRunning = async () => {
      try { const pid = Number(await readFile(join(cwd, "tool.pid"), "utf8")); process.kill(pid, 0); return true }
      catch (error) { if (["ENOENT", "ESRCH"].includes((error as NodeJS.ErrnoException).code ?? "")) return false; throw error }
    }
    const admissions: Array<{ toolRunning: boolean; toolFinished: boolean }> = []
    function reply(response: ServerResponse, action: any) {
      response.writeHead(200, { "content-type": "text/event-stream" })
      const send = (type: string, data: object) => response.write(`event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`)
      send("message_start", { message: { id: `msg_${crypto.randomUUID()}`, type: "message", role: "assistant", model: "probe", content: [], stop_reason: null, usage: { input_tokens: 10, output_tokens: 1 } } })
      if (action) {
        send("content_block_start", { index: 0, content_block: { type: "tool_use", id: `tool_${crypto.randomUUID()}`, name: action.name, input: {} } })
        send("content_block_delta", { index: 0, delta: { type: "input_json_delta", partial_json: JSON.stringify(action.input) } })
      } else {
        send("content_block_start", { index: 0, content_block: { type: "text", text: "" } })
        send("content_block_delta", { index: 0, delta: { type: "text_delta", text: "offline starter reply" } })
      }
      send("content_block_stop", { index: 0 })
      send("message_delta", { delta: { stop_reason: action ? "tool_use" : "end_turn", stop_sequence: null }, usage: { output_tokens: 2 } })
      send("message_stop", {}); response.end()
    }
    const server = createServer(async (request, response) => {
      try {
        const pathname = new URL(request.url ?? "/", "http://fixture").pathname
        const admission = { toolRunning: await toolRunning(), toolFinished: existsSync(join(cwd, "tool.finished")) }
        let raw = ""
        for await (const chunk of request) raw += chunk
        const body = JSON.parse(raw || "{}")
        if (pathname === "/control") {
          actions.push(...(body.actions ?? []))
          if (body.release) held.shift()?.()
          response.setHeader("content-type", "application/json")
          response.end(JSON.stringify({ calls: requests.length, inputs: requests.map(item => item.messages), held: held.length, admissions, ...admission, setups, pid: process.pid }))
          if (body.crash) setImmediate(() => process.kill(process.pid, "SIGKILL"))
          return
        }
        if (pathname.endsWith("/count_tokens")) { response.end(JSON.stringify({ input_tokens: 10 })); return }
        if (!pathname.endsWith("/messages")) { response.writeHead(404); response.end(); return }
        requests.push(body); admissions.push(admission)
        const action = actions.shift()
        if (action === "hold") held.push(() => reply(response, undefined))
        else reply(response, action)
      } catch (error) { response.statusCode = 500; response.end(String(error)) }
    })
    server.listen(0, "127.0.0.1"); await once(server, "listening")
    const port = (server.address() as { port: number }).port
    await writeFile(join(cwd, "control.json"), JSON.stringify({ port }))
    const common = { cwd, stateDirectory: `/var/lib/helmr/${kind}-starter`, sessionId: session.id, model: "fixture/probe", introduction: "You are a local test agent.", recovery: recovery.kind }
    let bridge: OpenCodeSession | PiSession
    if (kind === "opencode") {
      bridge = await OpenCodeSession.open({ ...common, native: {
        config: { directory: join(cwd, "config"), project: false, claude: false, content: JSON.stringify({ permissions: [{ action: "*", resource: "*", effect: "allow" }], providers: { fixture: { name: "Offline model", package: "@opencode/ai/providers/anthropic", settings: { baseURL: `http://127.0.0.1:${port}/v1`, apiKey: "local-fixture" }, models: { probe: { name: "Probe", limit: { context: 100000, output: 1000 }, capabilities: { tools: true, input: ["text"], output: ["text"] } } } } } }) },
        models: { fetch: false, snapshot: false }, fs: { filewatcher: false, fff: false }, plugins: [],
      } })
    } else {
      const agentDir = join(cwd, "config")
      const modelRuntime = await ModelRuntime.create({ authPath: join(agentDir, "auth.json"), modelsPath: null, modelsStorePath: join(agentDir, "models.json"), allowModelNetwork: false, refreshOnCreate: false })
      modelRuntime.registerProvider("fixture", { baseUrl: `http://127.0.0.1:${port}`, api: "anthropic-messages", apiKey: "local-fixture", models: [{ id: "probe", name: "Probe", reasoning: false, input: ["text"], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 100000, maxTokens: 1000 }] })
      const settingsManager = SettingsManager.inMemory({ compaction: { enabled: false }, retry: { enabled: false }, cacheWarming: "off" })
      const resourceLoader = new DefaultResourceLoader({ cwd, agentDir, settingsManager, noExtensions: true, noSkills: true, noThemes: true, noContextFiles: true, appendSystemPrompt: [common.introduction], extensionFactories: [pi => {
        pi.registerCommand("question", { description: "Ask user", handler: async (_, ctx) => { await ctx.ui.confirm("Continue?", "Native extension request") } })
      }] })
      await resourceLoader.reload()
      bridge = await PiSession.open({ ...common, native: { modelRuntime, settingsManager, resourceLoader, agentDir } })
    }
    return { bridge, setups, nonce: crypto.randomUUID() }
  },
  async turn(turn, { initResult }) {
    const { response, ...native } = await initResult.bridge.run(turn)
    if (response !== undefined) await turn.respond(response)
    return { ...native, setups: initResult.setups, nonce: initResult.nonce, ownerPid: process.pid } as JsonValue
  },
}) }
