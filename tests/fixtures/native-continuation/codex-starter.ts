// The editable starter bridge runs unchanged; this fixture owns only the local
// model, fault injection and observations around the real Helmr Agent.
import { agent, type JsonValue, type TemplateDefinition } from "../../../sdk/typescript/src/index"
import { mkdir, readFile, writeFile } from "node:fs/promises"
import { createServer } from "node:http"
import { createHash } from "node:crypto"
import { once } from "node:events"
import { channel } from "node:diagnostics_channel"
import { spawnSync, type ChildProcess } from "node:child_process"
import { join } from "node:path"
import { CodexSession, nativeHome } from "../../../starters/codex/agents/codex-bridge"

export function createCodexStarter(template: TemplateDefinition) { return agent({
  id: "codex-starter", template,
  async init({ session, recovery }) {
    const cwd = `/workspace/codex-starter/${session.id}`
    const home = nativeHome("/var/lib/helmr/codex-starter", session.id)
    await mkdir(cwd, { recursive: true })
    await mkdir(home, { recursive: true })
    let setups = 0
    try { setups = Number(await readFile(join(cwd, "setups"), "utf8")) }
    catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error }
    await writeFile(join(cwd, "setups"), String(++setups))
    const requests: any[] = [], actions: any[] = [], held: ((item: unknown) => void)[] = [], launchers: ChildProcess[] = []
    const launches = channel("child_process")
    launches.subscribe(value => {
      const child = (value as { process: ChildProcess }).process
      child.once("spawn", () => { if (child.spawnargs.some(arg => arg.endsWith("/bin/codex.js"))) launchers.push(child) })
    })
    const message = () => ({ type: "message", role: "assistant", id: `msg_${crypto.randomUUID()}`, content: [{ type: "output_text", text: "offline starter reply" }] })
    const server = createServer(async (request, response) => {
      try {
        let raw = ""
        for await (const chunk of request) raw += chunk
        const body = raw ? JSON.parse(raw) : {}
        if (request.url === "/control") {
          actions.push(...(body.actions ?? []))
          if (body.release) held.shift()?.(message())
          if (body.crash) {
            const pid = launchers.at(-1)?.pid
            if (!pid) throw new Error("No owned native launcher")
            const children = (await readFile(`/proc/${pid}/task/${pid}/children`, "utf8")).trim().split(/\s+/).map(Number)
            if (children.length !== 1 || !children[0]) throw new Error("Native owner identity is ambiguous")
            process.kill(children[0], "SIGKILL")
          }
          const lock = spawnSync("/usr/bin/flock", ["--nonblock", "--exclusive", join(cwd, "tool.lock"), "/bin/true"], { timeout: 1000 })
          if (lock.error || lock.signal || (lock.status !== 0 && lock.status !== 1)) throw new Error(`Invalid lifetime lock observation: ${JSON.stringify({ status: lock.status, signal: lock.signal, error: lock.error?.message })}`)
          response.setHeader("content-type", "application/json")
          response.end(JSON.stringify({ setups, held: held.length, calls: requests.length, inputs: requests.map(item => item.input),
            goalTools: requests.some(item => item.tools?.some((tool: any) => ["create_goal", "update_goal", "get_goal"].includes(tool.name))),
            owners: launchers.map(child => ({ pid: child.pid, exited: child.exitCode !== null || child.signalCode !== null })),
            configHash: createHash("sha256").update(await readFile(join(home, "config.toml"))).digest("hex"),
            toolRunning: lock.status === 1, toolFinished: await readFile(join(cwd, "tool.finished"), "utf8").catch(error => { if (error.code === "ENOENT") return null; throw error }) }))
          return
        }
        if (!request.url?.endsWith("/responses")) { response.end(JSON.stringify({ data: [] })); return }
        if (body.generate !== false) requests.push(body)
        response.writeHead(200, { "content-type": "text/event-stream" })
        const send = (item: unknown) => {
          const id = `resp_${crypto.randomUUID()}`
          for (const event of [{ type: "response.created", response: { id } }, ...(item ? [{ type: "response.output_item.done", item }] : []), { type: "response.completed", response: { id, output: item ? [item] : [], usage: { input_tokens: 10, output_tokens: 2, total_tokens: 12 } } }]) response.write(`data: ${JSON.stringify(event)}\n\n`)
          response.end()
        }
        const action = body.generate === false ? null : actions.shift() ?? message()
        if (action === "hold") held.push(send)
        else send(action)
      } catch (error) { response.statusCode = 500; response.end(String(error)) }
    })
    server.listen(0, "127.0.0.1")
    await once(server, "listening")
    const port = (server.address() as { port: number }).port
    await writeFile(join(cwd, "control.json"), JSON.stringify({ port }))
    await writeFile(join(home, "config.toml"), `model_provider = "starter_fixture"\nmodel_reasoning_effort = "low"\n[model_providers.starter_fixture]\nname = "Local starter fixture"\nbase_url = "http://127.0.0.1:${port}/v1"\nwire_api = "responses"\nrequires_openai_auth = false\nsupports_websockets = false\n[features]\ngoals = true\n`)
    return { bridge: new CodexSession({ cwd, stateDirectory: "/var/lib/helmr/codex-starter", sessionId: session.id, model: "gpt-5.1-codex", instructions: "You are a local test agent.", recovery: recovery.kind }), setups, nonce: crypto.randomUUID(), launchers }
  },
  async turn(turn, { initResult }) {
    const { response, ...native } = await initResult.bridge.run(turn)
    if (response !== undefined) await turn.respond(response)
    return { ...native, setups: initResult.setups, nonce: initResult.nonce, ownerPid: initResult.launchers.at(-1)?.pid ?? null } as JsonValue
  },
}) }
