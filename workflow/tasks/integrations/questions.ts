import { parseJson } from "./shell"
import type { AgentQuestionResult } from "./types"

export const agentQuestionSchema = {
  type: "object",
  additionalProperties: false,
  properties: {
    status: { type: "string", enum: ["done", "needs_input"] },
    content: { type: "string" },
    question: { type: "string" },
    context: { type: "string" },
  },
  required: ["status", "content", "question", "context"],
} as const

export interface OperatorQuestionRequest {
  readonly phase: string
  readonly questionNumber: number
  readonly question: string
  readonly context?: string
}

export type AskOperator = (request: OperatorQuestionRequest) => Promise<string>

export function renderAgentQuestionPrompt(basePrompt: string): string {
  return [
    "<interactive_output_contract>",
    "Return only valid JSON. Do not wrap it in markdown.",
    "Always include `status`, `content`, `question`, and `context`. Use an empty string for unused fields.",
    "If you have enough information to complete the requested phase, return:",
    `{"status":"done","content":"<the complete phase output>","question":"","context":""}`,
    "If a specific operator answer is required before you can produce a correct result, return:",
    `{"status":"needs_input","content":"","question":"<one concrete question>","context":"<why this blocks the workflow>"}`,
    "Ask at most one question. Ask only for information that materially changes the implementation plan or guardrails.",
    "Do not ask about secrets or request secret values.",
    "</interactive_output_contract>",
    "",
    basePrompt,
  ].join("\n")
}

export function renderOperatorAnswerPrompt(answer: string): string {
  return [
    "<operator_answer>",
    answer,
    "</operator_answer>",
    "",
    "<task>",
    "Continue the same phase using the operator answer.",
    "Return only valid JSON using the same interactive output contract: either `done` with complete content or `needs_input` with one concrete follow-up question.",
    "</task>",
  ].join("\n")
}

export function parseAgentQuestionResult(value: string, phase: string): AgentQuestionResult {
  const result = parseJson<AgentQuestionResult>(value)
  if (result.status === "done") {
    if (!result.content.trim()) {
      throw new Error(`${phase} returned empty content`)
    }
    return result
  }
  if (result.status === "needs_input") {
    if (!result.question.trim()) {
      throw new Error(`${phase} returned an empty operator question`)
    }
    return {
      status: "needs_input",
      question: result.question.trim(),
      context: result.context?.trim() || undefined,
    }
  }
  throw new Error(`${phase} returned invalid interactive status`)
}
