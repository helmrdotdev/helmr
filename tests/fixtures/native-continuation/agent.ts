import { fixtureValue } from "../../e2e/support/runtime-mcp"
// Disposable qualification application. Real native processes use local model
// responses; no provider credentials or external model service are needed.
import { readFile, writeFile, mkdir } from "node:fs/promises"
import { join } from "node:path"
import { createRuntimeMcpConnection, type RuntimeMcpConnection } from "../../../sdk/typescript/src/mcp"
import type { Turn } from "../../../sdk/typescript/src/agent"
import { CodexHarness } from "../../../examples/issue-fixer/tasks/issue-fixer/codex-harness"
import { ClaudeHarness } from "../../../examples/issue-fixer/tasks/issue-fixer/claude-harness"
import { conversation } from "../../../examples/issue-fixer/tasks/issue-fixer/conversation"

interface SetupState {
  harness: CodexHarness | ClaudeHarness
  mcp: RuntimeMcpConnection
  count: number
  nonce: string
}

function specimen(provider: "codex" | "claude") {
  return {
    kind: "agent" as const, id: provider, computer: { kind: "computer" as const, id: "native-continuation" },
    async setup(context: { session: { id: string } }): Promise<SetupState> {
      const directory = join("/workspace", context.session.id)
      await mkdir(directory, { recursive: true })
      const endpoints = JSON.parse(await readFile("/workspace/native-models.json", "utf8")) as { codex: number; claude: number; host?: string }
      const port = endpoints[provider]
      const host = endpoints.host ?? "127.0.0.1"
      const environment = { PATH: "/usr/local/bin:/usr/bin:/bin", HOME: directory,
        ANTHROPIC_BASE_URL: `http://${host}:${port}`, ANTHROPIC_API_KEY: "local-fixture",
        CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1", DISABLE_TELEMETRY: "1", ENABLE_TOOL_SEARCH: "false", MCP_CONNECTION_NONBLOCKING: "0", MCP_CONNECT_TIMEOUT_MS: "10000" }
      const mcp = await createRuntimeMcpConnection()
      if (provider === "codex") {
        const saved = await conversation(directory, context.session.id, provider)
        await writeFile(join(saved.directory, "config.toml"), `model_provider = "fixture"\nmodel = "gpt-5.1-codex"\n[model_providers.fixture]\nname = "local fixture"\nbase_url = "http://${host}:${port}/v1"\nwire_api = "responses"\nrequires_openai_auth = false\n[mcp_servers.helmr]\nurl = ${JSON.stringify(mcp.url)}\nhttp_headers = { Authorization = ${JSON.stringify(mcp.headers.Authorization)} }\n[mcp_servers.helmr.tools.enqueue]\napproval_mode = "approve"\n`)
      }
      const harness = provider === "codex"
        ? await CodexHarness.open(directory, context.session.id, environment)
        : await ClaudeHarness.open(directory, context.session.id, environment, { helmr: { type: "http", url: mcp.url, headers: mcp.headers } })
      return { harness, mcp, count: 0, nonce: crypto.randomUUID() }
    },
    async turn(turn: Turn, context: { setupResult: SetupState }) {
      const state = context.setupResult
      const result = await state.harness.run(turn, String(fixtureValue(turn.input)))
      return { count: ++state.count, pid: process.pid, nativePid: state.harness.processId!, nonce: state.nonce,
        result }
    },
  }
}
export const codex = specimen("codex")
export const claude = specimen("claude")
