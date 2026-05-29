import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process"
import { createRequire } from "node:module"
import { createInterface, type Interface as ReadlineInterface } from "node:readline"
import { baseAgentEnv } from "../env"
import {
  agentQuestionSchema,
  parseAgentQuestionResult,
  renderAgentQuestionPrompt,
  renderOperatorAnswerPrompt,
  type AskOperator,
} from "../questions"
import { parseJson } from "../shell"
import type { Input } from "../types"

const require = createRequire(import.meta.url)

export type CodexApprovalPolicy = "never" | "on-request" | "on-failure" | "untrusted"
export type CodexSandboxMode = "read-only" | "workspace-write" | "danger-full-access"
export type CodexReasoningEffort = "minimal" | "low" | "medium" | "high" | "xhigh"

export interface CodexThreadOptions {
  readonly model?: string
  readonly sandboxMode?: CodexSandboxMode
  readonly workingDirectory?: string
  readonly skipGitRepoCheck?: boolean
  readonly modelReasoningEffort?: CodexReasoningEffort
  readonly approvalPolicy?: CodexApprovalPolicy
}

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

export async function runCodex(apiKey: string, prompt: string, options: CodexThreadOptions): Promise<string> {
  const server = await CodexAppServer.start(apiKey)
  try {
    const thread = await server.startThread(options)
    const turn = await server.startTurn(thread.id, prompt, options)
    return finalCodexText(turn, "Codex turn")
  } finally {
    server.close()
  }
}

export async function runCodexReview(
  apiKey: string,
  developerInstructions: string,
  scopedDiff: string,
  options: CodexThreadOptions,
): Promise<string> {
  const server = await CodexAppServer.start(apiKey)
  try {
    const thread = await server.startThread(options, developerInstructions)
    const turn = await server.startTurn(thread.id, renderCodexScopedReviewPrompt(scopedDiff), options)
    return finalCodexText(turn, "Codex review")
  } finally {
    server.close()
  }
}

function renderCodexScopedReviewPrompt(scopedDiff: string): string {
  return [
    "Review only the scoped working tree diff below.",
    "The diff was generated with the workflow review index, includes untracked implementation files, and excludes .helmr-workflow-artifacts and .helmr/task-source.",
    "You may inspect changed files and directly related repository contracts for context, but do not review files outside this scoped review surface.",
    "",
    scopedDiff,
  ].join("\n")
}

export async function runCodexWithOperatorQuestions(
  input: Input,
  apiKey: string,
  phase: string,
  prompt: string,
  options: CodexThreadOptions,
  askOperator: AskOperator,
): Promise<string> {
  if (!input.operatorInput || input.maxOperatorQuestionsPerPhase === 0) {
    return runCodex(apiKey, prompt, options)
  }

  const server = await CodexAppServer.start(apiKey)
  try {
    const thread = await server.startThread(options)
    let nextPrompt = renderAgentQuestionPrompt(prompt)
    for (let questionNumber = 1; questionNumber <= input.maxOperatorQuestionsPerPhase + 1; questionNumber += 1) {
      const turn = await server.startTurn(thread.id, nextPrompt, options, agentQuestionSchema)
      const result = parseAgentQuestionResult(finalCodexText(turn, phase), phase)
      if (result.status === "done") {
        return result.content.trim()
      }
      if (questionNumber > input.maxOperatorQuestionsPerPhase) {
        throw new Error(`${phase} exceeded maxOperatorQuestionsPerPhase (${input.maxOperatorQuestionsPerPhase})`)
      }
      const answer = await askOperator({
        phase,
        questionNumber,
        question: result.question,
        context: result.context,
      })
      nextPrompt = renderOperatorAnswerPrompt(answer)
    }
  } finally {
    server.close()
  }

  throw new Error(`${phase} ended without a final response`)
}

export async function runCodexJson<T>(
  apiKey: string,
  prompt: string,
  schema: unknown,
  options: CodexThreadOptions,
): Promise<T> {
  const server = await CodexAppServer.start(apiKey)
  try {
    const thread = await server.startThread(options)
    const turn = await server.startTurn(thread.id, prompt, options, schema)
    return parseJson<T>(finalCodexText(turn, "Codex JSON turn"))
  } finally {
    server.close()
  }
}

interface CodexThread {
  readonly id: string
}

interface CodexTurnError {
  readonly message: string
}

interface CodexThreadItem {
  readonly type: string
  readonly text?: string
  readonly review?: string
}

