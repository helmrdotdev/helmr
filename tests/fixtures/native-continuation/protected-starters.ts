// Synthetic upstream configuration around the editable starter bridges. The
// credential comes only from the ordinary protected binding for this process.
import assert from "node:assert/strict"
import { mkdir, readFile, writeFile } from "node:fs/promises"
import { createServer } from "node:http"
import { once } from "node:events"
import { join } from "node:path"
import { agent, type TemplateDefinition, type JsonValue } from "../../../sdk/typescript/src/index"
import { CodexSession, nativeHome } from "../../../starters/codex/agents/codex-bridge"
import { ClaudeSession } from "../../../starters/claude-code/agents/claude-bridge"
import { OpenCodeSession } from "../../../starters/opencode/agents/opencode-bridge"
import { PiSession } from "../../../starters/pi/agents/pi-bridge"
import { ModelRuntime, SettingsManager } from "@earendil-works/pi-coding-agent"

export const protectedKinds = ["claude", "codex", "opencode-anthropic", "opencode-openai", "opencode-go", "pi-anthropic", "pi-openai", "pi-go"] as const
export type ProtectedKind = typeof protectedKinds[number]

export function createProtectedStarter(kind: ProtectedKind, template: TemplateDefinition) {
  return agent({
    id: `protected-${kind}`, template, maxTurnDuration: "5m",
    async init({ session, recovery }) {
      const { origin } = JSON.parse(await readFile("/workspace/protected-fixture.json", "utf8")) as { origin: string }
      const marker = process.env.NATIVE_PROTECTED_KEY
      assert(marker && /^hlmr_protected_[a-f0-9]{64}$/.test(marker), "missing protected selector")
      const cwd = `/workspace/protected-${kind}/${session.id}`
      await mkdir(cwd, { recursive: true })
      const common = { cwd, sessionId: session.id, stateDirectory: `/var/lib/helmr/protected-${kind}`, recovery: recovery.kind }
      const base = `${origin}/${kind}/${session.id}`
      let bridge: CodexSession | ClaudeSession | OpenCodeSession | PiSession
      if (kind === "codex") {
        const home = nativeHome(common.stateDirectory, session.id)
        await mkdir(home, { recursive: true })
        await writeFile(join(home, "config.toml"), `model_provider = "fixture"\nmodel_reasoning_effort = "low"\n[model_providers.fixture]\nname = "Protected fixture"\nbase_url = "${base}/v1"\nwire_api = "responses"\nenv_key = "NATIVE_PROTECTED_KEY"\nrequires_openai_auth = false\nsupports_websockets = true\n`)
        bridge = new CodexSession({ ...common, model: "gpt-5.1-codex", instructions: "Use the protected test model." })
      } else if (kind === "claude") {
        process.env.ANTHROPIC_BASE_URL = base
        process.env.ANTHROPIC_API_KEY = marker
        bridge = new ClaudeSession({ ...common, model: "claude-sonnet-4-5", appendSystemPrompt: "Use the protected test model." })
      } else if (kind.startsWith("opencode-")) {
        const provider = kind.slice("opencode-".length)
        const providerID = provider === "openai" ? "openai" : provider === "go" ? "opencode-go" : "anthropic"
        const packageName = provider === "anthropic" ? "anthropic" : provider === "openai" ? "openai" : "openai-compatible"
        bridge = await OpenCodeSession.open({ ...common, model: `${providerID}/probe`, introduction: "Use the protected test model.", native: {
          config: { directory: join(cwd, "config"), project: false, claude: false, content: JSON.stringify({ providers: { [providerID]: {
            name: "Protected fixture", package: `@opencode/ai/providers/${packageName}`,
            settings: { baseURL: `${base}/v1`, apiKey: marker },
            models: { probe: { name: "Probe", limit: { context: 100000, output: 1000 }, capabilities: { tools: true, input: ["text"], output: ["text"] } } },
          } } }) }, models: { fetch: false, snapshot: false }, fs: { filewatcher: false, fff: false }, plugins: [],
        } })
      } else {
        const provider = kind.slice("pi-".length)
        const agentDir = join(cwd, "config")
        const modelRuntime = await ModelRuntime.create({ authPath: join(agentDir, "auth.json"), modelsPath: null, modelsStorePath: join(agentDir, "models.json"), allowModelNetwork: false, refreshOnCreate: false })
        modelRuntime.registerProvider("fixture", { baseUrl: provider === "anthropic" ? base : `${base}/v1`, api: provider === "anthropic" ? "anthropic-messages" : provider === "openai" ? "openai-responses" : "openai-completions", apiKey: marker, models: [{ id: "probe", name: "Probe", reasoning: false, input: ["text"], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 100000, maxTokens: 1000 }] })
        bridge = await PiSession.open({ ...common, model: "fixture/probe", introduction: "Use the protected test model.", native: { modelRuntime, agentDir, settingsManager: SettingsManager.inMemory({ compaction: { enabled: false }, retry: { enabled: false }, cacheWarming: "off" }) } })
      }
      // A Command cannot address this process's PID namespace. The local test
      // control asks the actual Session owner to terminate itself.
      const control = createServer((request, response) => {
        if (request.method !== "POST" || request.url !== "/crash") { response.writeHead(404).end(); return }
        response.end("accepted")
        setImmediate(() => process.kill(process.pid, "SIGKILL"))
      })
      control.listen(0, "127.0.0.1")
      await once(control, "listening")
      control.unref()
      await writeFile(join(cwd, "control.json"), JSON.stringify({ port: (control.address() as { port: number }).port }))
      return { bridge, nonce: crypto.randomUUID(), marker }
    },
    async turn(turn, { initResult }) {
      const { response, ...native } = await initResult.bridge.run(turn)
      if (response) await turn.respond(response)
      return { ...native, nonce: initResult.nonce, ownerPid: process.pid, selectorOnly: /^hlmr_protected_[a-f0-9]{64}$/.test(initResult.marker) } as JsonValue
    },
  })
}
