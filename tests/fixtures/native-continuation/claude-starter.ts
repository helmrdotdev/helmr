// The editable starter bridge runs unchanged; this fixture owns only the local
// model, fault injection and observations around the real Helmr Agent.
import { agent, type JsonValue, type TemplateDefinition } from "../../../sdk/typescript/src/index"
import { mkdir, readFile, writeFile } from "node:fs/promises"
import { existsSync, readdirSync, readFileSync, readlinkSync } from "node:fs"
import { createServer } from "node:http"
import { once } from "node:events"
import { channel } from "node:diagnostics_channel"
import { spawn, spawnSync, type ChildProcess } from "node:child_process"
import { join } from "node:path"
import { ClaudeSession, nativeHome } from "../../../starters/claude-code/agents/claude-bridge"

export function createClaudeStarter(template: TemplateDefinition) { return agent({
  id: "claude-starter", template,
  async init({ session, recovery }) {
    const cwd = `/workspace/claude-starter/${session.id}`
    const home = nativeHome({ stateDirectory: "/var/lib/helmr/claude-starter", sessionId: session.id })
    await mkdir(cwd, { recursive: true })
    await mkdir(home, { recursive: true })
    let setups = 0
    try { setups = Number(await readFile(join(cwd, "setups"), "utf8")) }
    catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error }
    await writeFile(join(cwd, "setups"), String(++setups))
    let toolCase = await readFile(join(cwd, "tool-case"), "utf8").catch(error => { if (error.code === "ENOENT") return "retained"; throw error })
    const toolFile = (suffix: string) => join(cwd, `tool-${toolCase}.${suffix}`)
    const requests: any[] = [], actions: any[] = [], held: { send: (item: unknown) => void; input: unknown }[] = [], launchers: ChildProcess[] = []
    let auxiliaryCalls = 0, lastAuxiliaryInput: unknown = null
    const launches = channel("child_process")
    launches.subscribe(value => {
      const child = (value as { process: ChildProcess }).process
      child.once("spawn", () => { if (child.spawnargs.includes("--input-format")) launchers.push(child) })
    })
    const processes = () => readdirSync("/proc").filter(name => /^\d+$/.test(name)).flatMap(pid => {
      try {
        const stat = readFileSync(`/proc/${pid}/stat`, "utf8")
        const command = readFileSync(`/proc/${pid}/cmdline`, "utf8").replaceAll("\0", " ")
        const signals = readFileSync(`/proc/${pid}/status`, "utf8").split("\n").filter(line => /^(Sig|Uid:|Gid:)/.test(line))
        const locks = readdirSync(`/proc/${pid}/fd`).flatMap(fd => {
          try { return readlinkSync(`/proc/${pid}/fd/${fd}`) === toolFile("lock") ? [fd] : [] }
          catch { return [] }
        })
        return [{ pid: Number(pid), stat, command, signals, locks }]
      } catch { return [] }
    })
    const observeNative = () => {
      const lock = spawnSync("/usr/bin/flock", ["--nonblock", "--exclusive", toolFile("lock"), "/bin/true"], { timeout: 1000 })
      if (lock.error || lock.signal || (lock.status !== 0 && lock.status !== 1)) throw new Error(`Invalid lifetime lock observation: ${JSON.stringify({ status: lock.status, signal: lock.signal, error: lock.error?.message })}`)
      return { toolCase, owners: launchers.map(child => ({ pid: child.pid, exited: child.exitCode !== null || child.signalCode !== null })),
        toolRunning: lock.status === 1, toolFinished: existsSync(toolFile("finished")), processes: processes() }
    }
    const admissions: ReturnType<typeof observeNative>[] = []
    const message = () => ({ type: "text", text: "offline starter reply" })
    const server = createServer(async (request, response) => {
      try {
        // Observe at request entry, before body I/O can give old cleanup time to finish.
        const admission = request.url?.includes("/messages") && !request.url.includes("count_tokens") ? observeNative() : undefined
        let raw = ""
        for await (const chunk of request) raw += chunk
        const body = raw ? JSON.parse(raw) : {}
        if (request.url === "/control") {
          const orphanPids: number[] = []
          if (body.reapProbe) {
            for (let index = 0; index < 8; index++) {
              const parent = spawn(process.execPath, ["-e", `const {spawn}=require("node:child_process"); const child=spawn(process.execPath,["-e","setTimeout(()=>{},100)"],{stdio:"ignore"}); process.stdout.write(String(child.pid),()=>process.exit(0))`], { stdio: ["ignore", "pipe", "pipe"] })
              let output = ""
              parent.stdout.on("data", chunk => { output += chunk })
              const [code] = await once(parent, "close")
              const pid = Number(output.trim())
              if (code !== 0 || !Number.isInteger(pid) || pid <= 1) throw new Error("Orphan probe failed")
              orphanPids.push(pid)
            }
          }
          if (body.toolCase !== undefined) {
            if (!["retained", "cancelled", "lost"].includes(body.toolCase)) throw new Error("Unknown fixture tool case")
            toolCase = body.toolCase
            await writeFile(join(cwd, "tool-case"), toolCase)
          }
          actions.push(...(body.actions ?? []))
          if (body.release) held.shift()?.send(message())
          if (body.crash) {
            const pid = launchers.at(-1)?.pid
            if (!pid) throw new Error("No owned native launcher")
            process.kill(pid, "SIGKILL")
          }
          response.setHeader("content-type", "application/json")
          response.end(JSON.stringify({ orphanPids, setups, held: held.length, heldInputs: held.map(item => item.input), auxiliaryCalls, lastAuxiliaryInput, calls: requests.length, inputs: requests.map(item => item.messages), admissions, ...observeNative(),
            toolFinished: await readFile(toolFile("finished"), "utf8").catch(error => { if (error.code === "ENOENT") return null; throw error }) }))
          return
        }
        if (request.url?.includes("count_tokens")) { response.end(JSON.stringify({ input_tokens: 10 })); return }
        if (!request.url?.includes("/messages")) { response.writeHead(404); response.end("{}"); return }
        // Only requests carrying our appended system prompt belong to the
        // scripted conversation. Native auxiliary calls keep their defaults.
        const conversation = JSON.stringify(body.system ?? []).includes("You are a local test agent.")
        if (conversation) { requests.push(body); admissions.push(admission!) }
        else { auxiliaryCalls++; lastAuxiliaryInput = body.messages }
        const send = (item: any) => {
          const blocks = Array.isArray(item) ? item : [item]
          const modelMessage = { id: `msg_${crypto.randomUUID()}`, type: "message", role: "assistant", model: body.model, content: [], stop_reason: null, stop_sequence: null, usage: { input_tokens: 10, output_tokens: 1 } }
          response.writeHead(200, { "content-type": "text/event-stream" })
          const emit = (type: string, data: object) => response.write(`event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`)
          emit("message_start", { message: modelMessage })
          blocks.forEach((block, index) => {
            emit("content_block_start", { index, content_block: block.type === "tool_use" ? { ...block, input: {} } : { type: "text", text: "" } })
            emit("content_block_delta", { index, delta: block.type === "tool_use" ? { type: "input_json_delta", partial_json: JSON.stringify(block.input) } : { type: "text_delta", text: block.text } })
            emit("content_block_stop", { index })
          })
          emit("message_delta", { delta: { stop_reason: blocks.some(block => block.type === "tool_use") ? "tool_use" : "end_turn", stop_sequence: null }, usage: { output_tokens: 2 } })
          emit("message_stop", {}); response.end()
        }
        const action = (conversation ? actions.shift() : undefined) ?? message()
        if (action === "hold") held.push({ send, input: body.messages })
        else send(action)
      } catch (error) { response.statusCode = 500; response.end(String(error)) }
    })
    server.listen(0, "127.0.0.1")
    await once(server, "listening")
    const port = (server.address() as { port: number }).port
    await writeFile(join(cwd, "control.json"), JSON.stringify({ port }))
    process.env.ANTHROPIC_BASE_URL = `http://127.0.0.1:${port}`
    process.env.ANTHROPIC_API_KEY = "offline-fixture"
    return { bridge: new ClaudeSession({ cwd, stateDirectory: "/var/lib/helmr/claude-starter", sessionId: session.id, model: "claude-sonnet-4-5", appendSystemPrompt: "You are a local test agent.", recovery: recovery.kind }), setups, nonce: crypto.randomUUID(), launchers }
  },
  async turn(turn, { initResult }) {
    const { response, ...native } = await initResult.bridge.run(turn)
    if (response !== undefined) await turn.respond(response)
    return { ...native, setups: initResult.setups, nonce: initResult.nonce, ownerPid: initResult.launchers.at(-1)?.pid ?? null } as JsonValue
  },
}) }