interface CodexTurn {
  readonly id: string
  readonly status: "completed" | "interrupted" | "failed" | "inProgress"
  readonly error: CodexTurnError | null
  readonly items: readonly CodexThreadItem[]
}

interface CodexRpcResponse {
  readonly id: number
  readonly result?: unknown
  readonly error?: { readonly message?: string } | string
}

interface CodexRpcNotification {
  readonly method: string
  readonly params?: unknown
}

type CodexRpcMessage = CodexRpcResponse | CodexRpcNotification

class CodexAppServer {
  private readonly child: ChildProcessWithoutNullStreams
  private readonly lines: ReadlineInterface
  private readonly pending = new Map<number, {
    readonly resolve: (value: unknown) => void
    readonly reject: (reason: Error) => void
  }>()
  private readonly completedTurns = new Map<string, CodexTurn>()
  private readonly completedItems = new Map<string, CodexThreadItem[]>()
  private readonly turnWaiters = new Map<string, {
    readonly resolve: (turn: CodexTurn) => void
    readonly reject: (reason: Error) => void
  }>()
  private nextId = 1
  private stderr = ""
  private closed = false

  private constructor(apiKey: string) {
    this.child = spawn(process.execPath, [codexBinPath(), "app-server", "--listen", "stdio://"], {
      env: {
        ...baseAgentEnv(),
        OPENAI_API_KEY: apiKey,
        CODEX_API_KEY: apiKey,
        CODEX_INTERNAL_ORIGINATOR_OVERRIDE: "helmr_workflow",
      },
    })
    this.lines = createInterface({ input: this.child.stdout })
    this.lines.on("line", (line) => this.handleLine(line))
    this.child.stderr.on("data", (chunk: Buffer) => {
      this.stderr = `${this.stderr}${chunk.toString("utf8")}`.slice(-12000)
    })
    this.child.once("error", (error) => this.rejectAll(new Error(`Codex app-server failed to start: ${error.message}`)))
    this.child.once("exit", (code, signal) => {
      if (this.pending.size === 0 && this.turnWaiters.size === 0) return
      const detail = this.stderr.trim() ? `\n${this.stderr.trim()}` : ""
      this.rejectAll(new Error(`Codex app-server exited before completing requests: code=${code} signal=${signal}${detail}`))
    })
  }

  static async start(apiKey: string): Promise<CodexAppServer> {
    const server = new CodexAppServer(apiKey)
    await server.request("initialize", {
      clientInfo: { name: "helmr-workflow", title: "Helmr workflow", version: "0.1.0" },
      capabilities: {
        experimentalApi: true,
        requestAttestation: false,
        optOutNotificationMethods: [],
      },
    })
    await server.loginApiKey(apiKey)
    return server
  }

  private async loginApiKey(apiKey: string): Promise<void> {
    await this.request("account/login/start", {
      type: "apiKey",
      apiKey,
    })
  }

  async startThread(options: CodexThreadOptions, developerInstructions?: string): Promise<CodexThread> {
    const result = await this.request("thread/start", {
      model: options.model ?? null,
      cwd: options.workingDirectory ?? process.cwd(),
      approvalPolicy: options.approvalPolicy ?? "never",
      sandbox: options.sandboxMode ?? "read-only",
      config: codexConfig(options),
      serviceName: "helmr-workflow",
      developerInstructions: developerInstructions?.trim() || null,
      ephemeral: true,
    })
    const thread = objectField<CodexThread>(result, "thread")
    if (!thread.id) throw new Error("Codex app-server thread/start returned a thread without id")
    return thread
  }

  async startTurn(
    threadId: string,
    prompt: string,
    options: CodexThreadOptions,
    outputSchema?: unknown,
  ): Promise<CodexTurn> {
    const result = await this.request("turn/start", {
      threadId,
      input: [{ type: "text", text: prompt, text_elements: [] }],
      model: options.model ?? null,
      effort: options.modelReasoningEffort ?? null,
      approvalPolicy: options.approvalPolicy ?? "never",
      outputSchema: outputSchema ?? null,
    })
    return this.waitForCompletion(objectField<CodexTurn>(result, "turn"))
  }

  close(): void {
    if (this.closed) return
    this.closed = true
    this.rejectAll(new Error("Codex app-server closed before completing requests"))
    this.lines.close()
    this.child.stdin.end()
    if (!this.child.killed && this.child.exitCode === null) {
      setTimeout(() => {
        if (!this.child.killed && this.child.exitCode === null) this.child.kill("SIGTERM")
      }, 50).unref()
    }
  }

