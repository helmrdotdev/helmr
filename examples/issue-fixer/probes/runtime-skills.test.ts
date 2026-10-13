// Real pinned native harnesses and Helmr MCP handler; local model responses only.
import assert from "node:assert/strict"
import { spawn } from "node:child_process"
import { once } from "node:events"
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { createInterface } from "node:readline"
import test from "node:test"
import type { RuntimeMcpConnectionOptions } from "@helmr/sdk"
import { SessionRuntime, type SessionDriver } from "../../../runtime/typescript/src/agent-session"
import { CodexHarness } from "../agents/issue-fixer/codex-harness"
import { ClaudeHarness } from "../agents/issue-fixer/claude-harness"
import { conversation } from "../agents/issue-fixer/conversation"
import { localNativeRuntime } from "./native-runtime"
import { claudeModel, codexModel } from "./native-model"

function containsDocument(value: unknown, expected: string): boolean {
  if (typeof value === "string") {
    if (value === expected) return true
    // Codex wraps MCP content in a timing preamble; Claude returns content directly.
    const payload = value.startsWith("Wall time:") ? value.split("\nOutput:\n").slice(1).join("\nOutput:\n") : value
    try { return containsDocument(JSON.parse(payload), expected) } catch { return false }
  }
  if (Array.isArray(value)) return value.some(item => containsDocument(item, expected))
  return value !== null && typeof value === "object" && Object.values(value).some(item => containsDocument(item, expected))
}

for (const provider of ["codex", "claude"] as const) {
  test(`${provider} discovers and reads the bundled runtime skill over MCP`, { timeout: 40_000 }, async () => {
    const directory = await mkdtemp(join(tmpdir(), "helmr-runtime-skills-"))
    const expected = await readFile("../../internal/runtimemcp/skills/helmr-runtime/SKILL.md", "utf8")
    const server = spawn(resolve("dist/probes/runtime-mcp-server"), [], { stdio: ["pipe", "pipe", "inherit"] })
    const exited = once(server, "exit")
    const lines = createInterface({ input: server.stdout })
    const stopped = new AbortController()
    const deadline = setTimeout(() => stopped.abort(new Error("Skill probe timed out")), 30_000)
    const requests: any[] = []
    const argumentsFor = (index: number) => index === 1 ? {} : { name: "helmr-runtime" }
    const model = provider === "codex"
      ? codexModel(requests, false, false, (body, index) => {
        if (index > 2) return undefined
        const namespace = body.tools.find((tool: any) => tool.type === "namespace" && tool.tools?.some((nested: any) => nested.name === "skills"))
        assert(namespace, "Codex did not advertise the skills tool")
        return {
          type: "function_call", id: `skill_${index}`, call_id: `skill_${index}`,
          name: "skills", namespace: namespace.name, arguments: JSON.stringify(argumentsFor(index)),
        }
      })
      : claudeModel(requests, false, (_body, index) => index <= 2 ? {
        id: `skill_${index}`, name: "mcp__helmr__skills", input: argumentsFor(index),
      } : undefined)
    const native = localNativeRuntime(stopped.signal)
    let harness: CodexHarness | ClaudeHarness | undefined
    try {
      const [line] = await once(lines, "line", { signal: stopped.signal })
      const connection = JSON.parse(line) as RuntimeMcpConnectionOptions
      await new Promise<void>(resolve => model.listen(0, "127.0.0.1", resolve))
      const port = (model.address() as { port: number }).port
      const environment = {
        PATH: process.env.PATH, HOME: directory,
        ANTHROPIC_BASE_URL: `http://127.0.0.1:${port}`, ANTHROPIC_API_KEY: "local-fixture",
        CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1", DISABLE_TELEMETRY: "1",
        ENABLE_TOOL_SEARCH: "false", MCP_CONNECTION_NONBLOCKING: "0",
      }
      if (provider === "codex") {
        const saved = await conversation(directory, "skills-session", "codex")
        await writeFile(join(saved.directory, "config.toml"), `model_provider = "fixture"\nmodel = "gpt-5.1-codex"\n[model_providers.fixture]\nname = "local fixture"\nbase_url = "http://127.0.0.1:${port}/v1"\nwire_api = "responses"\nrequires_openai_auth = false\n`)
      }
      const driver: SessionDriver = {
        registerMessages: async () => {}, closeProcessing: async () => {},
        output: async () => ({ sequence: 1 }), respond: async () => {},
        ask: async () => ({ answer: Promise.resolve({ answer: { selected: [{ id: "allow", value: true }] }, respondedBy: { kind: "user", id: "fixture" } }), withdraw: async () => {} }),
        convergeNative: async (turnId, disposition, signal) => { await native.registry.join(turnId, disposition, signal.reason) },
        finalize: async (_id, result) => ({ status: "completed", result }),
        fail: async (_id, error) => ({ status: "failed", error }),
        hold: async () => {},
      }
      const runtime = new SessionRuntime({
        kind: "agent", id: "skills-probe", template: undefined!,
        init: async ctx => {
          const mcp = await ctx.mcp.getConnectionOptions()
          harness = provider === "codex"
            ? await CodexHarness.open(directory, ctx.session.id, environment, mcp)
            : await ClaudeHarness.open(directory, ctx.session.id, environment, { helmr: { type: "http", ...mcp } })
          return harness
        },
        turn: (turn, ctx) => (ctx.initResult as CodexHarness | ClaudeHarness).run(turn, "Read the Helmr runtime skill."),
      }, {
        session: { id: "skills-session" }, computer: { id: "computer" }, deployment: { id: "deployment" },
        recovery: { kind: "initial" }, signal: stopped.signal, mcp: { getConnectionOptions: async () => connection },
      }, driver, { failureConvergenceMs: 5000, native: native.registry })
      const outcome = await runtime.dispatch({ id: "skill-turn", sequence: 1, createdAt: new Date().toISOString(), input: [{ type: "text", text: "Read the Helmr runtime skill." }], source: { kind: "api" } })
      assert.equal(outcome.status, "completed", JSON.stringify(outcome))
      assert.equal(requests.length, 3, "expected list, read and completion model steps")
      assert(containsDocument(requests[2], expected), "the model did not receive the complete embedded skill")
    } finally {
      clearTimeout(deadline)
      stopped.abort()
      await harness?.close()
      native.uninstall()
      model.closeAllConnections()
      await new Promise<void>(resolve => model.close(() => resolve()))
      lines.close()
      server.stdin.end()
      await exited
      await rm(directory, { recursive: true, force: true })
    }
  })
}
