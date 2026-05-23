import { query, type Options as ClaudeOptions } from "@anthropic-ai/claude-agent-sdk"
import { Agent } from "@cursor/sdk"
import { Codex, type ThreadOptions } from "@openai/codex-sdk"
import { parseJson } from "./shell"
import type { Input } from "./types"

export const triageSchema = {
  type: "object",
  additionalProperties: false,
  properties: {
    summary: { type: "string" },
    findings: {
      type: "array",
      items: {
        type: "object",
        additionalProperties: false,
        properties: {
          title: { type: "string" },
          severity: { type: "string", enum: ["critical", "high", "medium", "low"] },
          details: { type: "string" },
          files: { type: "array", items: { type: "string" } },
          remediation: { type: "string" },
        },
        required: ["title", "severity", "details", "files", "remediation"],
      },
    },
  },
  required: ["summary", "findings"],
} as const

export async function runClaude(phase: string, prompt: string, options: ClaudeOptions): Promise<string> {
  const stream = query({
    prompt,
    options: {
      ...options,
      env: {
        ...baseAgentEnv(),
        ANTHROPIC_API_KEY: requiredProcessEnv("ANTHROPIC_API_KEY"),
        CLAUDE_AGENT_SDK_CLIENT_APP: "helmr-workflow/implement",
      },
    },
  })

  for await (const message of stream) {
    if (message.type !== "result") continue
    if (message.subtype === "success") {
      return message.result.trim()
    }
    throw new Error(`Claude ${phase} failed: ${message.errors.join("\n")}`)
  }
  throw new Error(`Claude ${phase} finished without a result message`)
}

export async function runCodex(apiKey: string, prompt: string, options: ThreadOptions): Promise<string> {
  const thread = codex(apiKey).startThread(options)
  const turn = await thread.run(prompt)
  return turn.finalResponse.trim()
}

export async function runCodexJson<T>(
  apiKey: string,
  prompt: string,
  schema: unknown,
  options: ThreadOptions,
): Promise<T> {
  const thread = codex(apiKey).startThread(options)
  const turn = await thread.run(prompt, { outputSchema: schema })
  return parseJson<T>(turn.finalResponse)
}

export async function runCursor(input: Input, cursorApiKey: string, prompt: string): Promise<string> {
  return withProcessEnv(cursorAgentEnv(cursorApiKey), async () => {
    const agent = await Agent.create({
      apiKey: cursorApiKey,
      model: { id: input.cursorModel },
      local: { cwd: process.cwd() },
    })
    try {
      const run = await agent.send(prompt, {
        model: { id: input.cursorModel },
        local: { force: true },
      })
      const result = await run.wait()
      if (result.status !== "finished") {
        throw new Error(`Cursor SDK run ${result.id} ended with status ${result.status}`)
      }
      if (!result.result?.trim()) {
        throw new Error(`Cursor SDK run ${result.id} finished without a text result`)
      }
      return result.result.trim()
    } finally {
      agent.close()
    }
  })
}

function codex(apiKey: string): Codex {
  return new Codex({
    apiKey,
    env: {
      ...baseAgentEnv(),
      OPENAI_API_KEY: apiKey,
      CODEX_API_KEY: apiKey,
    },
  })
}

function baseAgentEnv(): Record<string, string> {
  const env: Record<string, string> = {}
  for (const key of ["HOME", "PATH", "TMPDIR", "USER", "LOGNAME", "LANG", "LC_ALL"]) {
    const value = process.env[key]
    if (typeof value === "string") {
      env[key] = value
    }
  }
  return env
}

function cursorAgentEnv(cursorApiKey: string): Record<string, string> {
  return {
    ...baseAgentEnv(),
    CURSOR_API_KEY: cursorApiKey,
  }
}

async function withProcessEnv<T>(env: Record<string, string>, callback: () => Promise<T>): Promise<T> {
  const original = { ...process.env }
  for (const key of Object.keys(process.env)) {
    delete process.env[key]
  }
  Object.assign(process.env, env)
  try {
    return await callback()
  } finally {
    for (const key of Object.keys(process.env)) {
      delete process.env[key]
    }
    Object.assign(process.env, original)
  }
}

function requiredProcessEnv(key: string): string {
  const value = process.env[key]
  if (!value) {
    throw new Error(`${key} is required`)
  }
  return value
}