  private request(method: string, params: unknown): Promise<unknown> {
    if (this.closed) {
      return Promise.reject(new Error(`Codex app-server is closed; cannot send ${method}`))
    }
    const id = this.nextId
    this.nextId += 1
    const payload = `${JSON.stringify({ id, method, params })}\n`
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject })
      this.child.stdin.write(payload, (error) => {
        if (!error) return
        this.pending.delete(id)
        reject(new Error(`failed to write Codex app-server request ${method}: ${error.message}`))
      })
    })
  }

  private waitForCompletion(turn: CodexTurn): Promise<CodexTurn> {
    const completed = this.completedTurns.get(turn.id)
    if (completed) return Promise.resolve(assertSuccessfulCodexTurn(this.hydrateTurnItems(completed)))
    if (turn.status !== "inProgress") return Promise.resolve(assertSuccessfulCodexTurn(this.hydrateTurnItems(turn)))

    return new Promise((resolve, reject) => {
      this.turnWaiters.set(turn.id, {
        resolve: (completedTurn) => resolve(assertSuccessfulCodexTurn(this.hydrateTurnItems(completedTurn))),
        reject,
      })
    })
  }

  private handleLine(line: string): void {
    let message: CodexRpcMessage
    try {
      message = JSON.parse(line) as CodexRpcMessage
    } catch {
      return
    }

    if ("id" in message) {
      const pending = this.pending.get(message.id)
      if (!pending) return
      this.pending.delete(message.id)
      if (message.error) {
        pending.reject(new Error(codexErrorMessage(message.error)))
      } else {
        pending.resolve(message.result)
      }
      return
    }

    this.handleNotification(message)
  }

  private handleNotification(notification: CodexRpcNotification): void {
    if (notification.method === "item/completed") {
      const params = notification.params
      const turnId = objectField<string>(params, "turnId")
      const item = objectField<CodexThreadItem>(params, "item")
      const items = this.completedItems.get(turnId) ?? []
      items.push(item)
      this.completedItems.set(turnId, items)
    } else if (notification.method === "turn/completed") {
      const turn = objectField<CodexTurn>(notification.params, "turn")
      this.completedTurns.set(turn.id, turn)
      const waiter = this.turnWaiters.get(turn.id)
      if (waiter) {
        this.turnWaiters.delete(turn.id)
        waiter.resolve(turn)
      }
    } else if (notification.method === "error") {
      this.rejectAll(new Error(`Codex app-server error: ${JSON.stringify(notification.params)}`))
    }
  }

  private rejectAll(error: Error): void {
    for (const [id, pending] of this.pending) {
      this.pending.delete(id)
      pending.reject(error)
    }
    for (const [id, waiter] of this.turnWaiters) {
      this.turnWaiters.delete(id)
      waiter.reject(error)
    }
  }

  private hydrateTurnItems(turn: CodexTurn): CodexTurn {
    const items = this.completedItems.get(turn.id)
    if (!items || items.length === 0) return turn
    return { ...turn, items }
  }
}

function codexConfig(options: CodexThreadOptions): Record<string, string> | null {
  if (!options.modelReasoningEffort) return null
  return { model_reasoning_effort: options.modelReasoningEffort }
}

function codexBinPath(): string {
  return require.resolve("@openai/codex/bin/codex.js")
}

function objectField<T>(value: unknown, field: string): T {
  if (typeof value !== "object" || value === null || !(field in value)) {
    throw new Error(`Codex app-server response missing field: ${field}`)
  }
  return (value as Record<string, unknown>)[field] as T
}

function assertSuccessfulCodexTurn(turn: CodexTurn): CodexTurn {
  if (turn.status === "completed") return turn
  const detail = turn.error?.message ? `: ${turn.error.message}` : ""
  throw new Error(`Codex turn ended with status ${turn.status}${detail}`)
}

function finalCodexText(turn: CodexTurn, label: string): string {
  const texts = turn.items.flatMap((item) => {
    if (item.type === "agentMessage" && item.text?.trim()) return [item.text.trim()]
    if (item.type === "exitedReviewMode" && item.review?.trim()) return [item.review.trim()]
    return []
  })
  const text = texts.at(-1)
  if (!text) {
    throw new Error(`${label} completed without a final text response`)
  }
  return text
}

function codexErrorMessage(error: CodexRpcResponse["error"]): string {
  if (typeof error === "string") return error
  return error?.message || "Codex app-server request failed"
}
