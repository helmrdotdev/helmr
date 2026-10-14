export type { StandardSchemaV1 } from "@standard-schema/spec"
import type { StandardSchemaV1 } from "@standard-schema/spec"
import { assertUnicodeString } from "./internal/utf8"
import { slackStartOption } from "./client-slack"
import type { AnswerFor, AskAnswer, Question } from "./question"
import type { JsonValue } from "./contract"
export type { TurnOutcome } from "./contract"
import type { HumanContent, InputContent } from "./content"
import type { ImageBuilder } from "./image"

export type Json = JsonValue
export type { Duration, Retention } from "./duration"
import type { Duration, Retention } from "./duration"
export type MaybePromise<T> = T | Promise<T>

export interface ComputerIdentity { readonly id: string }
export interface BuildContext {
  readonly signal: AbortSignal
  exec(command: string | readonly string[], options?: { cwd?: string; env?: Record<string, string> }): Promise<void>
}
export type SecretBinding = { readonly secret: string } & (
  | { readonly env: { readonly name: string; readonly mode: "protected"; readonly allowedOrigins: readonly string[] }; readonly file?: never }
  | { readonly env: { readonly name: string; readonly mode: "raw"; readonly allowedOrigins?: never }; readonly file?: never }
  | { readonly file: { readonly path: string }; readonly env?: never }
)
export interface TemplateDefinition {
  readonly kind: "template"
  readonly id: string
  readonly image: ImageBuilder
  readonly resources: { readonly cpu: number; readonly memory: string; readonly disk?: string }
  readonly prepare?: (build: BuildContext) => MaybePromise<void>
  readonly deleteAfterUnused?: Retention
  readonly refresh?: { readonly every: Duration; readonly maxAge?: Duration }
  readonly secrets?: readonly SecretBinding[]
  readonly buildSecrets?: readonly SecretBinding[]
}
export function template(config: Omit<TemplateDefinition, "kind">): TemplateDefinition {
  if (!config.id.trim()) throw new TypeError("Computer id is required")
  if (!(config.resources.cpu > 0) || !config.resources.memory) throw new TypeError("Computer resources are required")
  return Object.freeze({ ...config, kind: "template" as const })
}
export interface OutputReceipt { readonly sequence: number }
export interface SlackInputSource {
  readonly teamId: string
  readonly channelId: string
  readonly threadTs: string
  readonly messageTs: string
  readonly userId: string
}
export type InputSource =
  | { readonly kind: "user"; readonly slack?: SlackInputSource; readonly requesterSessionId?: never; readonly originTurnId?: never }
  | { readonly kind: "api_key"; readonly slack?: never; readonly requesterSessionId?: never; readonly originTurnId?: never }
  | { readonly kind: "schedule"; readonly slack?: never; readonly requesterSessionId?: never; readonly originTurnId?: never }
  | { readonly kind: "session"; readonly requesterSessionId: string; readonly originTurnId?: string; readonly slack?: never }
export type MessageSource = Exclude<InputSource, { readonly kind: "schedule" }>
export interface Message {
  readonly id: string
  readonly content: InputContent
  readonly source: MessageSource
}
export interface Turn<I = InputContent> {
  readonly id: string
  readonly sequence: number
  readonly createdAt: string
  readonly input: I
  readonly source: InputSource
  readonly signal: AbortSignal
  // Runs once on cancellation or handler failure, concurrently with handler
  // drainage. The runtime joins asynchronous cleanup before settling the Turn.
  onCancel(handler: (reason: unknown) => MaybePromise<void>): void
  readonly output: {
    write(value: HumanContent): Promise<OutputReceipt>
    pipe(values: AsyncIterable<HumanContent>, options?: { signal?: AbortSignal }): Promise<void>
  }
  respond(value: HumanContent): Promise<void>
  onMessage(handler: (message: Message) => MaybePromise<void>): Promise<void>
  ask<const Q extends Question>(question: Q, options?: { signal?: AbortSignal }): Promise<AskAnswer<AnswerFor<Q>>>
}
export interface RuntimeMcpConnectionOptions {
  readonly url: string
  readonly headers: Readonly<{ Authorization: string }>
}

