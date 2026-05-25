import { DEFAULT_CLAUDE_MODEL, DEFAULT_CODEX_MODEL, DEFAULT_CURSOR_MODEL } from "../models"
import type { GitHubTaskSource, TaskContext } from "@helmr/sdk"

export type FeatureDesign = string | Record<string, unknown>

export interface Payload {
  readonly featureDesign?: FeatureDesign
  readonly prBaseBranch?: string
  readonly prTitle?: string
  readonly prBody?: string
  readonly maxReviewRounds?: number
  readonly operatorInput?: boolean
  readonly operatorInputTimeout?: number
  readonly maxOperatorQuestionsPerPhase?: number
  readonly claudeModel?: string
  readonly codexModel?: string
  readonly cursorModel?: string
}

export interface Input {
  readonly featureDesign: string
  readonly prBaseBranch?: string
  readonly prTitle: string
  readonly prBody: string
  readonly maxReviewRounds: number
  readonly operatorInput: boolean
  readonly operatorInputTimeout: number
  readonly maxOperatorQuestionsPerPhase: number
  readonly claudeModel?: string
  readonly codexModel?: string
  readonly cursorModel: string
}

export interface AuthSecrets {
  readonly openaiApiKey: string
  readonly cursorApiKey: string
  readonly githubToken: string
}

export interface RepoSnapshot {
  readonly head: string
  readonly baseSha: string
  readonly branch: string
  readonly status: string
}

export interface ReviewRound {
  readonly round: number
  readonly codexReview: string
  readonly claudeReview: string
  readonly codexTriage: TriageResult
  readonly cursorFix?: string
}

export interface TriageFinding {
  readonly title: string
  readonly severity: "critical" | "high" | "medium" | "low"
  readonly details: string
  readonly files: readonly string[]
  readonly remediation: string
}

export interface TriageResult {
  readonly summary: string
  readonly findings: readonly TriageFinding[]
}

export interface PullRequest {
  readonly html_url: string
  readonly number: number
}

export type AgentQuestionResult =
  | { readonly status: "done"; readonly content: string }
  | { readonly status: "needs_input"; readonly question: string; readonly context?: string }

export interface OperatorQuestionRecord {
  readonly phase: string
  readonly questionNumber: number
  readonly question: string
  readonly context?: string
  readonly answer: string
  readonly answeredBy: string
  readonly at: string
}

const payloadFields = new Set([
  "featureDesign",
  "prBaseBranch",
  "prTitle",
  "prBody",
  "maxReviewRounds",
  "operatorInput",
  "operatorInputTimeout",
  "maxOperatorQuestionsPerPhase",
  "claudeModel",
  "codexModel",
  "cursorModel",
])

export function normalizePayload(payload: Payload): Input {
  assertKnownPayloadFields(payload)
  if (!payload.featureDesign) {
    throw new Error("payload.featureDesign is required")
  }
  const featureDesign = formatFeatureDesign(payload.featureDesign)
  const title = firstLine(featureDesign)

  return {
    featureDesign,
    prBaseBranch: payload.prBaseBranch?.trim() || undefined,
    prTitle: payload.prTitle?.trim() || title,
    prBody: payload.prBody?.trim() || [
      "Created by the Helmr implementation workflow.",
      "",
      "Feature design:",
      "",
      featureDesign,
    ].join("\n"),
    maxReviewRounds: clampInteger(payload.maxReviewRounds ?? 100, 1, 100, "payload.maxReviewRounds"),
    operatorInput: payload.operatorInput ?? true,
    operatorInputTimeout: clampInteger(payload.operatorInputTimeout ?? 3600, 1, 86400),
    maxOperatorQuestionsPerPhase: clampInteger(payload.maxOperatorQuestionsPerPhase ?? 3, 0, 10),
    claudeModel: payload.claudeModel?.trim() || DEFAULT_CLAUDE_MODEL,
    codexModel: payload.codexModel?.trim() || DEFAULT_CODEX_MODEL,
    cursorModel: payload.cursorModel?.trim() || DEFAULT_CURSOR_MODEL,
  }
}

function assertKnownPayloadFields(payload: Payload): void {
  if (payload === null || typeof payload !== "object" || Array.isArray(payload)) {
    throw new Error("payload must be an object")
  }
  for (const field of Object.keys(payload as Record<string, unknown>)) {
    if (!payloadFields.has(field)) {
      throw new Error(`payload.${field} is not supported`)
    }
  }
}

export function requireGitHubSource(ctx: TaskContext): GitHubTaskSource {
  if (ctx.source.kind !== "github") {
    throw new Error("implement workflow requires a GitHub run source")
  }
  return ctx.source
}

function formatFeatureDesign(value: FeatureDesign): string {
  if (typeof value === "string") {
    const trimmed = value.trim()
    if (!trimmed) throw new Error("payload.featureDesign must not be empty")
    return trimmed
  }
  return JSON.stringify(value, null, 2)
}

function firstLine(value: string): string {
  return value.split("\n").map((line) => line.trim()).find(Boolean) ?? "Implement feature"
}

function clampInteger(value: number, min: number, max: number, name = "integer payload value"): number {
  if (!Number.isInteger(value)) {
    throw new Error(`${name} must be an integer`)
  }
  return Math.min(Math.max(value, min), max)
}
