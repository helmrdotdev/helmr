import { DEFAULT_CLAUDE_MODEL, DEFAULT_CODEX_MODEL, DEFAULT_CURSOR_MODEL } from "../models"
import { z } from "zod"

export type FeatureDesign = string | Record<string, unknown>

export interface Payload {
  readonly repository?: string
  readonly ref?: string
  readonly subpath?: string
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
  readonly repository: string
  readonly ref: string
  readonly subpath?: string
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

export interface GitRepositoryTarget {
  readonly repository: string
  readonly requestedRef: string
  readonly resolvedSha: string
  readonly refKind: "branch" | "sha" | "unknown"
  readonly refName?: string
  readonly subpath?: string
}

export interface ReviewRound {
  readonly round: number
  readonly codexReview: string
  readonly claudeCodeReview: string
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
  readonly at: string
}

const featureDesignSchema = z.union([z.string(), z.record(z.string(), z.unknown())])

export const payload = z.object({
  repository: z.string(),
  ref: z.string().optional(),
  subpath: z.string().optional(),
  featureDesign: featureDesignSchema,
  prBaseBranch: z.string().optional(),
  prTitle: z.string().optional(),
  prBody: z.string().optional(),
  maxReviewRounds: z.number().int().optional(),
  operatorInput: z.boolean().optional(),
  operatorInputTimeout: z.number().int().optional(),
  maxOperatorQuestionsPerPhase: z.number().int().optional(),
  claudeModel: z.string().optional(),
  codexModel: z.string().optional(),
  cursorModel: z.string().optional(),
}).strict()

export const lightPayload = z.object({
  repository: z.string(),
  ref: z.string().optional(),
  subpath: z.string().optional(),
  featureDesign: featureDesignSchema,
  prBaseBranch: z.string().optional(),
  prTitle: z.string().optional(),
  prBody: z.string().optional(),
  maxReviewRounds: z.number().int().optional(),
  claudeModel: z.string().optional(),
  codexModel: z.string().optional(),
  cursorModel: z.string().optional(),
}).strict()

export function normalizePayload(payload: Payload): Input {
  if (!payload.featureDesign) {
    throw new Error("payload.featureDesign is required")
  }
  const featureDesign = formatFeatureDesign(payload.featureDesign)
  const title = firstLine(featureDesign)

  return {
    repository: normalizeRepository(payload.repository),
    ref: normalizeRef(payload.ref),
    subpath: normalizeSubpath(payload.subpath),
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

function formatFeatureDesign(value: FeatureDesign): string {
  if (typeof value === "string") {
    const trimmed = value.trim()
    if (!trimmed) throw new Error("payload.featureDesign must not be empty")
    return trimmed
  }
  return JSON.stringify(value, null, 2)
}

function normalizeRepository(value: string | undefined): string {
  const trimmed = value?.trim()
  if (!trimmed) {
    throw new Error("payload.repository is required")
  }
  if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(trimmed)) {
    throw new Error(`payload.repository must be owner/name, received: ${trimmed}`)
  }
  return trimmed
}

function normalizeRef(value: string | undefined): string {
  const trimmed = value?.trim() || "main"
  if (
    !trimmed ||
    trimmed.startsWith("-") ||
    /[\s\0-\x1f\x7f]/.test(trimmed) ||
    trimmed.includes("..") ||
    trimmed.includes("@{") ||
    trimmed.endsWith(".lock")
  ) {
    throw new Error(`payload.ref is invalid: ${value}`)
  }
  return trimmed
}

function normalizeSubpath(value: string | undefined): string | undefined {
  const trimmed = value?.trim().replace(/^\/+|\/+$/g, "")
  if (!trimmed) return undefined
  const parts = trimmed.split("/")
  if (parts.some((part) => part === "" || part === "." || part === "..")) {
    throw new Error(`payload.subpath is invalid: ${value}`)
  }
  return parts.join("/")
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
