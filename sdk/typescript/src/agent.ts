import { slackStartOption } from "./client-slack"
import type { AnswerControl, AnswerFor, AskResponse, Question } from "./question"
import type { JsonValue } from "./contract"
export type { TurnOutcome } from "./contract"
import { normalizeInput } from "./content"
export { MessageRejected } from "./message-error"
import type { HumanContent, InputContent } from "./content"
import type { ImageBuilder } from "./image"

export type Json = JsonValue
export type Duration = string | number
export type MaybePromise<T> = T | Promise<T>

export interface ComputerRef { readonly id: string }
export interface BuildContext {
  readonly signal: AbortSignal
  exec(command: string | readonly string[], options?: { cwd?: string; env?: Record<string, string> }): Promise<void>
}
export type SecretBinding = { readonly secretId: string } & (
  | { readonly env: { readonly name: string; readonly mode: "protected"; readonly allowedOrigins: readonly string[] }; readonly file?: never }
  | { readonly env: { readonly name: string; readonly mode: "raw" }; readonly file?: never }
  | { readonly file: { readonly path: string }; readonly env?: never }
)
export interface ComputerDefinition {
  readonly kind: "computer"
  readonly id: string
  readonly image: ImageBuilder
  readonly resources: { readonly cpu: number; readonly memory: string; readonly disk?: string }
  readonly prepare?: (build: BuildContext) => MaybePromise<void>
  readonly refresh?: { readonly every: Duration; readonly maxAge?: Duration }
  readonly secrets?: readonly SecretBinding[]
  readonly buildSecrets?: readonly SecretBinding[]
}
export function computer(config: Omit<ComputerDefinition, "kind">): ComputerDefinition {
  if (!config.id.trim()) throw new TypeError("Computer id is required")
  if (!(config.resources.cpu > 0) || !config.resources.memory) throw new TypeError("Computer resources are required")
  return Object.freeze({ ...config, kind: "computer" as const })
}
export interface OutputReceipt { readonly sequence: number }
export interface Turn<I extends InputContent = InputContent> {
  readonly id: string
  readonly sequence: number
  readonly createdAt: string
  readonly input: I
  readonly source: Readonly<{ kind: string; requesterSessionId?: string; originTurnId?: string }>
  readonly signal: AbortSignal
  readonly output: {
    write(value: HumanContent): Promise<OutputReceipt>
    pipe(values: AsyncIterable<HumanContent>, options?: { signal?: AbortSignal }): Promise<void>
  }
  respond(value: HumanContent): Promise<void>
  onMessage(handler: (message: InputContent) => MaybePromise<void>): Promise<void>
  ask<C extends AnswerControl>(question: Question<C>, options?: { signal?: AbortSignal }): Promise<AskResponse<AnswerFor<C>>>
}
export interface SetupContext {
  readonly session: Readonly<{ id: string; key?: string; parent?: Readonly<{ id: string }> }>
  readonly computer: ComputerRef
  readonly deployment: Readonly<{ id: string }>
  readonly recovery: Readonly<{ kind: "initial" | "reconstructed"; reason?: string }>
  readonly signal: AbortSignal
}
export interface AgentContext<S> extends SetupContext { readonly setupResult: S }
export interface CronTrigger<I extends InputContent = InputContent> {
  readonly slack?: Readonly<{ channelId: string }>
  readonly id: string
  readonly cron: string
  readonly timezone: string
  readonly input: I
}
export const triggers = Object.freeze({
  cron<I extends InputContent>(id: string, expression: string, options: { readonly timezone: string; readonly input: I; readonly slack?: Readonly<{ channelId: string }> }): CronTrigger<I> {
    if (!id.trim() || !expression.trim() || typeof options.timezone !== "string" || !options.timezone.trim()) throw new TypeError("Cron id, expression and IANA timezone are required")
    normalizeInput(options.input)
    return Object.freeze({ id, cron: expression, timezone: options.timezone, input: options.input, ...(options.slack === undefined ? {} : { slack: Object.freeze({ channelId: slackStartOption(options.slack).channel_id }) }) })
  },
})
export interface AgentDefinition<I extends InputContent = InputContent, O extends Json = Json, S = unknown> {
  readonly kind: "agent"
  readonly id: string
  readonly computer: ComputerDefinition
  readonly setup?: (ctx: SetupContext) => MaybePromise<S>
  readonly turn: (turn: Turn<I>, ctx: AgentContext<S>) => MaybePromise<O>
  readonly closeAfterIdle?: Duration
  readonly maxTurnDuration?: Duration
  readonly triggers?: readonly CronTrigger<I>[]
}
export function agent<I extends InputContent = InputContent, O extends Json = Json, S = undefined>(config: Omit<AgentDefinition<I, O, S>, "kind">): AgentDefinition<I, O, S> {
  if (!config.id.trim() || config.computer.kind !== "computer" || typeof config.turn !== "function") throw new TypeError("Agent id, Computer and turn handler are required")
  return Object.freeze({ ...config, kind: "agent" as const })
}

export function requireJson(value: unknown): asserts value is Json {
  const ancestors = new Set<object>()
  function visit(current: unknown): void {
    if (current === null || typeof current === "string" || typeof current === "boolean") return
    if (typeof current === "number" && Number.isFinite(current)) return
    if (typeof current !== "object" || !current) throw new TypeError("A JSON value is required")
    if (ancestors.has(current)) throw new TypeError("JSON cannot contain a cycle")
    if (!Array.isArray(current) && Object.getPrototypeOf(current) !== Object.prototype && Object.getPrototypeOf(current) !== null) throw new TypeError("JSON objects must have ordinary properties")
    ancestors.add(current)
    for (const item of Array.isArray(current) ? current : Object.values(current)) visit(item)
    ancestors.delete(current)
  }
  visit(value)
}
