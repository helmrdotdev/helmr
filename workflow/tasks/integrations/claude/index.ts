import { query, type Options as ClaudeOptions } from "@anthropic-ai/claude-agent-sdk"
import { baseAgentEnv, requiredProcessEnv } from "../env"
import {
  parseAgentQuestionResult,
  renderAgentQuestionPrompt,
  renderOperatorAnswerPrompt,
  type AskOperator,
} from "../questions"
import type { Input } from "../types"

interface ClaudeTurn {
  readonly content: string
  readonly sessionId: string
}

export async function runClaude(phase: string, prompt: string, options: ClaudeOptions): Promise<string> {
  return (await runClaudeTurn(phase, prompt, options)).content
}

export async function runClaudeSlashCommand(
  phase: string,
  command: string,
  args: readonly string[],
  options: ClaudeOptions,
): Promise<string> {
  const commandName = command.replace(/^\//, "")
  const prompt = [`/${commandName}`, ...args].join(" ")
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

  let commandAvailable = false
  let finalText = ""
  for await (const message of stream) {
    if (message.type === "system" && message.subtype === "init") {
      commandAvailable = message.slash_commands.includes(commandName)
      continue
    }
    if (message.type === "assistant") {
      const text = assistantMessageText(message)
      if (text) finalText = text
      continue
    }
    if (message.type === "system" && message.subtype === "local_command_output" && message.content.trim()) {
      finalText = message.content.trim()
      continue
    }
    if (message.type !== "result") continue
    if (!commandAvailable) {
      throw new Error(`Claude ${phase} requires /${commandName}, but this SDK session did not advertise it`)
    }
    if (message.subtype === "success") {
      const result = message.result.trim() || finalText
      if (!result) throw new Error(`Claude ${phase} completed without output`)
      return result
    }
    throw new Error(`Claude ${phase} failed: ${message.errors.join("\n")}`)
  }
  throw new Error(`Claude ${phase} finished without a result message`)
}

export async function runClaudeWithOperatorQuestions(
  input: Input,
  phase: string,
  prompt: string,
  options: ClaudeOptions,
  askOperator: AskOperator,
): Promise<string> {
  if (!input.operatorInput || input.maxOperatorQuestionsPerPhase === 0) {
    return runClaude(phase, prompt, options)
  }

  let nextPrompt = renderAgentQuestionPrompt(prompt)
  let sessionId: string | undefined
  for (let questionNumber = 1; questionNumber <= input.maxOperatorQuestionsPerPhase + 1; questionNumber += 1) {
    const turn = await runClaudeTurn(
      phase,
      nextPrompt,
      sessionId === undefined ? options : { ...options, resume: sessionId },
    )
    sessionId = turn.sessionId
    const result = parseAgentQuestionResult(turn.content, phase)
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

  throw new Error(`${phase} ended without a final response`)
}

async function runClaudeTurn(phase: string, prompt: string, options: ClaudeOptions): Promise<ClaudeTurn> {
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
      return { content: message.result.trim(), sessionId: message.session_id }
    }
    throw new Error(`Claude ${phase} failed: ${message.errors.join("\n")}`)
  }
  throw new Error(`Claude ${phase} finished without a result message`)
}

function assistantMessageText(message: { readonly message: { readonly content: unknown } }): string {
  const content = message.message.content
  if (!Array.isArray(content)) return ""
  return content.flatMap((item) => {
    if (typeof item !== "object" || item === null) return []
    const record = item as { readonly type?: unknown; readonly text?: unknown }
    if (record.type === "text" && typeof record.text === "string" && record.text.trim()) {
      return [record.text.trim()]
    }
    return []
  }).join("\n\n").trim()
}