export interface InitContext {
  readonly mcp: Readonly<{ getConnectionOptions(): Promise<RuntimeMcpConnectionOptions> }>
  readonly session: Readonly<{ id: string; key?: string; parent?: Readonly<{ id: string }> }>
  readonly computer: ComputerIdentity
  readonly deployment: Readonly<{ id: string }>
  readonly recovery: Readonly<{ kind: "initial" | "reconstructed"; reason?: string }>
  readonly signal: AbortSignal
}
export interface AgentContext<S> extends InitContext { readonly initResult: S }
export interface CronTrigger<I = InputContent> {
  readonly slack?: Readonly<{ channelId: string }>
  readonly id: string
  readonly cron: string
  readonly timezone: string
  readonly input: I
}
export const triggers = Object.freeze({
  cron<const I>(id: string, expression: string, options: { readonly timezone: string; readonly input: I; readonly slack?: Readonly<{ channelId: string }> }): CronTrigger<I> {
    if (!id.trim() || !expression.trim() || typeof options.timezone !== "string" || !options.timezone.trim()) throw new TypeError("Cron id, expression and IANA timezone are required")
    requireJson(options.input)
    return Object.freeze({ id, cron: expression, timezone: options.timezone, input: options.input, ...(options.slack === undefined ? {} : { slack: Object.freeze({ channelId: slackStartOption(options.slack).channel_id }) }) })
  },
})
type Same<A, B> = (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2) ? true : false
export type AgentDefinition<I = InputContent, O = Json, S = undefined, ID extends string = string, Wire = I, Returned = O> = JsonCheck<O> & AgentContract<ID, Wire, O> & {
  readonly kind: "agent"
  readonly id: ID
  readonly template: TemplateDefinition
  readonly turn: (turn: Turn<I>, ctx: AgentContext<S>) => MaybePromise<Returned>
  readonly closeAfterIdle?: Retention
  readonly maxTurnDuration?: Duration
  readonly triggers?: readonly CronTrigger<Wire>[]
} & ([S] extends [undefined] ? { readonly init?: (ctx: InitContext) => MaybePromise<S> } : { readonly init: (ctx: InitContext) => MaybePromise<S> })
  & (Same<I, InputContent> extends true ? Same<Wire, InputContent> extends true ? { readonly inputSchema?: StandardSchemaV1<Wire, I> } : { readonly inputSchema: StandardSchemaV1<Wire, I> } : { readonly inputSchema: StandardSchemaV1<Wire, I> })
  & (Same<O, Returned> extends true ? { readonly resultSchema?: StandardSchemaV1<Returned, O> } : { readonly resultSchema: StandardSchemaV1<Returned, O> })


/** Type-only call contract: importing an agent's type does not load its resources. */
declare const agentContract: unique symbol
export interface AgentContract<ID extends string = string, Input = unknown, Output = unknown> {
  readonly id: ID
  readonly [agentContract]?: Readonly<{ input: Input; result: Output }>
}
export type AgentInput<A extends AgentContract> = NonNullable<A[typeof agentContract]>["input"]
export type AgentResult<A extends AgentContract> = NonNullable<A[typeof agentContract]>["result"]

type Schema = StandardSchemaV1 | undefined
type Parsed<S> = S extends StandardSchemaV1 ? StandardSchemaV1.InferOutput<S> : InputContent
type Accepted<S> = S extends StandardSchemaV1 ? StandardSchemaV1.InferInput<S> : InputContent
type Returned<S, O> = S extends StandardSchemaV1 ? StandardSchemaV1.InferInput<S> : O
type Result<S, O> = S extends StandardSchemaV1 ? StandardSchemaV1.InferOutput<S> : O
// Reject known non-JSON outer values without rewriting interface or nested types.
// The runtime validates the complete finite JSON value before admission/settlement.
type NonJsonObject = Function | Date | RegExp | PromiseLike<unknown> | ReadonlyMap<unknown, unknown> | ReadonlySet<unknown> | ArrayBuffer | ArrayBufferView
type JsonCheck<T> = 0 extends (1 & T) ? unknown : [T] extends [string | number | boolean | null | object] ? [Extract<T, NonJsonObject>] extends [never] ? unknown : never : never
type SchemaField<K extends string, S> = [S] extends [undefined] ? { readonly [P in K]?: never } : { readonly [P in K]: S }
type AgentConfig<ID extends string, SI extends Schema, SO extends Schema, State, O> = {
  readonly id: ID
  readonly template: TemplateDefinition
  readonly turn: (turn: Turn<NoInfer<Parsed<SI>>>, ctx: AgentContext<NoInfer<State>>) => MaybePromise<Returned<SO, O>>
  readonly closeAfterIdle?: Retention
  readonly maxTurnDuration?: Duration
  readonly triggers?: readonly CronTrigger<NoInfer<Accepted<SI>>>[]
} & SchemaField<"inputSchema", SI> & SchemaField<"resultSchema", SO>
export function agent<const ID extends string, SI extends Schema = undefined, SO extends Schema = undefined, State = undefined, O = Json>(config: AgentConfig<ID, SI, SO, State, O> & { readonly init: (ctx: InitContext) => MaybePromise<State> } & JsonCheck<Result<NoInfer<SO>, NoInfer<O>>>): AgentDefinition<Parsed<SI>, Result<SO, O>, State, ID, Accepted<SI>, Returned<SO, O>>
export function agent<const ID extends string, SI extends Schema = undefined, SO extends Schema = undefined, O = Json>(config: AgentConfig<ID, SI, SO, undefined, O> & { readonly init?: never } & JsonCheck<Result<NoInfer<SO>, NoInfer<O>>>): AgentDefinition<Parsed<SI>, Result<SO, O>, undefined, ID, Accepted<SI>, Returned<SO, O>>
export function agent(config: { readonly id: string; readonly template: TemplateDefinition; readonly turn: unknown; readonly inputSchema?: StandardSchemaV1; readonly resultSchema?: StandardSchemaV1 }): unknown {
  if (!config.id.trim() || config.template.kind !== "template" || typeof config.turn !== "function") throw new TypeError("Agent id, Template and turn handler are required")
  for (const schema of [config.inputSchema, config.resultSchema]) {
    if (schema !== undefined && (schema["~standard"]?.version !== 1 || typeof schema["~standard"].validate !== "function")) throw new TypeError("A Standard Schema v1 validator is required")
  }
  return Object.freeze({ ...config, kind: "agent" as const })
}

export function requireJson(value: unknown): asserts value is Json {
  const ancestors = new Set<object>()
  function visit(current: unknown): void {
    if (current === null || typeof current === "boolean") return
    if (typeof current === "string") { assertUnicodeString(current); return }
    if (typeof current === "number" && Number.isFinite(current)) return
    if (typeof current !== "object" || !current) throw new TypeError("A finite JSON value is required")
    if (ancestors.has(current)) throw new TypeError("JSON cannot contain a cycle")
    const array = Array.isArray(current)
    if (!array && Object.getPrototypeOf(current) !== Object.prototype && Object.getPrototypeOf(current) !== null) throw new TypeError("JSON objects must have ordinary properties")
    const keys = Reflect.ownKeys(current)
    if (array && keys.length !== current.length + 1) throw new TypeError("JSON arrays must be dense with no extra properties")
    ancestors.add(current)
    for (const key of keys) {
      if (array && key === "length") continue
      if (typeof key !== "string") throw new TypeError("JSON keys must be strings")
      assertUnicodeString(key)
      const descriptor = Object.getOwnPropertyDescriptor(current, key)!
      if (!descriptor.enumerable || !("value" in descriptor)) throw new TypeError("JSON requires ordinary data properties")
      if (array && (!/^(0|[1-9][0-9]*)$/.test(key) || Number(key) >= current.length)) throw new TypeError("JSON arrays cannot have extra properties")
      visit(descriptor.value)
    }
    ancestors.delete(current)
  }
  visit(value)
}
